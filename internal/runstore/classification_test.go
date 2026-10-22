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
