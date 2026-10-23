package api

// Product-gap tests: an experiment must pin an immutable dataset snapshot and a
// prompt version, and reported token counts must say where they came from.

import (
	"context"
	"testing"
	"time"

	"evalforge/internal/queue"
)

func (e *testEnv) attachQueue() {
	e.s.AttachQueue(queue.New(
		e.s.DB, e.s.WriteMutex(), e.s.log, 45*time.Second,
		func(context.Context, queue.Job) queue.Outcome { return queue.Outcome{} },
		e.s.retireExperiment,
	))
}

func TestDatasetVersionSnapshotsAndExperimentPinning(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	e.attachQueue()

	status, out := e.json(t, "POST", "/api/v1/projects", "", map[string]interface{}{"name": "versions"})
	if status != 200 {
		t.Fatalf("create project: %d %v", status, out)
	}
	projectID := out["id"].(string)

	status, out = e.json(t, "POST", "/api/v1/projects/"+projectID+"/datasets", "", map[string]interface{}{"name": "ds"})
	if status != 200 {
		t.Fatalf("create dataset: %d %v", status, out)
	}
	datasetID := out["id"].(string)

	// A new dataset owns a real (empty) version 1, so an evaluation can pin it
	// before the first record exists.
	status, ds := e.json(t, "GET", "/api/v1/datasets/"+datasetID, "", nil)
	if status != 200 {
		t.Fatalf("dataset detail: %d %v", status, ds)
	}
	v1, _ := ds["latest_version_id"].(string)
	if v1 == "" {
		t.Fatalf("dataset has no initial version: %v", ds)
	}

	// Adding a record snapshots again; the version counter advances.
	status, _ = e.json(t, "POST", "/api/v1/datasets/"+datasetID+"/records", "",
		map[string]interface{}{"input": "2+2", "expected": "4"})
	if status != 200 {
		t.Fatalf("add record: %d", status)
	}
	_, ds = e.json(t, "GET", "/api/v1/datasets/"+datasetID, "", nil)
	v2, _ := ds["latest_version_id"].(string)
	if v2 == "" || v2 == v1 {
		t.Fatalf("version did not advance: v1=%q v2=%q", v1, v2)
	}

	// Pin v2 explicitly.
	status, evalOut := e.json(t, "POST", "/api/v1/projects/"+projectID+"/evaluations", "",
		map[string]interface{}{"dataset_id": datasetID, "experiment_name": "pinned", "scorer_names": []string{"exact_match"}})
	if status != 200 {
		t.Fatalf("start eval: %d %v", status, evalOut)
	}
	pinned, _ := evalOut["dataset_version_id"].(string)
	if pinned != v2 {
		t.Fatalf("experiment pinned %q, want latest %q", pinned, v2)
	}

	// Mutating the live dataset must not change the pinned snapshot.
	status, _ = e.json(t, "POST", "/api/v1/datasets/"+datasetID+"/records", "",
		map[string]interface{}{"input": "3+3", "expected": "6"})
	if status != 200 {
		t.Fatalf("second add: %d", status)
	}
	status, snapshot := e.json(t, "GET", "/api/v1/dataset_versions/"+pinned, "", nil)
	if status != 200 {
		t.Fatalf("read pinned version: %d %v", status, snapshot)
	}
	records, _ := snapshot["records"].([]interface{})
	if len(records) != 1 {
		t.Fatalf("pinned snapshot has %d records, want the original 1", len(records))
	}

	var storedVersion string
	if err := e.s.DB.QueryRow(`SELECT COALESCE(dataset_version_id,'') FROM experiments WHERE id=?`,
		evalOut["experiment_id"].(string)).Scan(&storedVersion); err != nil {
		t.Fatal(err)
	}
	if storedVersion != pinned {
		t.Fatalf("experiment row pinned %q, want %q", storedVersion, pinned)
	}
}

