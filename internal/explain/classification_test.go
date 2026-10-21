package explain

import (
	"strings"
	"testing"

	"github.com/kedify/recommender/analysis"
)

func TestSavedClassificationInOfflineExplanation(t *testing.T) {
	run, row, _ := fixture(t)
	o := row.Scan.Object
	rate := .5
	percentage := 75.0
	row.Scan.Analysis.Classifications = []analysis.WorkloadClassification{{Target: analysis.Target{Namespace: o.Namespace, Kind: o.Kind, Name: o.Name, Container: o.Container}, Features: []analysis.WorkloadFeature{analysis.FeatureCPUThrottled}, Findings: []analysis.FeatureFinding{{Feature: analysis.FeatureCPUThrottled, Status: analysis.ClassificationDetected, Throttling: []analysis.CPUThrottlingEvidence{{SeriesID: "pod", SecondsPerSecond: &rate, ThrottledPeriodPercent: &percentage}}}}}}
	d := Build(run, row)
	if text := d.Text(true); !strings.Contains(text, "cpu-throttled") || !strings.Contains(text, "75.00% quota periods throttled") {
		t.Fatalf("offline explanation: %s", text)
	}
	html, err := HTML(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "Workload features") || !strings.Contains(string(html), `class="badge">cpu-throttled`) {
		t.Fatal("HTML tags missing")
	}
}
