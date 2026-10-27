package api

import (
	"testing"
)

func createGuardrail(t *testing.T, e *testEnv, pid string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/guardrail-policies", "", body)
	if status != 200 {
		t.Fatalf("create guardrail: %d %v", status, out)
	}
	return out
}

func TestGuardrailPolicyCRUD(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "guardrails")

	policy := createGuardrail(t, e, pid, map[string]interface{}{
		"name": "no pii", "mode": "redact",
		"checks": []map[string]interface{}{{"type": "PII"}},
	})
	id, _ := policy["id"].(string)
	if id == "" {
		t.Fatalf("policy id missing: %v", policy)
	}
	if policy["mode"] != "redact" || policy["enabled"] != true {
		t.Fatalf("unexpected policy: %v", policy)
	}

	status, list, _ := e.do(t, "GET", "/api/v1/projects/"+pid+"/guardrail-policies", "", nil, nil)
	if status != 200 || !contains(string(list), "no pii") {
		t.Fatalf("list: %d %s", status, list)
	}

	status, out := e.json(t, "PATCH", "/api/v1/projects/"+pid+"/guardrail-policies/"+id, "",
		map[string]interface{}{"mode": "block"})
	if status != 200 || out["mode"] != "block" {
		t.Fatalf("patch: %d %v", status, out)
	}

	status, _ = e.json(t, "DELETE", "/api/v1/projects/"+pid+"/guardrail-policies/"+id, "", nil)
	if status != 200 {
		t.Fatalf("delete: %d", status)
	}
	status, _ = e.json(t, "GET", "/api/v1/projects/"+pid+"/guardrail-policies/"+id, "", nil)
	if status != 404 {
		t.Fatalf("deleted policy should be 404, got %d", status)
	}
}

func TestGuardrailPolicyValidation(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "guardrail-validation")

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"no name", map[string]interface{}{"checks": []map[string]interface{}{{"type": "PII"}}}},
		{"no checks", map[string]interface{}{"name": "x"}},
		{"bad mode", map[string]interface{}{"name": "x", "mode": "ignore",
			"checks": []map[string]interface{}{{"type": "PII"}}}},
		{"bad type", map[string]interface{}{"name": "x",
			"checks": []map[string]interface{}{{"type": "SENTIMENT"}}}},
		{"topic without topics", map[string]interface{}{"name": "x",
			"checks": []map[string]interface{}{{"type": "TOPIC"}}}},
		{"custom without rules", map[string]interface{}{"name": "x",
			"checks": []map[string]interface{}{{"type": "CUSTOM"}}}},
		{"custom bad regex", map[string]interface{}{"name": "x",
			"checks": []map[string]interface{}{{"type": "CUSTOM", "config": map[string]interface{}{"regexes": []string{"(unclosed"}}}}}},
	}
	for _, tc := range cases {
		status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/guardrail-policies", "", tc.body)
		if status != 400 {
			t.Fatalf("%s: expected 400, got %d %v", tc.name, status, out)
		}
	}
}

func TestGuardrailCheckAllowFlagRedactBlock(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "guardrail-check")

	// No policies: everything is allowed.
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/guardrails/check", "",
		map[string]interface{}{"text": "email me at a@b.co"})
	if status != 200 || out["allowed"] != true || out["action"] != "allow" {
		t.Fatalf("no policy should allow: %d %v", status, out)
	}

	createGuardrail(t, e, pid, map[string]interface{}{
		"name": "pii redact", "mode": "redact",
		"checks": []map[string]interface{}{{"type": "PII"}},
	})
	status, out = e.json(t, "POST", "/api/v1/projects/"+pid+"/guardrails/check", "",
		map[string]interface{}{"text": "email me at a@b.co"})
	if status != 200 {
		t.Fatalf("check: %d %v", status, out)
	}
	if out["allowed"] != true || out["action"] != "redact" {
		t.Fatalf("a redact policy should allow with masking: %v", out)
	}
	if out["redacted"] == "email me at a@b.co" {
		t.Fatalf("redacted text should be masked: %v", out["redacted"])
	}

	// A block policy overrides the redact policy: strongest action wins.
	createGuardrail(t, e, pid, map[string]interface{}{
		"name": "pii block", "mode": "block",
		"checks": []map[string]interface{}{{"type": "PII", "config": map[string]interface{}{"entities": []string{"EMAIL_ADDRESS"}}}},
	})
	status, out = e.json(t, "POST", "/api/v1/projects/"+pid+"/guardrails/check", "",
		map[string]interface{}{"text": "email me at a@b.co"})
	if status != 200 {
		t.Fatalf("check: %d %v", status, out)
	}
	if out["allowed"] != false || out["action"] != "block" {
		t.Fatalf("the strictest policy must win: %v", out)
	}
}

