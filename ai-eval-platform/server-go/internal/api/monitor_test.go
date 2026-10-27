package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"evalforge/internal/monitor"
)

// recordingNotifier captures monitor notifications instead of sending them.
type recordingNotifier struct {
	mu     sync.Mutex
	events []monitor.Event
	err    error
}

func (n *recordingNotifier) Notify(_ context.Context, _ monitor.Monitor, e monitor.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
	return n.err
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.events)
}

// newMonitorEnv creates a project and a recording notifier.
func newMonitorEnv(t *testing.T) (*testEnv, *recordingNotifier, string) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	notifier := &recordingNotifier{}
	e.s.monitors.SetNotifier(notifier)
	pid := e.project(t, "", "monitors")
	return e, notifier, pid
}

// newWebhookMonitorEnv creates a project whose notifications really go out over
// HTTP, so the delivery path itself is under test.
func newWebhookMonitorEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	e.s.monitors.SetNotifier(&monitor.WebhookNotifier{})
	pid := e.project(t, "", "monitors")
	return e, pid
}

// ingestWithStatus ingests one trace with an explicit status.
func ingestWithStatus(t *testing.T, e *testEnv, pid, traceID, status string, age time.Duration) {
	t.Helper()
	start := time.Now().Add(-age).UnixMilli()
	event := map[string]interface{}{
		"id": traceID, "name": "support-agent", "type": "task",
		"input": "q", "output": "a", "start_ms": start, "end_ms": start + 100,
	}
	if status == "error" {
		event["error"] = map[string]interface{}{"message": "boom"}
	}
	status_code, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/spans:ingest", "",
		map[string]interface{}{"events": []interface{}{event}})
	if status_code != 200 {
		t.Fatalf("ingest: %d %v", status_code, out)
	}
}

func createMonitor(t *testing.T, e *testEnv, pid string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors", "", body)
	if status != 200 {
		t.Fatalf("create monitor: %d %v", status, out)
	}
	return out
}

func TestMonitorCRUD(t *testing.T) {
	e, _, pid := newMonitorEnv(t)

	m := createMonitor(t, e, pid, map[string]interface{}{
		"name": "error rate watch", "metric": "error_rate",
		"operator": ">", "threshold": 0.05, "window_seconds": 900,
	})
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("monitor id missing: %v", m)
	}
	if m["state"] != "ok" {
		t.Fatalf("new monitor should start ok: %v", m)
	}
	if m["enabled"] != true {
		t.Fatalf("monitor should default to enabled: %v", m)
	}

	status, out := e.json(t, "GET", "/api/v1/projects/"+pid+"/monitors/"+id, "", nil)
	if status != 200 {
		t.Fatalf("get: %d %v", status, out)
	}
	if out["metric"] != "error_rate" {
		t.Fatalf("metric not echoed: %v", out)
	}

	status, patched := e.json(t, "PATCH", "/api/v1/projects/"+pid+"/monitors/"+id, "",
		map[string]interface{}{"threshold": 0.1})
	if status != 200 {
		t.Fatalf("patch: %d %v", status, patched)
	}
	if patched["threshold"] != float64(0.1) {
		t.Fatalf("patch did not apply: %v", patched)
	}
	if patched["name"] != "error rate watch" {
		t.Fatalf("patch must not clear other fields: %v", patched)
	}

	status, _ = e.json(t, "DELETE", "/api/v1/projects/"+pid+"/monitors/"+id, "", nil)
	if status != 200 {
		t.Fatalf("delete: %d", status)
	}
	status, _ = e.json(t, "GET", "/api/v1/projects/"+pid+"/monitors/"+id, "", nil)
	if status != 404 {
		t.Fatalf("deleted monitor should be 404, got %d", status)
	}
}

func TestMonitorValidation(t *testing.T) {
	e, _, pid := newMonitorEnv(t)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"no name", map[string]interface{}{"metric": "error_rate", "operator": ">", "threshold": 0.1}},
		{"unknown metric", map[string]interface{}{"name": "x", "metric": "nope", "operator": ">", "threshold": 0.1}},
		{"bad operator", map[string]interface{}{"name": "x", "metric": "error_rate", "operator": "~", "threshold": 0.1}},
		{"no threshold", map[string]interface{}{"name": "x", "metric": "error_rate", "operator": ">"}},
		{"tiny window", map[string]interface{}{"name": "x", "metric": "error_rate", "operator": ">", "threshold": 0.1, "window_seconds": 5}},
		{"bad channel", map[string]interface{}{"name": "x", "metric": "error_rate", "operator": ">", "threshold": 0.1, "channel_type": "carrier-pigeon"}},
		{"channel without url", map[string]interface{}{"name": "x", "metric": "error_rate", "operator": ">", "threshold": 0.1, "channel_type": "webhook"}},
		{"bad url", map[string]interface{}{"name": "x", "metric": "error_rate", "operator": ">", "threshold": 0.1, "channel_type": "webhook", "channel_url": "not-a-url"}},
	}
	for _, tc := range cases {
		status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors", "", tc.body)
		if status != 400 {
			t.Fatalf("%s: expected 400, got %d %v", tc.name, status, out)
		}
	}
}

