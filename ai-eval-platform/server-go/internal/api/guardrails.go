package api

// Runtime guardrails: policy CRUD, the pre-flight check endpoint, and the hook
// that enforcement runs through.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"evalforge/internal/guardrails"
	"evalforge/internal/model"
)

type guardrailIn struct {
	Name    string             `json:"name"`
	Enabled *bool              `json:"enabled"`
	Mode    string             `json:"mode"`
	Checks  []guardrails.Check `json:"checks"`
}

func validateGuardrail(in guardrailIn) string {
	if strings.TrimSpace(in.Name) == "" {
		return "name is required"
	}
	if len(in.Name) > 120 {
		return "name is too long (max 120)"
	}
	mode := in.Mode
	if mode == "" {
		mode = guardrails.ActionFlag
	}
	if !guardrails.ValidMode(mode) {
		return "mode must be flag, redact or block"
	}
	if len(in.Checks) == 0 {
		return "at least one check is required"
	}
	for _, c := range in.Checks {
		if !guardrails.ValidType(c.Type) {
			return "unknown check type: " + c.Type + " (PII, PROMPT_INJECTION, TOPIC, CUSTOM)"
		}
		switch strings.ToUpper(c.Type) {
		case guardrails.TypeTopic:
			if len(c.Config.Topics) == 0 {
				return "TOPIC requires at least one topic"
			}
			if c.Config.Mode != "" && c.Config.Mode != "restrict" && c.Config.Mode != "allow" {
				return "TOPIC mode must be restrict or allow"
			}
		case guardrails.TypeCustom:
			if len(c.Config.Keywords) == 0 && len(c.Config.Regexes) == 0 {
				return "CUSTOM requires keywords or regexes"
			}
			for _, expr := range c.Config.Regexes {
				if _, err := regexp.Compile(expr); err != nil {
					return "invalid regexp: " + expr
				}
			}
		}
	}
	return ""
}

func guardrailJSON(p guardrails.Policy) map[string]interface{} {
	return map[string]interface{}{
		"id": p.ID, "project_id": p.ProjectID, "name": p.Name, "enabled": p.Enabled,
		"mode": p.Mode, "checks": p.Checks, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
	}
}

func (s *Server) handleGuardrailPolicies(w http.ResponseWriter, r *http.Request, projectID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		policies, err := s.guardrails.List(projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		out := make([]map[string]interface{}, 0, len(policies))
		for _, p := range policies {
			out = append(out, guardrailJSON(p))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in guardrailIn
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		if msg := validateGuardrail(in); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		policy := guardrails.Policy{
			ProjectID: projectID, Name: in.Name, Enabled: true,
			Mode: in.Mode, Checks: in.Checks,
		}
		if policy.Mode == "" {
			policy.Mode = guardrails.ActionFlag
		}
		if in.Enabled != nil {
			policy.Enabled = *in.Enabled
		}
		if err := s.guardrails.Insert(&policy); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") ||
				strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				errJSON(w, 409, "a policy with that name already exists in this project")
				return
			}
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, guardrailJSON(policy))
	default:
		errJSON(w, 405, "method not allowed")
	}
}

