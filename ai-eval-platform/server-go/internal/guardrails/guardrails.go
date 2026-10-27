// Package guardrails is EvalForge's runtime safety layer.
//
// Evaluation and protection are different jobs. A scorer answers "was this
// response good?" after the fact. A guardrail answers "may this text leave the
// application at all?" — it runs before the text is used or stored, and its
// answer is an action: allow, flag, redact, or block.
//
// The checks here are deterministic and local:
//
//   - PII: structured identifiers (email, phone, card, SSN, IBAN, IP, URL),
//     with the matched values masked in the redacted text.
//   - PROMPT_INJECTION: phrases that try to override the system instruction.
//   - TOPIC: a banned-topic list, in restrict mode (match fails) or allow mode
//     (anything outside the list fails).
//   - CUSTOM: an operator-defined policy — regexes and keywords — which is the
//     honest local stand-in for a trained classifier.
//
// What this package deliberately does not do: run a trained model. A custom
// classifier needs a training pipeline and an inference service, and pretending
// a regex list is one would be worse than saying so.
package guardrails

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"evalforge/internal/model"
)

// Check types.
const (
	TypePII             = "PII"
	TypePromptInjection = "PROMPT_INJECTION"
	TypeTopic           = "TOPIC"
	TypeCustom          = "CUSTOM"
)

// Actions.
const (
	ActionAllow  = "allow"
	ActionFlag   = "flag"
	ActionRedact = "redact"
	ActionBlock  = "block"
)

// Modes a policy can run in.
var modes = map[string]bool{ActionFlag: true, ActionRedact: true, ActionBlock: true}

// Config is the union of a check's options. One struct rather than four types
// keeps the JSON column and the API honest: an unknown field is a mistake, not
// a silently ignored option.
type Config struct {
	// PII
	Entities []string `json:"entities,omitempty"`
	// TOPIC
	Topics []string `json:"topics,omitempty"`
	// restrict: a listed topic fails. allow: a topic not listed fails.
	Mode      string  `json:"mode,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
	// CUSTOM
	Keywords []string `json:"keywords,omitempty"`
	Regexes  []string `json:"regexes,omitempty"`
}

// Check is one detector plus its configuration.
type Check struct {
	Type   string `json:"type"`
	Config Config `json:"config"`
}

// Policy is a project's guardrail configuration.
type Policy struct {
	ID        string
	ProjectID string
	Name      string
	Enabled   bool
	Mode      string
	Checks    []Check
	CreatedAt string
	UpdatedAt string
}

// Finding is one detector's verdict on one piece of text.
type Finding struct {
	Type    string `json:"type"`
	Matched string `json:"matched"`
	Detail  string `json:"detail"`
}

// Result is the outcome of scanning one text against a policy.
type Result struct {
	PolicyID   string    `json:"policy_id"`
	PolicyName string    `json:"policy_name"`
	Passed     bool      `json:"passed"`
	Action     string    `json:"action"`
	Findings   []Finding `json:"findings"`
	// Redacted is Text with every matched value masked. It is what a redact
	// policy stores instead of the original.
	Redacted string `json:"redacted"`
}

// Tripped reports whether any finding was produced.
func (r Result) Tripped() bool { return len(r.Findings) > 0 }

// piiPatterns are the structured identifiers we can detect precisely. Names and
// addresses need a model, not a regexp, so they are not in this list.
var piiPatterns = []struct {
	Entity string
	Re     *regexp.Regexp
}{
	{"EMAIL_ADDRESS", regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)},
	{"IBAN_CODE", regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}[A-Z0-9]{10,30}\b`)},
	{"CREDIT_CARD", regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)},
	{"SSN", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{"PHONE_NUMBER", regexp.MustCompile(`\+?\d[\d\s().\-]{8,}\d`)},
	{"IP_ADDRESS", regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
	{"URL", regexp.MustCompile(`https?://[^\s<>"]+`)},
}

// injectionPatterns are instruction-override phrasings. They are matched
// case-insensitively across the whole text.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all )?(the )?(previous|prior|above) (instructions|prompts|rules)`),
	regexp.MustCompile(`(?i)disregard (all )?(the )?(previous|prior|above) (instructions|prompts|rules)`),
	regexp.MustCompile(`(?i)forget (everything|all|your) (you (know|were told)|instructions|rules)`),
	regexp.MustCompile(`(?i)you are now (a|an|the) [a-z ]{3,40}(assistant|model|ai)\b`),
	regexp.MustCompile(`(?i)(reveal|print|show|repeat) (your|the) (system )?(prompt|instructions|rules)`),
	regexp.MustCompile(`(?i)(jailbreak|developer mode|do anything now|\bDAN\b)`),
	regexp.MustCompile(`(?i)new (system )?(instructions|rules)\s*:`),
	regexp.MustCompile(`(?i)^\s*system\s*:`),
}