func TestIngestGuardrailRedactsAndBlocks(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "guardrail-ingest")

	createGuardrail(t, e, pid, map[string]interface{}{
		"name": "redact pii", "mode": "redact",
		"checks": []map[string]interface{}{{"type": "PII"}},
	})

	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []interface{}{map[string]interface{}{
			"id": "t-redact", "name": "agent", "type": "task",
			"input": "my email is jane@example.com", "output": "noted",
		}}})
	if status != 200 {
		t.Fatalf("ingest with redact: %d %v", status, out)
	}

	var stored string
	if err := e.s.Telemetry.QueryRow(
		`SELECT input FROM spans WHERE project_id=? AND trace_id=?`, pid, "t-redact").Scan(&stored); err != nil {
		t.Fatalf("load span: %v", err)
	}
	if contains(stored, "jane@example.com") {
		t.Fatalf("redacted ingest must not store the email, got %q", stored)
	}
	if !contains(stored, "EMAIL_ADDRESS") {
		t.Fatalf("redacted ingest should store the mask, got %q", stored)
	}

	// A block policy refuses the batch and stores nothing.
	createGuardrail(t, e, pid, map[string]interface{}{
		"name": "block pii", "mode": "block",
		"checks": []map[string]interface{}{{"type": "PII"}},
	})
	status, out = e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []interface{}{map[string]interface{}{
			"id": "t-blocked", "name": "agent", "type": "task",
			"input": "card 4111 1111 1111 1111", "output": "ok",
		}}})
	if status != 422 {
		t.Fatalf("a block policy should refuse the batch, got %d %v", status, out)
	}
	var n int
	if err := e.s.Telemetry.QueryRow(
		`SELECT COUNT(*) FROM spans WHERE project_id=? AND trace_id=?`, pid, "t-blocked").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a blocked batch must store nothing, got %d rows", n)
	}
}

func TestGuardrailEventsAreRecordedWithoutTheSecret(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "guardrail-events")

	createGuardrail(t, e, pid, map[string]interface{}{
		"name": "flag pii", "mode": "flag",
		"checks": []map[string]interface{}{{"type": "PII"}},
	})
	status, _ := e.json(t, "POST", "/api/v1/projects/"+pid+"/guardrails/check", "",
		map[string]interface{}{"text": "reach me at leak@example.com", "trace_id": "t1"})
	if status != 200 {
		t.Fatalf("check: %d", status)
	}

	status, out := e.json(t, "GET", "/api/v1/projects/"+pid+"/guardrail-events", "", nil)
	if status != 200 {
		t.Fatalf("events: %d %v", status, out)
	}
	events, _ := out["events"].([]interface{})
	if len(events) == 0 {
		t.Fatalf("expected a recorded event: %v", out)
	}
	first, _ := events[0].(map[string]interface{})
	if first["check_type"] != "PII" || first["action"] != "flag" {
		t.Fatalf("unexpected event: %v", first)
	}
	// The preview must be masked: the log cannot become the place the secret is
	// kept.
	if preview, _ := first["preview"].(string); contains(preview, "leak@example.com") {
		t.Fatalf("event preview leaked the email: %q", preview)
	}
}

func TestGuardrailDashboardBlock(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "guardrail-dashboard")

	createGuardrail(t, e, pid, map[string]interface{}{
		"name": "pii", "mode": "flag",
		"checks": []map[string]interface{}{{"type": "PII"}},
	})
	status, dash := e.json(t, "GET", "/api/v1/projects/"+pid+"/dashboard", "", nil)
	if status != 200 {
		t.Fatalf("dashboard: %d %v", status, dash)
	}
	block, _ := dash["guardrails"].(map[string]interface{})
	if block == nil {
		t.Fatalf("dashboard has no guardrails block: %v", dash)
	}
	if block["policies"] != float64(1) || block["enabled"] != float64(1) {
		t.Fatalf("dashboard should count the policy: %v", block)
	}
}
