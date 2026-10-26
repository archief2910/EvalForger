package api

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"evalforge/internal/online"
)

// fakeScorer stands in for the Python worker: online matching and persistence
// are what these tests verify, not scorer correctness (worker tests cover that).
type fakeScorer struct {
	mu    sync.Mutex
	score float64
	calls []online.ScoreRequest
	err   error
}

func (f *fakeScorer) ScoreTrace(_ context.Context, req online.ScoreRequest) ([]online.Score, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	score, err := f.score, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]online.Score, 0, len(req.Scorers))
	for _, name := range req.Scorers {
		v := score
		out = append(out, online.Score{Scorer: name, Score: &v, Rationale: "fake", JudgeMode: "fake"})
	}
	return out, nil
}

func (f *fakeScorer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newOnlineEnv(t *testing.T) (*testEnv, *fakeScorer, string) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	fake := &fakeScorer{score: 0.2}
	e.s.SetOnlineScorer(fake)
	_, project := e.json(t, "POST", "/api/v1/projects", "", map[string]interface{}{"name": "online"})
	pid, _ := project["id"].(string)
	if pid == "" {
		t.Fatalf("project create: %v", project)
	}
	return e, fake, pid
}

func ingestTrace(t *testing.T, e *testEnv, pid, traceID, output string, errored bool) {
	t.Helper()
	event := map[string]interface{}{
		"id": traceID, "name": "support-agent", "type": "task",
		"input": "customer question", "output": output,
	}
	if errored {
		event["error"] = map[string]interface{}{"message": "boom"}
	}
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []interface{}{event}})
	if status != 200 {
		t.Fatalf("ingest: %d %v", status, out)
	}
}

func createRule(t *testing.T, e *testEnv, pid string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", body)
	if status != 200 {
		t.Fatalf("create rule: %d %v", status, out)
	}
	return out
}

func TestOnlineRuleScoresLiveTraceAndRaisesAlert(t *testing.T) {
	e, fake, pid := newOnlineEnv(t)

	rule := createRule(t, e, pid, map[string]interface{}{
		"name": "hallucination watch", "scorers": []string{"exact_match"},
		"sample_rate": 1, "alert_threshold": 0.5,
	})
	if rule["enabled"] != true {
		t.Fatalf("rule should default to enabled: %v", rule)
	}

	ingestTrace(t, e, pid, "trace-1", "the answer", false)
	e.s.online.Wait()

	var scored int
	if err := e.s.Telemetry.QueryRow(`SELECT COUNT(*) FROM online_scores WHERE trace_id=?`, "trace-1").Scan(&scored); err != nil {
		t.Fatalf("count scores: %v", err)
	}
	if scored != 1 {
		t.Fatalf("expected one online score, got %d", scored)
	}
	if fake.callCount() != 1 {
		t.Fatalf("scorer called %d times, want 1", fake.callCount())
	}

	var alerts int
	if err := e.s.DB.QueryRow(`SELECT COUNT(*) FROM online_alerts WHERE trace_id=? AND status='open'`, "trace-1").Scan(&alerts); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if alerts != 1 {
		t.Fatalf("expected one open alert below threshold, got %d", alerts)
	}

	// The trace view must show the automatic score with its rule name.
	status, detailRaw, _ := e.do(t, "GET", "/api/v1/projects/"+pid+"/traces/trace-1", "", nil, nil)
	if status != 200 {
		t.Fatalf("trace detail: %d %s", status, detailRaw)
	}
	var detail map[string]interface{}
	_ = json.Unmarshal(detailRaw, &detail)
	scores, _ := detail["online_scores"].([]interface{})
	if len(scores) != 1 {
		t.Fatalf("trace detail online_scores = %v", detail["online_scores"])
	}
	first, _ := scores[0].(map[string]interface{})
	if first["rule_name"] != "hallucination watch" || first["scorer"] != "exact_match" {
		t.Fatalf("unexpected online score payload: %v", first)
	}

	// Alerts endpoint + acknowledgement.
	status, alertRaw, _ := e.do(t, "GET", "/api/v1/projects/"+pid+"/alerts", "", nil, nil)
	if status != 200 {
		t.Fatalf("alerts: %d %s", status, alertRaw)
	}
	var alertList struct {
		Alerts []map[string]interface{} `json:"alerts"`
	}
	_ = json.Unmarshal(alertRaw, &alertList)
	if len(alertList.Alerts) != 1 {
		t.Fatalf("alerts payload: %s", alertRaw)
	}
	alertID, _ := alertList.Alerts[0]["id"].(string)
	if st, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/alerts/"+alertID+"/ack", "", nil); st != 200 || out["status"] != "acknowledged" {
		t.Fatalf("ack: %d %v", st, out)
	}
	if err := e.s.DB.QueryRow(`SELECT COUNT(*) FROM online_alerts WHERE status='open'`).Scan(&alerts); err != nil || alerts != 0 {
		t.Fatalf("open alerts after ack = %d (%v)", alerts, err)
	}

	// A second ingest of the same trace must not double-score it.
	ingestTrace(t, e, pid, "trace-1", "the answer", false)
	e.s.online.Wait()
	if fake.callCount() != 1 {
		t.Fatalf("retried ingest re-scored the trace: %d calls", fake.callCount())
	}
}

