package api

// Conversation (thread) coverage: turns belong to a thread, thread scores are
// separate from per-trace scores, lifecycle is open/closed, and thread
// evaluations flow through the durable queue and the worker callback.

import (
	"fmt"
	"testing"
	"time"
)

func threadFromList(t *testing.T, list map[string]interface{}, id string) map[string]interface{} {
	t.Helper()
	rows, _ := list["threads"].([]interface{})
	for _, raw := range rows {
		row, _ := raw.(map[string]interface{})
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("thread %q not found in %v", id, list)
	return nil
}

func TestThreadLifecycleAndScores(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	e.attachQueue()

	_, project := e.json(t, "POST", "/api/v1/projects", "", map[string]interface{}{"name": "threads"})
	pid := project["id"].(string)
	now := time.Now().UnixMilli()

	// Two turns of one conversation. Each turn is its own trace, so the caller
	// passes the shared thread_id; the thread row must count both turns.
	for i, q := range []string{"first question", "follow-up question"} {
		payload := map[string]interface{}{
			"thread_id": "conv-test",
			"events": []map[string]interface{}{{
				"id": fmt.Sprintf("turn-%d", i), "name": "agent", "type": "task",
				"input": q, "output": "answer " + q,
				"start_ms": now + int64(i)*60_000, "end_ms": now + int64(i)*60_000 + 500,
			}},
		}
		status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "", payload)
		if status != 200 {
			t.Fatalf("ingest turn %d: %d %v", i, status, out)
		}
	}

	// A later batch for an existing turn may omit thread_id: the trace keeps
	// the conversation it was already assigned to.
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []map[string]interface{}{{
			"id": "turn-1-child", "trace_id": "turn-1", "parent_id": "turn-1",
			"name": "retrieve", "type": "tool", "input": "docs",
			"start_ms": now + 61_000, "end_ms": now + 61_100,
		}}})
	if status != 200 {
		t.Fatalf("ingest same-trace child: %d %v", status, out)
	}
	_, detail := e.json(t, "GET", "/api/v1/projects/"+pid+"/traces/turn-1", "", nil)
	if got, _ := detail["thread_id"].(string); got != "conv-test" {
		t.Fatalf("same-trace child thread_id = %q, want conv-test", got)
	}

	status, list := e.json(t, "GET", "/api/v1/projects/"+pid+"/threads", "", nil)
	if status != 200 {
		t.Fatalf("thread list: %d %v", status, list)
	}
	thread := threadFromList(t, list, "conv-test")
	if turns, _ := thread["turn_count"].(float64); turns != 2 {
		t.Fatalf("turn_count = %v, want 2", thread["turn_count"])
	}

	status, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/conv-test", "", nil)
	if status != 200 {
		t.Fatalf("thread detail: %d %v", status, detail)
	}
	if turns, _ := detail["turns"].([]interface{}); len(turns) != 2 {
		t.Fatalf("detail turns = %d, want 2", len(turns))
	}

	// A manual conversation score is visible in the summary immediately.
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-test/scores", "",
		map[string]interface{}{"name": "thread_completion", "score": 0.75, "rationale": "manual check"})
	if status != 200 {
		t.Fatalf("save thread score: %d", status)
	}
	_, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/conv-test", "", nil)
	summary, _ := detail["score_summary"].(map[string]interface{})
	completion, _ := summary["thread_completion"].(map[string]interface{})
	if avg, _ := completion["avg"].(float64); avg != 0.75 {
		t.Fatalf("thread_completion avg = %v, want 0.75", completion["avg"])
	}
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-test/scores", "",
		map[string]interface{}{"name": "thread_completion", "score": 1.5})
	if status != 400 {
		t.Fatalf("out-of-range score status = %d, want 400", status)
	}

	// Manual lifecycle: close then reopen.
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-test/close", "", nil)
	if status != 200 {
		t.Fatalf("close thread: %d", status)
	}
	_, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/conv-test", "", nil)
	if got := detail["thread"].(map[string]interface{})["status"]; got != "closed" {
		t.Fatalf("status after close = %v", got)
	}
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-test/open", "", nil)
	if status != 200 {
		t.Fatalf("open thread: %d", status)
	}
	_, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/conv-test", "", nil)
	if got := detail["thread"].(map[string]interface{})["status"]; got != "open" {
		t.Fatalf("status after open = %v", got)
	}

	// Trace list can be filtered to one conversation.
	status, traces := e.json(t, "GET", "/api/v1/projects/"+pid+"/traces?thread_id=conv-test", "", nil)
	if status != 200 {
		t.Fatalf("filtered traces: %d %v", status, traces)
	}
	if total, _ := traces["total"].(float64); total != 2 {
		t.Fatalf("filtered traces total = %v, want 2", traces["total"])
	}

	// Thread evaluation is a durable queue job, not a synchronous call.
	status, job := e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-test/evaluate", "",
		map[string]interface{}{"scorers": []string{"thread_completion", "thread_error_free"}})
	if status != 200 {
		t.Fatalf("enqueue thread eval: %d %v", status, job)
	}
	jobID, _ := job["job_id"].(string)
	if jobID == "" {
		t.Fatalf("thread eval has no job id: %v", job)
	}

	// The worker callback writes scores under the job, and is idempotent.
	callback := map[string]interface{}{
		"scores": []map[string]interface{}{
			{"name": "thread_completion", "score": 1.0, "rationale": "both turns answered"},
			{"name": "thread_error_free", "score": 1.0, "rationale": "no errors"},
		},
		"status": "completed",
	}
	status, out = e.json(t, "POST", "/api/v1/worker_callback/thread/"+jobID, "", callback)
	if status != 200 {
		t.Fatalf("thread callback: %d %v", status, out)
	}
	_, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/conv-test", "", nil)
	var workerScores int
	for _, raw := range detail["scores"].([]interface{}) {
		row, _ := raw.(map[string]interface{})
		if row["job_id"] == jobID {
			workerScores++
		}
	}
	if workerScores != 2 {
		t.Fatalf("worker scores = %d, want 2", workerScores)
	}

	// A retried callback replaces this job's scores instead of appending.
	callback["scores"] = []map[string]interface{}{
		{"name": "thread_completion", "score": 0.5, "rationale": "retry"},
	}
	status, _ = e.json(t, "POST", "/api/v1/worker_callback/thread/"+jobID, "", callback)
	if status != 200 {
		t.Fatalf("second thread callback: %d", status)
	}
	_, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/conv-test", "", nil)
	workerScores = 0
	for _, raw := range detail["scores"].([]interface{}) {
		row, _ := raw.(map[string]interface{})
		if row["job_id"] == jobID {
			workerScores++
		}
	}
	if workerScores != 1 {
		t.Fatalf("worker scores after retry = %d, want 1", workerScores)
	}
}

