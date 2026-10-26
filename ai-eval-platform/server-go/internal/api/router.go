package api

// Routing. One ServeMux, thin path parsing, handlers as Server methods. The
// route table is intentionally explicit so `go doc` and a reviewer can see the
// whole public surface in one place.

import (
	"net/http"
	"strings"
)

// Router returns the API route table (without middleware).
func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		s.handleHealth(w, r)
	})

	// OTLP/HTTP: the path every OpenTelemetry SDK and collector is pointed at.
	// Mounted at the root because exporters append /v1/traces to a base URL.
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		s.handleOTLPTraces(w, r, "")
	})
	mux.HandleFunc("/otel/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		s.handleOTLPTraces(w, r, "")
	})

	// ---- agent interface (MCP) and its authorization server ----
	mux.HandleFunc("/mcp", s.handleMCPHTTP)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleOAuthMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource/", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/oauth/register", s.handleOAuthRegister)
	mux.HandleFunc("/oauth/authorize", s.handleOAuthAuthorize)
	mux.HandleFunc("/oauth/token", s.handleOAuthToken)
	mux.HandleFunc("/oauth/revoke", s.handleOAuthRevoke)

	// ---- credentials ----
	mux.HandleFunc("/api/v1/keys", s.handleKeys)
	mux.HandleFunc("/api/v1/keys/", s.handleKeys)
	mux.HandleFunc("/api/v1/whoami", s.handleWhoAmI)

	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		s.handleProjects(w, r, "")
	})
	mux.HandleFunc("/api/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
		if len(parts) < 2 {
			errJSON(w, 404, "not found")
			return
		}
		projectID := parts[1]
		rest := parts[2:]
		switch {
		case len(rest) == 0:
			if r.Method != http.MethodGet {
				errJSON(w, 405, "method not allowed")
				return
			}
			s.handleProjectOne(w, r, "", projectID)
		case len(rest) == 1 && rest[0] == "spans:ingest":
			if r.Method != http.MethodPost {
				errJSON(w, 405, "method not allowed")
				return
			}
			s.handleIngest(w, r, projectID)
		case len(rest) == 3 && rest[0] == "otel" && rest[1] == "v1" && rest[2] == "traces":
			s.handleOTLPTraces(w, r, projectID)
		case len(rest) == 1 && rest[0] == "traces":
			s.handleTraces(w, r, projectID)
		case len(rest) == 2 && rest[0] == "traces":
			s.handleTraceDetail(w, r, projectID, rest[1])
		case len(rest) == 3 && rest[0] == "traces" && rest[2] == "feedback":
			s.handleFeedback(w, r, projectID, rest[1])
		case len(rest) == 1 && rest[0] == "dashboard":
			s.handleDashboard(w, r, projectID)
		case len(rest) == 1 && rest[0] == "threads":
			s.handleThreads(w, r, projectID)
		case len(rest) == 2 && rest[0] == "threads":
			s.handleThreadDetail(w, r, projectID, rest[1])
		case len(rest) == 3 && rest[0] == "threads" && (rest[2] == "close" || rest[2] == "open"):
			s.handleThreadStatus(w, r, projectID, rest[1], rest[2])
		case len(rest) == 3 && rest[0] == "threads" && rest[2] == "scores":
			s.handleThreadScores(w, r, projectID, rest[1])
		case len(rest) == 3 && rest[0] == "threads" && rest[2] == "evaluate":
			s.handleThreadEvaluate(w, r, projectID, rest[1])
		case len(rest) == 1 && rest[0] == "datasets":
			s.handleDatasets(w, r, projectID)
		case len(rest) == 1 && rest[0] == "prompts":
			s.handlePrompts(w, r, projectID)
		case len(rest) == 1 && rest[0] == "evaluations":
			s.handleStartEval(w, r, projectID)
		case len(rest) == 1 && rest[0] == "experiments":
			s.handleExperimentsList(w, r, projectID)
		default:
			errJSON(w, 404, "route not found: "+strings.Join(rest, "/"))
		}
	})

	mux.HandleFunc("/api/v1/scorers", func(w http.ResponseWriter, r *http.Request) {
		s.handleScorers(w, r)
	})

	mux.HandleFunc("/api/v1/datasets/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/datasets/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			errJSON(w, 404, "not found")
			return
		}
		dsID := parts[0]
		switch {
		case len(parts) == 1:
			s.handleDatasetOne(w, r, "", dsID)
		case len(parts) == 2 && parts[1] == "records":
			s.handleAddRecord(w, r, "", dsID)
		case len(parts) == 2 && parts[1] == "records:from_trace":
			s.handleRecordFromTrace(w, r, "", dsID)
		case len(parts) == 2 && parts[1] == "versions":
			s.handleDatasetVersions(w, r, "", dsID)
		default:
			errJSON(w, 404, "route not found")
		}
	})

	// An immutable dataset snapshot, read by the worker. Deliberately not under
	// the dataset path: an experiment pins the snapshot id and nothing else.
	mux.HandleFunc("/api/v1/dataset_versions/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/dataset_versions/")
		if id == "" || strings.Contains(id, "/") {
			errJSON(w, 404, "not found")
			return
		}
		if r.Method != http.MethodGet {
			errJSON(w, 405, "method not allowed")
			return
		}
		s.handleDatasetVersion(w, r, "", id)
	})

	// One prompt version by id. Name/version resolution happens at experiment
	// creation, so the worker never has to guess.
	mux.HandleFunc("/api/v1/prompts/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/prompts/")
		if id == "" || strings.Contains(id, "/") {
			errJSON(w, 404, "not found")
			return
		}
		if r.Method != http.MethodGet {
			errJSON(w, 405, "method not allowed")
			return
		}
		s.handlePromptVersion(w, r, "", id)
	})

	mux.HandleFunc("/api/v1/experiments/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/experiments/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			errJSON(w, 404, "not found")
			return
		}
		expID := parts[0]
		switch {
		case len(parts) == 1:
			s.handleExperimentDetail(w, r, "", expID)
		case len(parts) == 2 && parts[1] == "compare":
			s.handleCompare(w, r, "", expID)
		default:
			errJSON(w, 404, "route not found")
		}
	})

	mux.HandleFunc("/api/v1/worker_callback/thread/", func(w http.ResponseWriter, r *http.Request) {
		jobID := strings.TrimPrefix(r.URL.Path, "/api/v1/worker_callback/thread/")
		if jobID == "" || strings.Contains(jobID, "/") {
			errJSON(w, 404, "bad path")
			return
		}
		s.handleThreadCallback(w, r, jobID)
	})

	mux.HandleFunc("/api/v1/worker_callback/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/worker_callback/"), "/")
		if len(parts) != 1 || parts[0] == "" {
			errJSON(w, 404, "bad path")
			return
		}
		s.handleWorkerCallback(w, r, "", parts[0])
	})

	return mux
}
