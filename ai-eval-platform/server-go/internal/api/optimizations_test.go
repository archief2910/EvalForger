package api

import (
	"testing"
)

// optimizationEnv builds a project with a dataset (and its snapshot) and a
// queue, which is everything the create endpoint needs.
func optimizationEnv(t *testing.T) (*testEnv, string, string) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	e.attachQueue()
	pid := e.project(t, "", "optimizer")

	status, ds := e.json(t, "POST", "/api/v1/projects/"+pid+"/datasets", "",
		map[string]interface{}{"name": "opt ds"})
	if status != 200 {
		t.Fatalf("create dataset: %d %v", status, ds)
	}
	datasetID, _ := ds["id"].(string)
	status, _ = e.json(t, "POST", "/api/v1/datasets/"+datasetID+"/records", "",
		map[string]interface{}{"input": "How do I refund?", "expected": "Within 30 days."})
	if status != 200 {
		t.Fatalf("add record: %d", status)
	}
	return e, pid, datasetID
}

func TestOptimizationCreateEnqueuesJob(t *testing.T) {
	e, pid, datasetID := optimizationEnv(t)

	status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/optimizations", "",
		map[string]interface{}{
			"name": "tighten the prompt", "dataset_id": datasetID,
			"scorers": []string{"exact_match"}, "max_candidates": 2,
		})
	if status != 200 {
		t.Fatalf("create optimization: %d %v", status, out)
	}
	if out["status"] != "queued" {
		t.Fatalf("optimization should start queued: %v", out)
	}
	if out["dataset_version_id"] == "" || out["dataset_version_id"] == nil {
		t.Fatalf("optimization must pin a dataset snapshot: %v", out)
	}

	var kind string
	var n int
	if err := e.s.DB.QueryRow(`SELECT kind FROM jobs WHERE experiment_id=?`, out["id"]).Scan(&kind); err != nil {
		t.Fatalf("job not enqueued: %v", err)
	}
	if kind != "optimize" {
		t.Fatalf("job kind = %q, want optimize", kind)
	}
	if err := e.s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE experiment_id=?`, out["id"]).Scan(&n); err != nil || n != 1 {
		t.Fatalf("expected exactly one job, got %d (%v)", n, err)
	}
}

func TestOptimizationCreateValidation(t *testing.T) {
	e, pid, datasetID := optimizationEnv(t)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"no dataset", map[string]interface{}{"scorers": []string{"exact_match"}}},
		{"unknown scorer", map[string]interface{}{"dataset_id": datasetID, "scorers": []string{"nope"}}},
		{"too many candidates", map[string]interface{}{"dataset_id": datasetID,
			"scorers": []string{"exact_match"}, "max_candidates": 99}},
		{"unknown dataset", map[string]interface{}{"dataset_id": "missing", "scorers": []string{"exact_match"}}},
	}
	for _, tc := range cases {
		status, out := e.json(t, "POST", "/api/v1/projects/"+pid+"/optimizations", "", tc.body)
		if status != 400 {
			t.Fatalf("%s: expected 400, got %d %v", tc.name, status, out)
		}
	}
}

func TestOptimizationPromotesOnlyAnImprovement(t *testing.T) {
	e, pid, datasetID := optimizationEnv(t)

	status, created := e.json(t, "POST", "/api/v1/projects/"+pid+"/optimizations", "",
		map[string]interface{}{"name": "run one", "dataset_id": datasetID, "scorers": []string{"exact_match"}})
	if status != 200 {
		t.Fatalf("create: %d %v", status, created)
	}
	id, _ := created["id"].(string)

	// The worker reports a better candidate.
	status, out := e.json(t, "POST", "/api/v1/worker_callback/optimization/"+id, "",
		map[string]interface{}{
			"baseline": map[string]interface{}{
				"prompt": "baseline", "mean": 0.4, "scores": map[string]float64{"exact_match": 0.4},
			},
			"candidates": []map[string]interface{}{
				{"name": "concise", "prompt": "baseline\n\nAnswer concisely.",
					"mean": 0.8, "scores": map[string]float64{"exact_match": 0.8}},
			},
			"best": map[string]interface{}{
				"name": "concise", "prompt": "baseline\n\nAnswer concisely.",
				"mean": 0.8, "scores": map[string]float64{"exact_match": 0.8},
			},
		})
	if status != 200 {
		t.Fatalf("callback: %d %v", status, out)
	}
	if out["improved"] != true || out["promoted"] != true {
		t.Fatalf("a better candidate should be promoted: %v", out)
	}

	status, detail := e.json(t, "GET", "/api/v1/projects/"+pid+"/optimizations/"+id, "", nil)
	if status != 200 {
		t.Fatalf("detail: %d %v", status, detail)
	}
	if detail["status"] != "completed" {
		t.Fatalf("optimization should be completed: %v", detail)
	}
	if detail["best_score"] != 0.8 || detail["baseline_score"] != 0.4 {
		t.Fatalf("scores not recorded: %v", detail)
	}
	bestID, _ := detail["best_prompt_version_id"].(string)
	if bestID == "" {
		t.Fatalf("the winner should be saved as a prompt version: %v", detail)
	}

	// The new prompt version is readable through the normal prompts API.
	status, prompt := e.json(t, "GET", "/api/v1/prompts/"+bestID, "", nil)
	if status != 200 {
		t.Fatalf("read winning prompt: %d %v", status, prompt)
	}
	if got, _ := prompt["template"].(string); !contains(got, "Answer concisely") {
		t.Fatalf("winning prompt body missing: %v", prompt)
	}
}

func TestOptimizationDiscardsAWorseCandidate(t *testing.T) {
	e, pid, datasetID := optimizationEnv(t)

	status, created := e.json(t, "POST", "/api/v1/projects/"+pid+"/optimizations", "",
		map[string]interface{}{"name": "run two", "dataset_id": datasetID, "scorers": []string{"exact_match"}})
	if status != 200 {
		t.Fatalf("create: %d %v", status, created)
	}
	id, _ := created["id"].(string)

	status, out := e.json(t, "POST", "/api/v1/worker_callback/optimization/"+id, "",
		map[string]interface{}{
			"baseline": map[string]interface{}{"prompt": "baseline", "mean": 0.9},
			"candidates": []map[string]interface{}{
				{"name": "step_by_step", "prompt": "baseline\n\nThink step by step.", "mean": 0.5},
			},
			"best": map[string]interface{}{"name": "baseline", "prompt": "baseline", "mean": 0.9},
		})
	if status != 200 {
		t.Fatalf("callback: %d %v", status, out)
	}
	if out["improved"] != false || out["promoted"] != false {
		t.Fatalf("a worse candidate must not be promoted: %v", out)
	}

	status, detail := e.json(t, "GET", "/api/v1/projects/"+pid+"/optimizations/"+id, "", nil)
	if status != 200 {
		t.Fatalf("detail: %d", status)
	}
	if detail["best_prompt_version_id"] != "" {
		t.Fatalf("no prompt should be created when nothing improved: %v", detail)
	}

	// The prompt list must not gain an "optimized:" entry.
	_, raw, _ := e.do(t, "GET", "/api/v1/projects/"+pid+"/prompts", "", nil, nil)
	if contains(string(raw), "optimized:") {
		t.Fatalf("a discarded candidate created a prompt: %s", raw)
	}
}

func TestOptimizationFailureIsRecorded(t *testing.T) {
	e, pid, datasetID := optimizationEnv(t)

	status, created := e.json(t, "POST", "/api/v1/projects/"+pid+"/optimizations", "",
		map[string]interface{}{"name": "run three", "dataset_id": datasetID, "scorers": []string{"exact_match"}})
	if status != 200 {
		t.Fatalf("create: %d %v", status, created)
	}
	id, _ := created["id"].(string)

	status, _ = e.json(t, "POST", "/api/v1/worker_callback/optimization/"+id, "",
		map[string]interface{}{"error": "provider unavailable"})
	if status != 200 {
		t.Fatalf("callback: %d", status)
	}

	status, detail := e.json(t, "GET", "/api/v1/projects/"+pid+"/optimizations/"+id, "", nil)
	if status != 200 {
		t.Fatalf("detail: %d", status)
	}
	if detail["status"] != "failed" {
		t.Fatalf("a reported error should fail the run: %v", detail)
	}
	if detail["error"] != "provider unavailable" {
		t.Fatalf("the reason should be kept: %v", detail)
	}
}

func TestOptimizationList(t *testing.T) {
	e, pid, datasetID := optimizationEnv(t)
	for _, name := range []string{"a", "b"} {
		status, _ := e.json(t, "POST", "/api/v1/projects/"+pid+"/optimizations", "",
			map[string]interface{}{"name": name, "dataset_id": datasetID, "scorers": []string{"exact_match"}})
		if status != 200 {
			t.Fatalf("create %s: %d", name, status)
		}
	}
	status, list, _ := e.do(t, "GET", "/api/v1/projects/"+pid+"/optimizations", "", nil, nil)
	if status != 200 {
		t.Fatalf("list: %d", status)
	}
	if !contains(string(list), `"a"`) || !contains(string(list), `"b"`) {
		t.Fatalf("list is missing a run: %s", list)
	}
}