func TestThreadFallbackAndIdleSweep(t *testing.T) {
	e := newTestEnvWithAuth(t, false)

	_, project := e.json(t, "POST", "/api/v1/projects", "", map[string]interface{}{"name": "fallback"})
	pid := project["id"].(string)
	old := time.Now().Add(-2 * time.Hour).UnixMilli()

	// No thread_id: the trace id becomes a one-turn conversation, so every
	// trace is addressable as a thread.
	status, _ := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []map[string]interface{}{{
			"id": "solo", "name": "agent", "type": "task", "input": "hello",
			"output": "hi", "start_ms": old, "end_ms": old + 100,
		}}})
	if status != 200 {
		t.Fatalf("ingest: %d", status)
	}
	_, detail := e.json(t, "GET", "/api/v1/projects/"+pid+"/traces/solo", "", nil)
	if got, _ := detail["thread_id"].(string); got != "solo" {
		t.Fatalf("trace thread_id = %q, want the trace id", got)
	}

	// The idle sweeper closes the old thread; a new turn reopens it.
	e.s.closeIdleThreads(30 * time.Minute)
	_, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/solo", "", nil)
	if got := detail["thread"].(map[string]interface{})["status"]; got != "closed" {
		t.Fatalf("idle thread status = %v, want closed", got)
	}
	now := time.Now().UnixMilli()
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"thread_id": "solo", "events": []map[string]interface{}{{
			"id": "solo-2", "name": "agent", "type": "task", "input": "again",
			"output": "welcome back", "start_ms": now, "end_ms": now + 100,
		}}})
	if status != 200 {
		t.Fatalf("reopen ingest: %d", status)
	}
	_, detail = e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/solo", "", nil)
	if got := detail["thread"].(map[string]interface{})["status"]; got != "open" {
		t.Fatalf("thread status after new activity = %v, want open", got)
	}
}
