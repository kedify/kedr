package runstore

import (
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
)

func TestClassificationDigestVersionAndThrottlingSensitivity(t *testing.T) {
	metrics := strategy.Metrics{WindowStart: 1000, EvaluationTime: 3000}
	old, err := MetricsDigest(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(old, "normalized-v1:") {
		t.Fatal("old digest changed")
	}
	metrics.ClassifyWorkloads = true
	metrics.Throttling.Seconds = []analysis.Series{{ID: "pod", Kind: analysis.SampleGauge, Samples: []analysis.Sample{{Timestamp: 1000, Value: 0}, {Timestamp: 2000, Value: 1}}}}
	first, err := MetricsDigest(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "normalized-v2:") {
		t.Fatal("classification digest version missing")
	}
	metrics.Throttling.Seconds[0].Samples[1].Value = 2
	second, err := MetricsDigest(metrics)
	if err != nil || first == second {
		t.Fatal("changed throttling not detected")
	}
}

func TestGuardedDigestIncludesCountersAndCoverage(t *testing.T) {
	var row Row
	row.Scan.Analysis = &analysis.Output{}
	row.Scan.Analysis.EffectivePolicy.CPU.LimitReductionGuard = &analysis.CPULimitReductionGuardPolicy{}
	metrics := strategy.Metrics{WindowStart: 1000, EvaluationTime: 3000}
	metrics.Throttling.Periods = []analysis.Series{{ID: "pod", Kind: analysis.SampleGauge, Samples: []analysis.Sample{{Timestamp: 1000, Value: 1}}}}
	first, err := row.MetricsDigest(metrics)
	if err != nil || !strings.HasPrefix(first, "normalized-v3:") {
		t.Fatalf("expected a guard digest, got %q: %v", first, err)
	}
	metrics.Throttling.SourceComplete = true
	second, err := row.MetricsDigest(metrics)
	if err != nil || first == second {
		t.Fatal("changed coverage did not change the guarded digest")
	}
	metrics.Throttling.Periods[0].Samples[0].Value++
	third, err := row.MetricsDigest(metrics)
	if err != nil || second == third {
		t.Fatal("changed counters did not change the guarded digest")
	}
}