func TestMonitorFiresOnceAndResolves(t *testing.T) {
	e, notifier, pid := newMonitorEnv(t)

	// Two of three traces are errored: 66% error rate.
	ingestWithStatus(t, e, pid, "t1", "error", time.Minute)
	ingestWithStatus(t, e, pid, "t2", "error", time.Minute)
	ingestWithStatus(t, e, pid, "t3", "ok", time.Minute)

	m := createMonitor(t, e, pid, map[string]interface{}{
		"name": "errors", "metric": "error_rate", "operator": ">", "threshold": 0.5,
		"window_seconds": 3600, "channel_type": "webhook", "channel_url": "https://example.invalid/hook",
	})
	id, _ := m["id"].(string)

	// First evaluation: the condition is true, so the monitor fires.
	status, ev := e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors/"+id+"/evaluate", "", nil)
	if status != 200 {
		t.Fatalf("evaluate: %d %v", status, ev)
	}
	if ev["firing"] != true {
		t.Fatalf("monitor should be firing: %v", ev)
	}
	if ev["changed"] != true {
		t.Fatalf("first evaluation should be a state change: %v", ev)
	}
	if notifier.count() != 1 {
		t.Fatalf("expected one notification, got %d", notifier.count())
	}

	// Second evaluation: still in breach, but no new event.
	status, ev = e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors/"+id+"/evaluate", "", nil)
	if status != 200 {
		t.Fatalf("second evaluate: %d %v", status, ev)
	}
	if ev["firing"] != true {
		t.Fatalf("monitor should still be firing: %v", ev)
	}
	if ev["changed"] != false {
		t.Fatalf("a monitor that stays in breach must not change state again: %v", ev)
	}
	if notifier.count() != 1 {
		t.Fatalf("expected exactly one notification, got %d", notifier.count())
	}

	// The event list holds one open event.
	status, events := e.json(t, "GET", "/api/v1/projects/"+pid+"/monitor-events", "", nil)
	if status != 200 {
		t.Fatalf("events: %d %v", status, events)
	}
	list, _ := events["events"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("expected one event, got %d", len(list))
	}
	first, _ := list[0].(map[string]interface{})
	if first["state"] != "firing" {
		t.Fatalf("event should be firing: %v", first)
	}
}

func TestMonitorResolvesWhenConditionClears(t *testing.T) {
	e, _, pid := newMonitorEnv(t)

	// All traces are fine at first.
	ingestWithStatus(t, e, pid, "t1", "ok", time.Minute)

	m := createMonitor(t, e, pid, map[string]interface{}{
		"name": "errors", "metric": "error_rate", "operator": ">", "threshold": 0.25,
		"window_seconds": 3600,
	})
	id, _ := m["id"].(string)

	status, ev := e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors/"+id+"/evaluate", "", nil)
	if status != 200 {
		t.Fatalf("evaluate: %d %v", status, ev)
	}
	if ev["firing"] != false {
		t.Fatalf("monitor should not be firing: %v", ev)
	}

	// Now an errored trace arrives; the monitor fires.
	ingestWithStatus(t, e, pid, "t2", "error", time.Minute)
	status, ev = e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors/"+id+"/evaluate", "", nil)
	if status != 200 {
		t.Fatalf("evaluate: %d %v", status, ev)
	}
	if ev["firing"] != true {
		t.Fatalf("monitor should fire: %v", ev)
	}

	// The error ages out of the window, so the condition clears and the event
	// resolves.
	e.s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	status, ev = e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors/"+id+"/evaluate", "", nil)
	if status != 200 {
		t.Fatalf("evaluate after window: %d %v", status, ev)
	}
	if ev["firing"] != false {
		t.Fatalf("monitor should have recovered: %v", ev)
	}
	if ev["changed"] != true {
		t.Fatalf("recovery should be a state change: %v", ev)
	}

	status, events := e.json(t, "GET", "/api/v1/projects/"+pid+"/monitor-events", "", nil)
	if status != 200 {
		t.Fatalf("events: %d %v", status, events)
	}
	list, _ := events["events"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("expected one event, got %d", len(list))
	}
	first, _ := list[0].(map[string]interface{})
	if first["state"] != "resolved" {
		t.Fatalf("event should be resolved: %v", first)
	}
	if first["resolved_at"] == nil {
		t.Fatalf("resolved event should carry resolved_at: %v", first)
	}
}