func TestOnlineRuleSamplingAndFilters(t *testing.T) {
	e, fake, pid := newOnlineEnv(t)

	createRule(t, e, pid, map[string]interface{}{
		"name": "never", "scorers": []string{"exact_match"}, "sample_rate": 0,
	})
	createRule(t, e, pid, map[string]interface{}{
		"name": "errors only", "scorers": []string{"exact_match"}, "filter_status": "error",
	})
	ingestTrace(t, e, pid, "ok-trace", "fine", false)
	e.s.online.Wait()
	if fake.callCount() != 0 {
		t.Fatalf("sampled-out and filtered traces were scored: %d calls", fake.callCount())
	}

	// An error trace matches the status filter, so exactly one rule fires.
	ingestTrace(t, e, pid, "error-trace", "bad", true)
	e.s.online.Wait()
	if fake.callCount() != 1 {
		t.Fatalf("error trace scored %d times, want 1", fake.callCount())
	}

	// Manual run ignores sampling/filters and returns scores to the caller.
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules/"+ruleIDFor(t, e, pid, "never")+"/run", "",
		map[string]interface{}{"trace_id": "ok-trace"})
	if status != 200 {
		t.Fatalf("manual run: %d %v", status, out)
	}
	scores, _ := out["scores"].([]interface{})
	if len(scores) != 1 {
		t.Fatalf("manual run scores: %v", out)
	}
}

func ruleIDFor(t *testing.T, e *testEnv, pid, name string) string {
	t.Helper()
	status, raw, _ := e.do(t, "GET", "/api/v1/projects/"+pid+"/online-rules", "", nil, nil)
	if status != 200 {
		t.Fatalf("list rules: %d %s", status, raw)
	}
	var rules []map[string]interface{}
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("decode rules: %v", err)
	}
	for _, r := range rules {
		if r["name"] == name {
			return r["id"].(string)
		}
	}
	t.Fatalf("rule %q not found in %s", name, raw)
	return ""
}

func TestOnlineRuleValidationAndPatch(t *testing.T) {
	e, _, pid := newOnlineEnv(t)

	if status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "",
		map[string]interface{}{"name": "bad", "scorers": []string{"does_not_exist"}}); status != 400 {
		t.Fatalf("unknown scorer accepted: %d %v", status, out)
	}
	if status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "",
		map[string]interface{}{"name": "bad", "scorers": []string{"thread_completion"}}); status != 400 {
		t.Fatalf("thread scorer accepted for trace rules: %d %v", status, out)
	}
	if status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "",
		map[string]interface{}{"name": "bad", "scorers": []string{"exact_match"}, "sample_rate": 3}); status != 400 {
		t.Fatalf("out-of-range sample accepted: %d %v", status, out)
	}

	rule := createRule(t, e, pid, map[string]interface{}{
		"name": "keep me", "scorers": []string{"exact_match"}, "alert_threshold": 0.7,
	})
	id, _ := rule["id"].(string)

	// PATCH with only `enabled` must not wipe the scorers or the threshold.
	status, out := e.json(t, "PATCH", "/api/v1/projects/"+pid+"/online-rules/"+id, "",
		map[string]interface{}{"enabled": false})
	if status != 200 {
		t.Fatalf("patch: %d %v", status, out)
	}
	if out["enabled"] != false {
		t.Fatalf("patch did not disable: %v", out)
	}
	if out["alert_threshold"] != 0.7 {
		t.Fatalf("patch lost the threshold: %v", out)
	}
	if scorers, _ := out["scorers"].([]interface{}); len(scorers) != 1 {
		t.Fatalf("patch lost the scorers: %v", out)
	}

	// Disabled rules do not score new traffic.
	ingestTrace(t, e, pid, "after-disable", "x", false)
	e.s.online.Wait()
	var n int
	if err := e.s.Telemetry.QueryRow(`SELECT COUNT(*) FROM online_scores WHERE trace_id=?`, "after-disable").Scan(&n); err != nil || n != 0 {
		t.Fatalf("disabled rule scored: n=%d err=%v", n, err)
	}
}
