package api

import (
	"testing"
	"time"
)

// ingestOldTrace ingests a trace whose spans are dated in the past, so a
// retention policy has something to expire.
func ingestOldTrace(t *testing.T, e *testEnv, pid, traceID string, age time.Duration, threadID string) {
	t.Helper()
	start := time.Now().Add(-age).UnixMilli()
	event := map[string]interface{}{
		"id": traceID, "name": "support-agent", "type": "task",
		"input": "customer question", "output": "answer",
		"start_ms": start, "end_ms": start + 100,
	}
	if threadID != "" {
		event["thread_id"] = threadID
	}
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []interface{}{event}})
	if status != 200 {
		t.Fatalf("ingest: %d %v", status, out)
	}
}

func countRows(t *testing.T, e *testEnv, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := e.s.Telemetry.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestRetentionRuleCRUD(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "retention-crud")

	status, out := e.json(t, "GET", "/api/v1/projects/"+pid+"/retention-rules", "", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, out)
	}

	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/retention-rules", "",
		map[string]interface{}{"name": "30 days", "retention_days": 30})
	if status != 200 {
		t.Fatalf("create: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)
	if id == "" {
		t.Fatalf("rule id missing: %v", rule)
	}
	if rule["retention_days"] != float64(30) {
		t.Fatalf("retention_days not echoed: %v", rule)
	}
	if rule["enabled"] != true {
		t.Fatalf("rule should default to enabled: %v", rule)
	}
	if rule["apply_to_past"] != true {
		t.Fatalf("rule should default to apply_to_past: %v", rule)
	}

	// PATCH merges: only the fields that are sent change.
	status, patched := e.json(t, "PATCH", "/api/v1/projects/"+pid+"/retention-rules/"+id, "",
		map[string]interface{}{"retention_days": 7})
	if status != 200 {
		t.Fatalf("patch: %d %v", status, patched)
	}
	if patched["retention_days"] != float64(7) {
		t.Fatalf("patch did not apply: %v", patched)
	}
	if patched["name"] != "30 days" {
		t.Fatalf("patch must not clear other fields: %v", patched)
	}

	status, _ = e.json(t, "DELETE", "/api/v1/projects/"+pid+"/retention-rules/"+id, "", nil)
	if status != 200 {
		t.Fatalf("delete: %d", status)
	}
	status, _ = e.json(t, "GET", "/api/v1/projects/"+pid+"/retention-rules/"+id, "", nil)
	if status != 404 {
		t.Fatalf("deleted rule should be 404, got %d", status)
	}
}

func TestRetentionRuleValidation(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "retention-validation")

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing days", map[string]interface{}{"name": "x"}},
		{"zero days", map[string]interface{}{"retention_days": 0}},
		{"negative days", map[string]interface{}{"retention_days": -1}},
		{"too many days", map[string]interface{}{"retention_days": 4000}},
	}
	for _, tc := range cases {
		status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/retention-rules", "", tc.body)
		if status != 400 {
			t.Fatalf("%s: expected 400, got %d %v", tc.name, status, out)
		}
	}
}

func TestRetentionExpiresOldTelemetry(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "retention-apply")

	// One trace older than the policy, one inside it.
	ingestOldTrace(t, e, pid, "trace-old", 40*24*time.Hour, "thread-old")
	ingestOldTrace(t, e, pid, "trace-new", 2*time.Hour, "thread-new")

	before := countRows(t, e, `SELECT COUNT(*) FROM spans WHERE project_id=?`, pid)
	if before != 2 {
		t.Fatalf("expected 2 spans before the sweep, got %d", before)
	}

	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/retention-rules", "",
		map[string]interface{}{"name": "30 days", "retention_days": 30})
	if status != 200 {
		t.Fatalf("create rule: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)

	status, applied := e.json(t, "POST", "/api/v1/projects/"+pid+"/retention-rules/"+id+"/apply", "", nil)
	if status != 200 {
		t.Fatalf("apply: %d %v", status, applied)
	}
	result, _ := applied["applied"].(map[string]interface{})
	if result == nil {
		t.Fatalf("apply result missing: %v", applied)
	}
	if result["spans_deleted"] != float64(1) {
		t.Fatalf("expected 1 span deleted, got %v", result["spans_deleted"])
	}

	after := countRows(t, e, `SELECT COUNT(*) FROM spans WHERE project_id=?`, pid)
	if after != 1 {
		t.Fatalf("expected 1 span to survive, got %d", after)
	}
	remaining := countRows(t, e, `SELECT COUNT(*) FROM spans WHERE project_id=? AND trace_id=?`, pid, "trace-new")
	if remaining != 1 {
		t.Fatalf("the recent trace must survive, got %d spans", remaining)
	}

	// The conversation whose only turn expired goes with it.
	threads := countRows(t, e, `SELECT COUNT(*) FROM threads WHERE project_id=?`, pid)
	if threads != 1 {
		t.Fatalf("expected 1 surviving conversation, got %d", threads)
	}
}

func TestRetentionRespectsApplyToPast(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "retention-future-only")

	// A trace that predates the rule. A policy that does not apply to the
	// past must leave it alone.
	ingestOldTrace(t, e, pid, "trace-old", 40*24*time.Hour, "")

	status, rule := e.json(t, "POST", "/api/v1/projects/"+pid+"/retention-rules", "",
		map[string]interface{}{"name": "30 days", "retention_days": 30, "apply_to_past": false})
	if status != 200 {
		t.Fatalf("create rule: %d %v", status, rule)
	}
	id, _ := rule["id"].(string)

	status, _ = e.json(t, "POST", "/api/v1/projects/"+pid+"/retention-rules/"+id+"/apply", "", nil)
	if status != 200 {
		t.Fatalf("apply: %d", status)
	}

	after := countRows(t, e, `SELECT COUNT(*) FROM spans WHERE project_id=?`, pid)
	if after != 1 {
		t.Fatalf("apply_to_past=false must not delete pre-existing telemetry, got %d spans", after)
	}
}

func TestRetentionDashboardSummary(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	pid := e.project(t, "", "retention-dashboard")

	status, _ := e.json(t, "POST", "/api/v1/projects/"+pid+"/retention-rules", "",
		map[string]interface{}{"name": "90 days", "retention_days": 90})
	if status != 200 {
		t.Fatalf("create rule: %d", status)
	}

	status, dash := e.json(t, "GET", "/api/v1/projects/"+pid+"/dashboard", "", nil)
	if status != 200 {
		t.Fatalf("dashboard: %d %v", status, dash)
	}
	ret, _ := dash["retention"].(map[string]interface{})
	if ret == nil {
		t.Fatalf("dashboard has no retention block: %v", dash)
	}
	if ret["enabled"] != true {
		t.Fatalf("dashboard should report an enabled policy: %v", ret)
	}
	if ret["retention_days"] != float64(90) {
		t.Fatalf("dashboard should report the retention window: %v", ret)
	}
}
