package report

import (
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
)

func TestFeaturesColumnAndTaggedUnchangedShortHistoryRow(t *testing.T) {
	report := fixture()
	scan := &report.Scans[0]
	for _, r := range model.ResourceTypes {
		scan.Recommended.Requests[r] = model.RecommendationValue{Value: scan.Object.Allocations.Requests[r]}
		scan.Recommended.Limits[r] = model.RecommendationValue{Value: scan.Object.Allocations.Limits[r]}
	}
	o := scan.Object
	scan.Analysis = &analysis.Output{Results: []analysis.ResourceAnalysis{{DataQuality: analysis.DataQuality{Status: analysis.DataQualityUnavailable, Reasons: []analysis.Reason{analysis.ReasonInsufficientHistory}}}}, Classifications: []analysis.WorkloadClassification{{Target: analysis.Target{Namespace: o.Namespace, Kind: o.Kind, Name: o.Name, Container: o.Container}, Features: []analysis.WorkloadFeature{analysis.FeatureLeaderElection, analysis.FeatureMetrics}}}}
	cfg := config.Default("simple")
	cfg.ClassifyWorkloads = true
	text, err := Render(report, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Features") || !strings.Contains(text, "leader-election, has-metrics") || !strings.Contains(text, "classification findings remain visible") {
		t.Fatalf("classification hidden: %s", text)
	}
	if strings.Contains(text, "Potential Mem Leak") || strings.Contains(text, "OOMKilled") {
		t.Fatal("classification enabled old columns")
	}
	cfg.ClassifyWorkloads = false
	text, err = Render(report, cfg, false)
	if err != nil || strings.Contains(text, "Features") || strings.Contains(text, "has-metrics") {
		t.Fatalf("column not gated: %s %v", text, err)
	}
}
