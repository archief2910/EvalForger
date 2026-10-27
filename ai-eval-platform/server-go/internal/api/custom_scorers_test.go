package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"evalforge/internal/sandbox"
)

// fakeSandbox is the isolation backend used by these tests: the API and
// engine behaviour is what they verify, not container isolation (sandbox
// tests cover the runner).
type fakeSandbox struct {
	mu       sync.Mutex
	code     []string
	payloads []map[string]interface{}
	result   sandbox.Result
	err      error
	up       bool
}

func (f *fakeSandbox) Run(_ context.Context, code string, payload map[string]interface{}) (sandbox.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = append(f.code, code)
	f.payloads = append(f.payloads, payload)
	return f.result, f.err
}

func (f *fakeSandbox) Available() bool  { return f.up }
func (f *fakeSandbox) Describe() string { return "fake" }

func (f *fakeSandbox) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.code)
}

func newSandboxEnv(t *testing.T) (*testEnv, *fakeSandbox, string) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	fake := &fakeSandbox{up: true}
	e.s.sandbox = fake
	// Use the real backend over the fake runner, so availability and payload
	// construction are exercised exactly as in production.
	e.s.online.SetCustomScorer(&customMetricBackend{s: e.s, run: fake})
	pid := e.project(t, "", "custom-metrics")
	return e, fake, pid
}

func registerMetric(t *testing.T, e *testEnv, pid, name, code string) map[string]interface{} {
	t.Helper()
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/custom-scorers", "",
		map[string]interface{}{"name": name, "description": "test metric", "code": code})
	if status != 200 {
		t.Fatalf("register metric: %d %v", status, out)
	}
	return out
}

func TestCustomScorerRegistry(t *testing.T) {
	e, _, pid := newSandboxEnv(t)

	metric := registerMetric(t, e, pid, "tone_check", "def score(payload):\n    return 0.9\n")
	if metric["name"] != "tone_check" {
		t.Fatalf("name not echoed: %v", metric)
	}
	if metric["kind"] != "python_metric" {
		t.Fatalf("kind should mark it as user code: %v", metric)
	}

	// The code is returned to the owner so it can be edited, and the registry
	// marks it custom so a reader can tell it apart from a builtin.
	status, out := e.json(t, "GET", "/api/v1/projects/"+pid+"/custom-scorers", "", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, out)
	}
	list, _ := out["scorers"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("expected one metric, got %v", out["scorers"])
	}

	// Deleting removes it.
	status, _ = e.json(t, "DELETE", "/api/v1/projects/"+pid+"/custom-scorers/tone_check", "", nil)
	if status != 200 {
		t.Fatalf("delete: %d", status)
	}
	status, _ = e.json(t, "GET", "/api/v1/projects/"+pid+"/custom-scorers/tone_check", "", nil)
	if status != 404 {
		t.Fatalf("deleted metric should be 404, got %d", status)
	}
}

func TestCustomScorerValidation(t *testing.T) {
	e, _, pid := newSandboxEnv(t)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"bad name", map[string]interface{}{"name": "Not A Name", "code": "def score(p): return 1"}},
		{"empty code", map[string]interface{}{"name": "ok_name", "code": "  "}},
		{"no score function", map[string]interface{}{"name": "ok_name", "code": "x = 1"}},
	}
	for _, tc := range cases {
		status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/custom-scorers", "", tc.body)
		if status != 400 {
			t.Fatalf("%s: expected 400, got %d %v", tc.name, status, out)
		}
	}

	// Duplicate names are a conflict, not a server error.
	registerMetric(t, e, pid, "dup_name", "def score(p): return 1")
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/custom-scorers", "",
		map[string]interface{}{"name": "dup_name", "code": "def score(p): return 1"})
	if status != 409 {
		t.Fatalf("duplicate should be 409, got %d %v", status, out)
	}
}

func TestCustomScorerAppearsInRegistry(t *testing.T) {
	e, _, pid := newSandboxEnv(t)
	registerMetric(t, e, pid, "tone_check", "def score(p): return 0.9")

	status, out := e.json(t, "GET", "/api/v1/scorers?project_id="+pid, "", nil)
	if status != 200 {
		t.Fatalf("scorers: %d %v", status, out)
	}
	// The response is a JSON array, which the test helper decodes into a map;
	// use the raw body instead.
	_, raw, _ := e.do(t, "GET", "/api/v1/scorers?project_id="+pid, "", nil, nil)
	if !contains(string(raw), "tone_check") {
		t.Fatalf("custom metric missing from the registry: %s", raw)
	}
	if !contains(string(raw), `"custom":true`) {
		t.Fatalf("custom metric should be marked custom: %s", raw)
	}
}

