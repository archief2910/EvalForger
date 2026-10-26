package online

import "testing"

func TestRuleMatches(t *testing.T) {
	trace := TraceInfo{ID: "t1", Name: "Support Agent", Status: "error", Models: []string{"gpt-4o", "gpt-4o-mini"}}
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"no filters", Rule{}, true},
		{"status match", Rule{FilterStatus: "error"}, true},
		{"status mismatch", Rule{FilterStatus: "ok"}, false},
		{"status is case-insensitive", Rule{FilterStatus: "ERROR"}, true},
		{"name substring", Rule{FilterName: "support"}, true},
		{"name mismatch", Rule{FilterName: "billing"}, false},
		{"model exact", Rule{FilterModel: "gpt-4o-mini"}, true},
		{"model mismatch", Rule{FilterModel: "claude"}, false},
		{"combined match", Rule{FilterStatus: "error", FilterModel: "gpt-4o", FilterName: "agent"}, true},
		{"combined mismatch", Rule{FilterStatus: "ok", FilterModel: "gpt-4o"}, false},
	}
	for _, c := range cases {
		if got := c.rule.Matches(trace); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSamplingIsDeterministic(t *testing.T) {
	rule := Rule{ID: "rule-1"}
	if !(Rule{ID: "r", SampleRate: 1}).Sampled("t") {
		t.Fatal("rate 1 must always sample")
	}
	if (Rule{ID: "r", SampleRate: 0}).Sampled("t") {
		t.Fatal("rate 0 must never sample")
	}
	first := rule.Sampled("trace-abc")
	for i := 0; i < 5; i++ {
		if got := rule.Sampled("trace-abc"); got != first {
			t.Fatalf("sampling is not deterministic: %v then %v", first, got)
		}
	}
	// Across many traces, a 50% rule should land near half. This is a sanity
	// band, not a statistical test.
	half := Rule{ID: "rule-1", SampleRate: 0.5}
	hits := 0
	for i := 0; i < 2000; i++ {
		if half.Sampled("trace-" + itoa(uint64(i))) {
			hits++
		}
	}
	if hits < 800 || hits > 1200 {
		t.Fatalf("50%% rule sampled %d/2000 traces", hits)
	}
}