func (s *Server) handleGuardrailPolicy(w http.ResponseWriter, r *http.Request, projectID, policyID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	policy, err := s.guardrails.Get(projectID, policyID)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "guardrail policy not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, guardrailJSON(policy))
	case http.MethodPatch, http.MethodPost:
		body, err := readAll(r)
		if err != nil {
			errJSON(w, 400, "read body: "+err.Error())
			return
		}
		var in guardrailIn
		if err := json.Unmarshal(body, &in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		var sent map[string]json.RawMessage
		_ = json.Unmarshal(body, &sent)
		if _, ok := sent["name"]; ok {
			policy.Name = in.Name
		}
		if _, ok := sent["enabled"]; ok && in.Enabled != nil {
			policy.Enabled = *in.Enabled
		}
		if _, ok := sent["mode"]; ok {
			policy.Mode = in.Mode
		}
		if _, ok := sent["checks"]; ok {
			policy.Checks = in.Checks
		}
		check := guardrailIn{Name: policy.Name, Mode: policy.Mode, Checks: policy.Checks}
		if msg := validateGuardrail(check); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		if err := s.guardrails.Save(policy); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, guardrailJSON(policy))
	case http.MethodDelete:
		if err := s.guardrails.Delete(projectID, policyID); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"deleted": policyID})
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// handleGuardrailCheck is the runtime firewall: an application posts the text it
// is about to send (or log) and gets back a decision before acting on it.
func (s *Server) handleGuardrailCheck(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	var in struct {
		Text       string `json:"text"`
		PolicyID   string `json:"policy_id"`
		PolicyName string `json:"policy_name"`
		TraceID    string `json:"trace_id"`
		SpanID     string `json:"span_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		errJSON(w, 400, "bad json: "+err.Error())
		return
	}
	policies, err := s.enabledGuardrails(projectID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if in.PolicyID != "" || in.PolicyName != "" {
		filtered := make([]guardrails.Policy, 0, len(policies))
		for _, p := range policies {
			if (in.PolicyID != "" && p.ID == in.PolicyID) || (in.PolicyName != "" && p.Name == in.PolicyName) {
				filtered = append(filtered, p)
			}
		}
		policies = filtered
	}
	if len(policies) == 0 {
		writeJSON(w, 200, map[string]interface{}{
			"allowed": true, "action": guardrails.ActionAllow,
			"redacted": in.Text, "results": []interface{}{},
		})
		return
	}

	results := []map[string]interface{}{}
	action := guardrails.ActionAllow
	redacted := in.Text
	for _, p := range policies {
		findings, masked := guardrails.Scan(in.Text, p.Checks, p.Mode == guardrails.ActionRedact)
		effective := guardrails.ActionAllow
		if len(findings) > 0 {
			effective = p.Mode
		}
		action = strongestAction(action, effective)
		if effective == guardrails.ActionRedact {
			redacted = masked
		}
		results = append(results, map[string]interface{}{
			"policy_id": p.ID, "policy_name": p.Name,
			"passed": len(findings) == 0, "action": effective, "findings": findings,
		})
		s.recordGuardrailEvents(projectID, p, in.Text, findings, in.TraceID, in.SpanID, effective)
	}
	writeJSON(w, 200, map[string]interface{}{
		"allowed":  action != guardrails.ActionBlock,
		"action":   action,
		"redacted": redacted,
		"results":  results,
	})
}

// handleGuardrailEvents lists recent decisions.
func (s *Server) handleGuardrailEvents(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		errJSON(w, 405, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	rows, err := s.Telemetry.Query(
		`SELECT id, policy_id, policy_name, action, check_type, matched, detail, preview,
		        trace_id, span_id, created_at
		 FROM guardrail_events WHERE project_id=? ORDER BY created_at DESC LIMIT 200`, projectID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, policyID, policyName, action, checkType, matched, detail, preview, traceID, spanID, created string
		if rows.Scan(&id, &policyID, &policyName, &action, &checkType, &matched, &detail, &preview,
			&traceID, &spanID, &created) != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"id": id, "policy_id": policyID, "policy_name": policyName, "action": action,
			"check_type": checkType, "matched": matched, "detail": detail, "preview": preview,
			"trace_id": traceID, "span_id": spanID, "created_at": created,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"events": out})
}

// ---------- enforcement ----------

// enabledGuardrails loads the project's enabled policies. Ingest calls this
// once per batch, not once per span.
func (s *Server) enabledGuardrails(projectID string) ([]guardrails.Policy, error) {
	all, err := s.guardrails.List(projectID)
	if err != nil {
		return nil, err
	}
	out := make([]guardrails.Policy, 0, len(all))
	for _, p := range all {
		if p.Enabled && len(p.Checks) > 0 {
			out = append(out, p)
		}
	}
	return out, nil
}

// guardrailDecision is the aggregate verdict for one text.
type guardrailDecision struct {
	Action   string
	Redacted string
	Policy   string
	Findings []guardrails.Finding
}

// applyGuardrails scans one text and records what each policy decided.
//
// The action is the strongest any policy asked for, so a `block` policy cannot
// be watered down by a `flag` policy that also matched.
func (s *Server) applyGuardrails(projectID, traceID, spanID, text string, policies []guardrails.Policy) guardrailDecision {
	decision := guardrailDecision{Action: guardrails.ActionAllow, Redacted: text}
	if text == "" || len(policies) == 0 {
		return decision
	}
	for _, p := range policies {
		findings, masked := guardrails.Scan(text, p.Checks, p.Mode == guardrails.ActionRedact)
		if len(findings) == 0 {
			continue
		}
		decision.Action = strongestAction(decision.Action, p.Mode)
		if p.Mode == guardrails.ActionRedact {
			decision.Redacted = masked
		}
		if decision.Policy == "" {
			decision.Policy = p.Name
			decision.Findings = findings
		}
		s.recordGuardrailEvents(projectID, p, text, findings, traceID, spanID, p.Mode)
	}
	return decision
}

// strongestAction orders actions by how much they restrict the request.
func strongestAction(a, b string) string {
	rank := map[string]int{
		guardrails.ActionAllow: 0, guardrails.ActionFlag: 1,
		guardrails.ActionRedact: 2, guardrails.ActionBlock: 3,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// recordGuardrailEvents writes one row per finding. The preview is redacted and
// truncated: the event log must not become the place the secret is kept.
func (s *Server) recordGuardrailEvents(projectID string, p guardrails.Policy, text string,
	findings []guardrails.Finding, traceID, spanID, action string) {
	if len(findings) == 0 {
		return
	}
	_, masked := guardrails.Scan(text, p.Checks, true)
	preview := truncate(masked, 160)
	for _, f := range findings {
		_, err := s.Telemetry.Exec(
			`INSERT INTO guardrail_events(id, project_id, policy_id, policy_name, action, check_type,
			 matched, detail, preview, trace_id, span_id, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			model.NewID(), projectID, p.ID, p.Name, action, f.Type,
			f.Matched, f.Detail, preview, traceID, spanID, model.NowUTC())
		if err != nil {
			s.log.Warn("record guardrail event", "policy_id", p.ID, "err", err)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// asGuardrailText projects a span field onto the text a policy scans. A string
// is scanned as-is; a structure is scanned as its JSON, which is what a leak
// would look like anyway.
func asGuardrailText(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// guardrailSummary feeds the dashboard block.
func (s *Server) guardrailSummary(projectID string) map[string]interface{} {
	out := map[string]interface{}{
		"policies": 0, "enabled": 0, "blocked": 0, "redacted": 0, "flagged": 0,
	}
	policies, err := s.guardrails.List(projectID)
	if err != nil {
		return out
	}
	out["policies"] = len(policies)
	for _, p := range policies {
		if p.Enabled {
			out["enabled"] = out["enabled"].(int) + 1
		}
	}
	rows, err := s.Telemetry.Query(
		`SELECT action, COUNT(*) FROM guardrail_events
		 WHERE project_id=? AND created_at >= ? GROUP BY action`,
		projectID, model.NowUTC()[:10])
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		var n int
		if rows.Scan(&action, &n) == nil {
			out[action] = n
		}
	}
	return out
}

// sortFindings gives a stable order to an aggregate result.
func sortFindings(f []guardrails.Finding) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Type != f[j].Type {
			return f[i].Type < f[j].Type
		}
		return f[i].Matched < f[j].Matched
	})
}