// lowercase keywords used by the topic check.
func normList(v []string) []string {
	out := make([]string, 0, len(v))
	for _, s := range v {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Scan runs every check in a policy over one text and returns the aggregate
// result. It is pure: no database, no clock, no network.
func Scan(text string, checks []Check, redact bool) ([]Finding, string) {
	findings := []Finding{}
	redacted := text
	for _, c := range checks {
		switch strings.ToUpper(c.Type) {
		case TypePII:
			f, out := scanPII(text, redacted, c.Config)
			findings = append(findings, f...)
			redacted = out
		case TypePromptInjection:
			findings = append(findings, scanInjection(text, c.Config)...)
		case TypeTopic:
			findings = append(findings, scanTopic(text, c.Config)...)
		case TypeCustom:
			f, out := scanCustom(text, redacted, c.Config)
			findings = append(findings, f...)
			redacted = out
		}
	}
	_ = redact
	return findings, redacted
}

// scanPII detects structured identifiers and masks the ones it finds.
func scanPII(text, redacted string, cfg Config) ([]Finding, string) {
	wanted := map[string]bool{}
	for _, e := range cfg.Entities {
		wanted[strings.ToUpper(strings.TrimSpace(e))] = true
	}
	findings := []Finding{}
	for _, p := range piiPatterns {
		if len(wanted) > 0 && !wanted[p.Entity] {
			continue
		}
		matches := p.Re.FindAllString(text, -1)
		if len(matches) == 0 {
			continue
		}
		// A 13-19 digit run is only a card if it passes the Luhn checksum;
		// without that, order numbers and timestamps become "PII" and the
		// guardrail cries wolf until someone turns it off.
		if p.Entity == "CREDIT_CARD" {
			kept := matches[:0]
			for _, m := range matches {
				if luhn(m) {
					kept = append(kept, m)
				}
			}
			matches = kept
			if len(matches) == 0 {
				continue
			}
		}
		for _, m := range matches {
			redacted = strings.ReplaceAll(redacted, m, "["+p.Entity+"]")
		}
		findings = append(findings, Finding{
			Type: TypePII, Matched: p.Entity,
			Detail: fmt.Sprintf("%d %s match(es)", len(matches), p.Entity),
		})
	}
	return findings, redacted
}

// luhn validates a card-like digit run.
func luhn(s string) bool {
	digits := make([]int, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// scanInjection scores how many override phrasings appear. The threshold lets
// an operator require more than one hit before treating the text as hostile.
func scanInjection(text string, cfg Config) []Finding {
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = 1
	}
	hits := []string{}
	for _, re := range injectionPatterns {
		if re.MatchString(text) {
			hits = append(hits, re.String())
		}
	}
	if len(hits) == 0 || float64(len(hits)) < threshold {
		return nil
	}
	return []Finding{{
		Type: TypePromptInjection, Matched: "instruction_override",
		Detail: fmt.Sprintf("%d override pattern(s) matched", len(hits)),
	}}
}

// scanTopic implements restrict and allow modes over a keyword list.
func scanTopic(text string, cfg Config) []Finding {
	topics := normList(cfg.Topics)
	if len(topics) == 0 {
		return nil
	}
	lower := strings.ToLower(text)
	matched := []string{}
	for _, t := range topics {
		if strings.Contains(lower, t) {
			matched = append(matched, t)
		}
	}
	switch strings.ToLower(cfg.Mode) {
	case "allow":
		// Everything outside the allowed list is a finding; an empty match set
		// is the failure.
		if len(matched) == 0 {
			return []Finding{{
				Type: TypeTopic, Matched: "outside_allowed_topics",
				Detail: "text matches none of the allowed topics",
			}}
		}
		return nil
	default: // restrict
		if len(matched) == 0 {
			return nil
		}
		sort.Strings(matched)
		return []Finding{{
			Type: TypeTopic, Matched: strings.Join(matched, ","),
			Detail: fmt.Sprintf("%d banned topic(s) matched", len(matched)),
		}}
	}
}

// scanCustom applies an operator-defined policy. It is the local equivalent of
// a custom classifier: deterministic rules the operator wrote, not a model.
func scanCustom(text, redacted string, cfg Config) ([]Finding, string) {
	findings := []Finding{}
	lower := strings.ToLower(text)
	for _, kw := range normList(cfg.Keywords) {
		if strings.Contains(lower, kw) {
			findings = append(findings, Finding{
				Type: TypeCustom, Matched: kw, Detail: "keyword matched",
			})
		}
	}
	for _, expr := range cfg.Regexes {
		re, err := regexp.Compile(expr)
		if err != nil {
			findings = append(findings, Finding{
				Type: TypeCustom, Matched: "invalid_regex", Detail: expr + ": " + err.Error(),
			})
			continue
		}
		matches := re.FindAllString(text, -1)
		if len(matches) == 0 {
			continue
		}
		for _, m := range matches {
			redacted = strings.ReplaceAll(redacted, m, "[CUSTOM]")
		}
		findings = append(findings, Finding{
			Type: TypeCustom, Matched: expr,
			Detail: fmt.Sprintf("%d match(es)", len(matches)),
		})
	}
	return findings, redacted
}

// ---------- policy storage ----------

// Store owns guardrail policies and their event log.
type Store struct {
	control   *sql.DB
	telemetry *sql.DB
}

// New builds a store over the two planes.
func New(control, telemetry *sql.DB) *Store {
	return &Store{control: control, telemetry: telemetry}
}

const policyColumns = `id, project_id, name, enabled, mode, checks, created_at, updated_at`

func scanPolicy(sc interface{ Scan(...interface{}) error }) (Policy, error) {
	var p Policy
	var enabled int
	var checks string
	if err := sc.Scan(&p.ID, &p.ProjectID, &p.Name, &enabled, &p.Mode, &checks,
		&p.CreatedAt, &p.UpdatedAt); err != nil {
		return Policy{}, err
	}
	p.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(checks), &p.Checks); err != nil {
		p.Checks = []Check{}
	}
	return p, nil
}

// List returns every policy for a project.
func (s *Store) List(projectID string) ([]Policy, error) {
	rows, err := s.control.Query(
		`SELECT `+policyColumns+` FROM guardrail_policies WHERE project_id=? ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Policy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one policy scoped to its project.
func (s *Store) Get(projectID, id string) (Policy, error) {
	return scanPolicy(s.control.QueryRow(
		`SELECT `+policyColumns+` FROM guardrail_policies WHERE id=? AND project_id=?`, id, projectID))
}

// Insert stores a new policy.
func (s *Store) Insert(p *Policy) error {
	now := model.NowUTC()
	p.ID = model.NewID()
	p.CreatedAt, p.UpdatedAt = now, now
	checks, _ := json.Marshal(p.Checks)
	_, err := s.control.Exec(
		`INSERT INTO guardrail_policies(id, project_id, name, enabled, mode, checks, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		p.ID, p.ProjectID, p.Name, boolInt(p.Enabled), p.Mode, string(checks), p.CreatedAt, p.UpdatedAt)
	return err
}

// Save updates the editable fields of a policy.
func (s *Store) Save(p Policy) error {
	p.UpdatedAt = model.NowUTC()
	checks, _ := json.Marshal(p.Checks)
	_, err := s.control.Exec(
		`UPDATE guardrail_policies SET name=?, enabled=?, mode=?, checks=?, updated_at=?
		 WHERE id=? AND project_id=?`,
		p.Name, boolInt(p.Enabled), p.Mode, string(checks), p.UpdatedAt, p.ID, p.ProjectID)
	return err
}

// Delete removes a policy. Its events stay as the audit trail.
func (s *Store) Delete(projectID, id string) error {
	_, err := s.control.Exec(
		`DELETE FROM guardrail_policies WHERE id=? AND project_id=?`, id, projectID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ValidMode reports whether a mode is one we enforce.
func ValidMode(m string) bool { return modes[m] }

// ValidType reports whether a check type is known.
func ValidType(t string) bool {
	switch strings.ToUpper(t) {
	case TypePII, TypePromptInjection, TypeTopic, TypeCustom:
		return true
	}
	return false
}
