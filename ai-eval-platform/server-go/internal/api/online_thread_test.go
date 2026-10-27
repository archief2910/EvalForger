package api

import (
	"testing"
	"time"
)

// ingestTurn adds one turn to a conversation.
func ingestTurn(t *testing.T, e *testEnv, pid, threadID, traceID, output string) {
	t.Helper()
	event := map[string]interface{}{
		"id": traceID, "name": "support-agent", "type": "task",
		"input": "customer question", "output": output, "thread_id": threadID,
	}
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []interface{}{event}})
	if status != 200 {
		t.Fatalf("ingest turn: %d %v", status, out)
	}
}

func TestOnlineThreadRuleValidatesScope(t *testing.T) {
	e, _, pid := newOnlineEnv(t)

	// A thread rule may name thread scorers.
	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "conversation quality", "scope": "thread",
		"scorers": []string{"thread_completion", "thread_coherence_judge"},
	})
	if status != 200 {
		t.Fatalf("thread rule should be accepted: %d %v", status, rule)
	}
	if rule["scope"] != "thread" {
		t.Fatalf("scope not echoed: %v", rule)
	}

	// A thread rule may not name a trace scorer.
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "mixed", "scope": "thread", "scorers": []string{"exact_match"},
	})
	if status != 400 {
		t.Fatalf("a thread rule must reject trace scorers, got %d %v", status, out)
	}

	// A trace rule may not name a thread scorer.
	status, out = e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "mixed", "scope": "trace", "scorers": []string{"thread_completion"},
	})
	if status != 400 {
		t.Fatalf("a trace rule must reject thread scorers, got %d %v", status, out)
	}

	// An unknown scope is rejected.
	status, out = e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "bad scope", "scope": "galaxy", "scorers": []string{"exact_match"},
	})
	if status != 400 {
		t.Fatalf("an unknown scope must be rejected, got %d %v", status, out)
	}
}

func TestOnlineThreadRuleScoresConversationOnClose(t *testing.T) {
	e, fake, pid := newOnlineEnv(t)

	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "conversation quality", "scope": "thread",
		"scorers": []string{"thread_completion"}, "alert_threshold": 0.5,
	})
	if status != 200 {
		t.Fatalf("create rule: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)
	if id == "" {
		t.Fatalf("rule id missing: %v", rule)
	}

	// Two turns in one conversation.
	ingestTurn(t, e, pid, "conv-1", "t1", "first answer")
	ingestTurn(t, e, pid, "conv-1", "t2", "second answer")

	// The conversation has not closed, so nothing has been scored yet.
	if fake.threadCallCount() != 0 {
		t.Fatalf("a conversation must not be scored before it closes, got %d calls", fake.threadCallCount())
	}

	// Closing the conversation is the trigger.
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-1/close", "", nil)
	if status != 200 {
		t.Fatalf("close thread: %d %v", status, out)
	}
	e.s.online.Wait()

	if fake.threadCallCount() != 1 {
		t.Fatalf("expected one conversation scoring run, got %d", fake.threadCallCount())
	}

	// The score is stored and visible on the conversation.
	status, detail := e.json(t, "GET", "/api/v1/projects/"+pid+"/threads/conv-1", "", nil)
	if status != 200 {
		t.Fatalf("thread detail: %d %v", status, detail)
	}
	scores, _ := detail["online_scores"].([]interface{})
	if len(scores) != 1 {
		t.Fatalf("expected one automatic conversation score, got %v", detail["online_scores"])
	}
	first, _ := scores[0].(map[string]interface{})
	if first["scorer"] != "thread_completion" {
		t.Fatalf("unexpected scorer: %v", first)
	}

	// The fake scores 0.2, which is below the threshold, so an alert fires —
	// and it points at the conversation, not a trace.
	status, alerts := e.json(t, "GET", "/api/v1/projects/"+pid+"/alerts", "", nil)
	if status != 200 {
		t.Fatalf("alerts: %d %v", status, alerts)
	}
	list, _ := alerts["alerts"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("expected one alert, got %v", alerts["alerts"])
	}
	alert, _ := list[0].(map[string]interface{})
	if alert["subject_type"] != "thread" {
		t.Fatalf("alert should name the conversation as its subject: %v", alert)
	}
	if alert["subject_id"] != "conv-1" {
		t.Fatalf("alert should point at the conversation: %v", alert)
	}
}

func TestOnlineThreadRuleIsIdempotent(t *testing.T) {
	e, fake, pid := newOnlineEnv(t)

	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "conversation quality", "scope": "thread",
		"scorers": []string{"thread_completion"},
	})
	if status != 200 {
		t.Fatalf("create rule: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)

	ingestTurn(t, e, pid, "conv-1", "t1", "answer")
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-1/close", "", nil)
	if status != 200 {
		t.Fatalf("close thread: %d", status)
	}
	e.s.online.Wait()
	if fake.threadCallCount() != 1 {
		t.Fatalf("expected one scoring run, got %d", fake.threadCallCount())
	}

	// Closing again must not score the same conversation twice.
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-1/close", "", nil)
	if status != 200 {
		t.Fatalf("second close: %d", status)
	}
	e.s.online.Wait()
	if fake.threadCallCount() != 1 {
		t.Fatalf("a conversation must not be scored twice, got %d runs", fake.threadCallCount())
	}

	// The stored score count agrees.
	var n int
	if err := e.s.Telemetry.QueryRow(
		`SELECT COUNT(*) FROM online_thread_scores WHERE rule_id=? AND thread_id=?`, id, "conv-1").Scan(&n); err != nil {
		t.Fatalf("count scores: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected one stored score, got %d", n)
	}
}

func TestOnlineThreadRuleManualRun(t *testing.T) {
	e, fake, pid := newOnlineEnv(t)

	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "conversation quality", "scope": "thread",
		"scorers": []string{"thread_completion"},
	})
	if status != 200 {
		t.Fatalf("create rule: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)

	ingestTurn(t, e, pid, "conv-1", "t1", "answer")

	// A thread rule run needs a thread id, not a trace id.
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules/"+id+"/run", "",
		map[string]interface{}{"trace_id": "t1"})
	if status != 400 {
		t.Fatalf("a thread rule must require a thread_id, got %d %v", status, out)
	}

	status, out = e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules/"+id+"/run", "",
		map[string]interface{}{"thread_id": "conv-1"})
	if status != 200 {
		t.Fatalf("manual run: %d %v", status, out)
	}
	if out["scope"] != "thread" {
		t.Fatalf("run should report the scope: %v", out)
	}
	scores, _ := out["scores"].([]interface{})
	if len(scores) != 1 {
		t.Fatalf("expected one score, got %v", out["scores"])
	}
	if fake.threadCallCount() != 1 {
		t.Fatalf("expected one scoring run, got %d", fake.threadCallCount())
	}
}

