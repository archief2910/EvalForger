package sandbox

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The sandbox is a security boundary, so the tests that matter most are the
// ones that prove it refuses to run rather than the ones that prove it can.

func TestDockerRunnerIsUnavailableWithoutADaemon(t *testing.T) {
	// This test is skipped when a daemon is present, because then the point
	// (refusal) is not the behaviour under test.
	r := NewDockerRunner()
	if r.Available() {
		t.Skip("docker daemon is reachable; the unavailable path is not exercised")
	}
	_, err := r.Run(context.Background(), "def score(p): return 1.0", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected an error when docker is unavailable")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error should wrap ErrUnavailable, got %v", err)
	}
}

func TestLocalRunnerFailsClosedByDefault(t *testing.T) {
	r := &LocalRunner{}
	if r.Available() {
		t.Fatal("a local runner must not be available unless explicitly allowed")
	}
	_, err := r.Run(context.Background(), "def score(p): return 1.0", nil)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("refusal should wrap ErrUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "SANDBOX_ALLOW_UNSAFE_LOCAL") {
		t.Fatalf("refusal should say how to opt in, got %v", err)
	}
}

// TestLocalRunnerExecutesWhenAllowed exercises the runner script itself: the
// container path and the local path share it, so this is the end-to-end check
// of the metric contract when no daemon is present.
func TestLocalRunnerExecutesWhenAllowed(t *testing.T) {
	if _, err := os.Stat("runner.py"); err != nil {
		t.Skip("runner.py not present")
	}
	r := &LocalRunner{AllowUnsafe: true, Timeout: 20 * time.Second}

	cases := []struct {
		name    string
		code    string
		payload map[string]interface{}
		want    float64
	}{
		{
			name:    "function returning a float",
			code:    "def score(payload):\n    return 1.0\n",
			payload: map[string]interface{}{"input": "a", "output": "b"},
			want:    1.0,
		},
		{
			name:    "function returning a dict",
			code:    "def score(payload):\n    return {'score': 0.25, 'rationale': 'meh'}\n",
			payload: map[string]interface{}{"input": "a", "output": "b"},
			want:    0.25,
		},
		{
			name:    "class with a score method",
			code:    "class Metric:\n    def score(self, **kwargs):\n        return 0.5\n",
			payload: map[string]interface{}{"input": "a", "output": "b"},
			want:    0.5,
		},
		{
			name:    "reads the payload",
			code:    "def score(payload):\n    return 1.0 if 'refund' in str(payload.get('output','')) else 0.0\n",
			payload: map[string]interface{}{"input": "q", "output": "refund policy"},
			want:    1.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := r.Run(context.Background(), tc.code, tc.payload)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.Score == nil {
				t.Fatalf("no score returned: %+v", res)
			}
			if *res.Score != tc.want {
				t.Fatalf("score = %v, want %v", *res.Score, tc.want)
			}
		})
	}
}

func TestLocalRunnerReportsMetricErrors(t *testing.T) {
	if _, err := os.Stat("runner.py"); err != nil {
		t.Skip("runner.py not present")
	}
	r := &LocalRunner{AllowUnsafe: true, Timeout: 20 * time.Second}

	cases := []struct {
		name string
		code string
		want string
	}{
		{"syntax error", "def score(payload:\n", "invalid Python code"},
		{"no score", "x = 1\n", "define a score"},
		{"raises", "def score(payload):\n    raise ValueError('boom')\n", "boom"},
		{"not a number", "def score(payload):\n    return 'high'\n", "not a number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Run(context.Background(), tc.code, map[string]interface{}{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseResultRejectsOutOfRangeScores(t *testing.T) {
	_, err := parseResult([]byte(`{"score": 1.5}`))
	if err == nil {
		t.Fatal("a score outside [0,1] must be rejected")
	}
	_, err = parseResult([]byte(`{"score": -0.1}`))
	if err == nil {
		t.Fatal("a negative score must be rejected")
	}
	res, err := parseResult([]byte(`{"score": 0.5, "rationale": "ok"}`))
	if err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	if res.Score == nil || *res.Score != 0.5 {
		t.Fatalf("unexpected result: %+v", res)
	}
}
