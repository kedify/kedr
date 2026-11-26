package config

import "testing"

func TestAnalysisPolicyAdoptsSharedSafetyGuards(t *testing.T) {
	for _, tc := range []struct {
		strategy string
		classify bool
	}{{"simple", false}, {"simple_limit", false}, {"simple_limit", true}} {
		cfg := Default(tc.strategy)
		cfg.ClassifyWorkloads = tc.classify
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		p, err := cfg.AnalysisPolicy()
		if err != nil {
			t.Fatal(err)
		}
		if !p.Memory.SafetyGuards {
			t.Fatalf("%s: memory safety guards disabled", tc.strategy)
		}
		if p.Memory.LeakDetection != nil || cfg.UseOOMKillData {
			t.Fatalf("%s: advisory leak display or OOM input forced on", tc.strategy)
		}
		if (p.Classification != nil) != tc.classify {
			t.Fatalf("%s: classification policy does not follow --classify-workloads", tc.strategy)
		}
		guard := p.CPU.LimitReductionGuard
		if tc.strategy == "simple" {
			if !p.CPU.RequestsOnly || guard != nil && guard.RequireCompleteCoverage {
				t.Fatal("simple must size CPU requests only")
			}
		} else if guard == nil || !guard.RequireCompleteCoverage {
			t.Fatalf("simple_limit (classify=%v) must require complete CFS coverage", tc.classify)
		}
	}
}