func TestExperimentPinsPromptVersion(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	e.attachQueue()

	_, project := e.json(t, "POST", "/api/v1/projects", "", map[string]interface{}{"name": "prompts"})
	projectID := project["id"].(string)
	_, dataset := e.json(t, "POST", "/api/v1/projects/"+projectID+"/datasets", "", map[string]interface{}{"name": "ds"})
	datasetID := dataset["id"].(string)

	_, p1 := e.json(t, "POST", "/api/v1/projects/"+projectID+"/prompts", "",
		map[string]interface{}{"name": "answer", "template": "old: {input}"})
	_, p2 := e.json(t, "POST", "/api/v1/projects/"+projectID+"/prompts", "",
		map[string]interface{}{"name": "answer", "template": "new: {input}"})
	if p1["id"] == p2["id"] {
		t.Fatal("prompt versions share an id")
	}

	status, out := e.json(t, "POST", "/api/v1/projects/"+projectID+"/evaluations", "",
		map[string]interface{}{
			"dataset_id": datasetID, "experiment_name": "prompt-pinned",
			"prompt_name": "answer", "prompt_version": 1,
		})
	if status != 200 {
		t.Fatalf("start eval: %d %v", status, out)
	}
	if out["prompt_version_id"] != p1["id"] {
		t.Fatalf("pinned prompt = %v, want %v", out["prompt_version_id"], p1["id"])
	}

	status, prompt := e.json(t, "GET", "/api/v1/prompts/"+p1["id"].(string), "", nil)
	if status != 200 {
		t.Fatalf("read prompt version: %d", status)
	}
	if prompt["version"].(float64) != 1 {
		t.Fatalf("prompt version = %v", prompt["version"])
	}

	// A rerun can pin the id directly, which is what the UI does.
	status, rerun := e.json(t, "POST", "/api/v1/projects/"+projectID+"/evaluations", "",
		map[string]interface{}{
			"dataset_id": datasetID, "experiment_name": "rerun",
			"prompt_version_id": p1["id"],
		})
	if status != 200 {
		t.Fatalf("rerun eval: %d %v", status, rerun)
	}
	if rerun["prompt_version_id"] != p1["id"] {
		t.Fatalf("rerun pinned prompt = %v, want %v", rerun["prompt_version_id"], p1["id"])
	}
	// A prompt from another project must not be pinnable by id.
	_, otherProject := e.json(t, "POST", "/api/v1/projects", "", map[string]interface{}{"name": "other"})
	otherPID := otherProject["id"].(string)
	_, otherDataset := e.json(t, "POST", "/api/v1/projects/"+otherPID+"/datasets", "", map[string]interface{}{"name": "other-ds"})
	status, _ = e.json(t, "POST", "/api/v1/projects/"+otherPID+"/evaluations", "",
		map[string]interface{}{"dataset_id": otherDataset["id"], "prompt_version_id": p1["id"]})
	if status != 404 {
		t.Fatalf("cross-project eval status = %d, want 404", status)
	}
}

func TestTokenSourceIsExplicit(t *testing.T) {
	e := newTestEnvWithAuth(t, false)
	_, project := e.json(t, "POST", "/api/v1/projects", "", map[string]interface{}{"name": "tokens"})
	projectID := project["id"].(string)

	status, _ := e.json(t, "POST", "/api/v1/projects/"+projectID+"/spans:ingest", "", map[string]interface{}{
		"events": []map[string]interface{}{
			{"id": "reported", "trace_id": "t-reported", "name": "llm", "type": "llm", "model": "gpt-4o",
				"input": "hello", "output": "hi", "prompt_tokens": 100, "completion_tokens": 20},
			{"id": "estimated", "trace_id": "t-estimated", "name": "llm2", "type": "llm", "model": "gpt-4o",
				"input": "hello there", "output": "hi there"},
			{"id": "plain", "trace_id": "t-plain", "name": "tool", "type": "task", "input": "x"},
		},
	})
	if status != 200 {
		t.Fatalf("ingest: %d", status)
	}

	var source string
	if err := e.s.DB.QueryRow(`SELECT token_source FROM spans WHERE id='reported'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "provider" {
		t.Errorf("reported token_source = %q, want provider", source)
	}
	if err := e.s.DB.QueryRow(`SELECT token_source FROM spans WHERE id='estimated'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "estimated" {
		t.Errorf("estimated token_source = %q, want estimated", source)
	}
	if err := e.s.DB.QueryRow(`SELECT COALESCE(token_source,'') FROM spans WHERE id='plain'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "" {
		t.Errorf("non-LLM token_source = %q, want empty", source)
	}
}