func TestOnlineThreadRuleIgnoresTraceRules(t *testing.T) {
	e, fake, pid := newOnlineEnv(t)

	// A trace-scoped rule must not be triggered by a conversation closing.
	status, _ := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "trace watch", "scope": "trace", "scorers": []string{"exact_match"},
	})
	if status != 200 {
		t.Fatalf("create rule: %d", status)
	}

	ingestTurn(t, e, pid, "conv-1", "t1", "answer")
	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/threads/conv-1/close", "", nil)
	if status != 200 {
		t.Fatalf("close thread: %d", status)
	}
	e.s.online.Wait()

	if fake.threadCallCount() != 0 {
		t.Fatalf("a trace rule must not score a conversation, got %d calls", fake.threadCallCount())
	}
}

func TestOnlineThreadRuleScoresOnIdleSweep(t *testing.T) {
	e, fake, pid := newOnlineEnv(t)

	status, _ := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "conversation quality", "scope": "thread",
		"scorers": []string{"thread_completion"},
	})
	if status != 200 {
		t.Fatalf("create rule: %d", status)
	}

	// A turn that is already older than the idle timeout, so the sweeper has
	// something to close.
	start := time.Now().Add(-time.Hour).UnixMilli()
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []interface{}{map[string]interface{}{
			"id": "t1", "name": "support-agent", "type": "task",
			"input": "customer question", "output": "answer", "thread_id": "conv-1",
			"start_ms": start, "end_ms": start + 100,
		}}})
	if status != 200 {
		t.Fatalf("ingest: %d %v", status, out)
	}

	// The idle sweeper closes conversations that went quiet, and that close is
	// the trigger for conversation scoring.
	e.s.closeIdleThreads(30 * time.Minute)
	e.s.online.Wait()

	if fake.threadCallCount() != 1 {
		t.Fatalf("the idle sweep should have scored the conversation, got %d calls", fake.threadCallCount())
	}
}