func TestCustomScorerTestEndpointFailsClosed(t *testing.T) {
	e, fake, pid := newSandboxEnv(t)
	registerMetric(t, e, pid, "tone_check", "def score(p): return 0.9")

	// The sandbox is down: the endpoint must refuse, not run the code.
	fake.up = false
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/custom-scorers/tone_check/test", "", nil)
	if status != 503 {
		t.Fatalf("expected 503 when the sandbox is down, got %d %v", status, out)
	}
	if fake.runs() != 0 {
		t.Fatalf("code must not run without a sandbox, got %d runs", fake.runs())
	}

	// With a sandbox it runs and returns the verdict.
	fake.up = true
	score := 0.75
	fake.result = sandbox.Result{Score: &score, Rationale: "looks fine"}
	status, out = e.json(t, "POST", "/api/v1/projects/"+pid+"/custom-scorers/tone_check/test", "", nil)
	if status != 200 {
		t.Fatalf("test run: %d %v", status, out)
	}
	if out["score"] != 0.75 {
		t.Fatalf("score not returned: %v", out)
	}
	if fake.runs() != 1 {
		t.Fatalf("expected one run, got %d", fake.runs())
	}
}

func TestOnlineRuleRunsCustomMetric(t *testing.T) {
	e, sandboxFake, pid := newSandboxEnv(t)
	registerMetric(t, e, pid, "tone_check", "def score(payload):\n    return 0.9\n")

	// A rule may name a custom metric; it is validated against the project's
	// registry, not the builtin one. Sampling is off so the ingest trigger
	// does not race the manual run this test is about.
	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "tone watch", "scorers": []string{"tone_check"}, "sample_rate": 0,
	})
	if status != 200 {
		t.Fatalf("rule with a custom metric should be accepted: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)

	// A trace in the project.
	ingestWithStatus(t, e, pid, "t1", "ok", time.Minute)

	score := 0.9
	sandboxFake.result = sandbox.Result{Score: &score, Rationale: "polite"}
	status, run := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules/"+id+"/run", "",
		map[string]interface{}{"trace_id": "t1"})
	if status != 200 {
		t.Fatalf("run: %d %v", status, run)
	}
	if sandboxFake.runs() != 1 {
		t.Fatalf("expected the metric to run once, got %d", sandboxFake.runs())
	}

	// The payload handed to the metric is the trace root, not the whole row.
	payload := sandboxFake.payloads[0]
	if payload["trace_id"] != "t1" {
		t.Fatalf("metric payload should name the trace: %v", payload)
	}
	if payload["output"] != "a" {
		t.Fatalf("metric payload should carry the root output, got %v", payload["output"])
	}

	// The score is stored alongside the trace.
	var n int
	if err := e.s.Telemetry.QueryRow(
		`SELECT COUNT(*) FROM online_scores WHERE trace_id=? AND scorer=?`, "t1", "tone_check").Scan(&n); err != nil {
		t.Fatalf("count scores: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected one stored custom score, got %d", n)
	}
}

func TestOnlineRuleRejectsUnknownCustomMetric(t *testing.T) {
	e, _, pid := newSandboxEnv(t)

	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "tone watch", "scorers": []string{"not_a_metric"},
	})
	if status != 400 {
		t.Fatalf("an unknown metric must be rejected, got %d %v", status, out)
	}
}

func TestCustomMetricFailsClosedWithoutSandbox(t *testing.T) {
	e, sandboxFake, pid := newSandboxEnv(t)
	registerMetric(t, e, pid, "tone_check", "def score(p): return 0.9")
	sandboxFake.up = false

	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules", "", map[string]interface{}{
		"name": "tone watch", "scorers": []string{"tone_check"}, "sample_rate": 1,
	})
	if status != 200 {
		t.Fatalf("create rule: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)
	ingestWithStatus(t, e, pid, "t1", "ok", time.Minute)

	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/online-rules/"+id+"/run", "",
		map[string]interface{}{"trace_id": "t1"})
	// The run fails rather than silently producing zero scores.
	if status == 200 {
		t.Fatalf("a custom metric must not run without a sandbox: %v", out)
	}
	if sandboxFake.runs() != 0 {
		t.Fatalf("code must not run without a sandbox, got %d runs", sandboxFake.runs())
	}
}
