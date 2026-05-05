package scenario

import (
	"math"
	"testing"

	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
)

func TestProbabilisticDeciderDeterministicWithSeed(t *testing.T) {
	approvals := Approvals{
		Policy: "probabilistic",
		Seed:   42,
		Rules: []ApprovalRule{{
			Match: ApprovalMatch{Host: "api.example.test"},
			Weights: map[string]float64{
				"allow_once":    0.7,
				"allow_session": 0.2,
				"deny":          0.1,
			},
		}},
		Default: "deny",
	}
	payload := runtimeproxy.RuntimeApprovalPayload{Host: "api.example.test", Method: "POST", Path: "/x"}

	a := NewProbabilisticDecider(approvals)
	b := NewProbabilisticDecider(approvals)
	for i := 0; i < 32; i++ {
		ra := a.Decide(nil, payload)
		rb := b.Decide(nil, payload)
		if ra != rb {
			t.Fatalf("seed %d not deterministic at i=%d: %q vs %q", approvals.Seed, i, ra, rb)
		}
	}
}

func TestProbabilisticDeciderHonorsWeights(t *testing.T) {
	approvals := Approvals{
		Policy: "probabilistic",
		Seed:   1, // any fixed seed; we're testing the marginal frequencies
		Rules: []ApprovalRule{{
			Match: ApprovalMatch{},
			Weights: map[string]float64{
				"allow_once": 0.8,
				"deny":       0.2,
			},
		}},
	}
	d := NewProbabilisticDecider(approvals)
	payload := runtimeproxy.RuntimeApprovalPayload{Host: "x.test", Method: "GET", Path: "/"}

	const n = 4000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		counts[d.Decide(nil, payload)]++
	}
	allowFrac := float64(counts["allow_once"]) / n
	if math.Abs(allowFrac-0.8) > 0.05 {
		t.Fatalf("allow_once fraction %.3f outside 0.8 ± 0.05 (counts %+v)", allowFrac, counts)
	}
}

func TestProbabilisticDeciderFallsBackToDefault(t *testing.T) {
	approvals := Approvals{
		Policy: "probabilistic",
		Seed:   1,
		Rules: []ApprovalRule{{
			Match: ApprovalMatch{Host: "other.test"},
			Weights: map[string]float64{"allow_once": 1},
		}},
		Default: "allow_session",
	}
	d := NewProbabilisticDecider(approvals)
	got := d.Decide(nil, runtimeproxy.RuntimeApprovalPayload{Host: "x.test"})
	if got != "allow_session" {
		t.Fatalf("non-matching payload should yield default; got %q", got)
	}
}

func TestProbabilisticDeciderEmptyWeightsUsesResolution(t *testing.T) {
	approvals := Approvals{
		Policy: "probabilistic",
		Seed:   1,
		Rules: []ApprovalRule{{
			Match:      ApprovalMatch{},
			Resolution: "allow_once",
		}},
	}
	d := NewProbabilisticDecider(approvals)
	got := d.Decide(nil, runtimeproxy.RuntimeApprovalPayload{Host: "x.test"})
	if got != "allow_once" {
		t.Fatalf("expected static Resolution when Weights empty; got %q", got)
	}
}

func TestNewDeciderSelectsByPolicy(t *testing.T) {
	if _, ok := NewDecider(Approvals{Policy: "probabilistic"}).(*ProbabilisticDecider); !ok {
		t.Fatalf("policy=probabilistic should yield ProbabilisticDecider")
	}
	if _, ok := NewDecider(Approvals{Policy: "scripted"}).(*ScriptedDecider); !ok {
		t.Fatalf("policy=scripted should yield ScriptedDecider")
	}
	if _, ok := NewDecider(Approvals{}).(*ScriptedDecider); !ok {
		t.Fatalf("empty policy should default to ScriptedDecider")
	}
}