func TestMonitorWebhookDelivery(t *testing.T) {
	e, pid := newWebhookMonitorEnv(t)

	// A real HTTP endpoint stands in for the customer's Slack or PagerDuty.
	var got struct {
		mu   sync.Mutex
		body []byte
		hits int
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got.mu.Lock()
		got.body = buf[:n]
		got.hits++
		got.mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ingestWithStatus(t, e, pid, "t1", "error", time.Minute)

	m := createMonitor(t, e, pid, map[string]interface{}{
		"name": "errors", "metric": "error_rate", "operator": ">", "threshold": 0.1,
		"window_seconds": 3600, "channel_type": "webhook", "channel_url": srv.URL,
		"channel_secret": "s3cret",
	})
	id, _ := m["id"].(string)

	status, ev := e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors/"+id+"/evaluate", "", nil)
	if status != 200 {
		t.Fatalf("evaluate: %d %v", status, ev)
	}
	if ev["firing"] != true {
		t.Fatalf("monitor should fire: %v", ev)
	}

	got.mu.Lock()
	hits, body := got.hits, string(got.body)
	got.mu.Unlock()
	if hits != 1 {
		t.Fatalf("expected one webhook delivery, got %d", hits)
	}
	if !contains(body, "error_rate") {
		t.Fatalf("webhook body should name the metric: %s", body)
	}
	if !contains(body, "EvalForge") && !contains(body, "monitor") {
		t.Fatalf("webhook body should identify the monitor: %s", body)
	}
}

func TestMonitorDeliveryFailureIsRecorded(t *testing.T) {
	e, pid := newWebhookMonitorEnv(t)

	// A channel that refuses the connection: the event must still exist, and
	// the failure must be visible on it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	ingestWithStatus(t, e, pid, "t1", "error", time.Minute)
	m := createMonitor(t, e, pid, map[string]interface{}{
		"name": "errors", "metric": "error_rate", "operator": ">", "threshold": 0.1,
		"window_seconds": 3600, "channel_type": "webhook", "channel_url": srv.URL,
	})
	id, _ := m["id"].(string)

	status, ev := e.json(t, "POST", "/api/v1/projects/"+pid+"/monitors/"+id+"/evaluate", "", nil)
	if status != 200 {
		t.Fatalf("evaluate: %d %v", status, ev)
	}
	if ev["firing"] != true {
		t.Fatalf("monitor should fire even if delivery fails: %v", ev)
	}

	status, events := e.json(t, "GET", "/api/v1/projects/"+pid+"/monitor-events", "", nil)
	if status != 200 {
		t.Fatalf("events: %d %v", status, events)
	}
	list, _ := events["events"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("expected one event, got %d", len(list))
	}
	first, _ := list[0].(map[string]interface{})
	if first["notified"] != false {
		t.Fatalf("a failed delivery must not be marked notified: %v", first)
	}
	if first["notification_error"] == nil {
		t.Fatalf("a failed delivery must record the reason: %v", first)
	}
}

func TestMonitorDashboardStats(t *testing.T) {
	e, _, pid := newMonitorEnv(t)

	createMonitor(t, e, pid, map[string]interface{}{
		"name": "errors", "metric": "error_rate", "operator": ">", "threshold": 0.5,
		"window_seconds": 3600,
	})

	status, dash := e.json(t, "GET", "/api/v1/projects/"+pid+"/dashboard", "", nil)
	if status != 200 {
		t.Fatalf("dashboard: %d %v", status, dash)
	}
	stats, _ := dash["monitors"].(map[string]interface{})
	if stats == nil {
		t.Fatalf("dashboard has no monitors block: %v", dash)
	}
	if stats["monitors"] != float64(1) {
		t.Fatalf("dashboard should count the monitor: %v", stats)
	}
	if stats["enabled"] != float64(1) {
		t.Fatalf("dashboard should count the enabled monitor: %v", stats)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
