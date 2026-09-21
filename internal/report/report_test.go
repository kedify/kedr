package report

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/resource"
	"github.com/kedify/recommender/analysis"
)

func fixture() model.Report {
	cfg := config.Default("simple")
	_ = cfg.Validate()
	a := model.EmptyAllocations()
	a.Requests[model.CPU] = model.Number(.05)
	a.Requests[model.Memory] = model.Number(2048 * 1024 * 1024)
	rec := model.Recommendation{Requests: map[model.ResourceType]model.RecommendationValue{model.CPU: {Value: model.Number(.006), Severity: model.SeverityOK}, model.Memory: {Value: model.Number(500), Severity: model.SeverityCritical}}, Limits: map[model.ResourceType]model.RecommendationValue{model.CPU: {Value: model.Unknown(), Severity: model.SeverityUnknown}, model.Memory: {Value: model.Number(500), Severity: model.SeverityCritical}}, Info: map[model.ResourceType]*string{model.CPU: nil, model.Memory: nil}}
	cluster := "mock"
	return model.Report{Scans: []model.Scan{{Object: model.Object{Cluster: &cluster, Name: "app", Container: "main", Pods: []model.Pod{{Name: "p"}, {Name: "old", Deleted: true}}, HPA: nil, Namespace: "default", Kind: "Deployment", Allocations: a, Warnings: []string{}, Labels: nil, Annotations: nil}, Recommended: rec, Severity: model.SeverityCritical}}, Resources: []string{"cpu", "memory"}, Strategy: model.StrategyData{Name: "simple", Settings: map[string]any{}}, Errors: []map[string]any{}, ClusterSummary: map[string]any{}, Config: cfg}
}

func TestTableCPUQuantitiesAndDiffs(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		current, recommended float64
		pods                 int
		wantDiff, wantChange string
	}{
		{"multiple pods", .004, .004 + 1.0417997505975267/6, 6, "+1042m", "(+174m) 4m -> 178m"},
		{"subtraction rounding", .1, .027, 1, "-73m", "(-73m) 100m -> 27m"},
		{"fractional recommendation", .1, .02755247433231, 1, "-72m", "(-72m) 100m -> 28m"},
		{"whole cores", 2, 2 - 1.45744752566769, 1, "-1457m", "(-1457m) 2000m -> 543m"},
		{"round to nearest millicore", 2, .54255, 1, "-1457m", "(-1457m) 2000m -> 543m"},
		{"display zero", .1, .1 - .0000001, 1, "+0m", "(+0m) 100m -> 100m"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := fixture()
			scan := &report.Scans[0]
			scan.Object.Pods = append(make([]model.Pod, tt.pods), model.Pod{Deleted: true})
			scan.Object.Allocations.Requests[model.CPU] = model.Number(tt.current)
			scan.Object.Allocations.Limits[model.CPU] = model.Number(2)
			scan.Recommended.Requests[model.CPU] = model.RecommendationValue{Value: model.Number(tt.recommended)}
			scan.Recommended.Limits[model.CPU] = model.RecommendationValue{Value: model.Number(1.23456789)}
			before, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			text, err := Render(report, config.Default("simple"), false)
			if err != nil {
				t.Fatal(err)
			}
			row := firstTableRow(t, text)
			if row[7] != tt.wantDiff || row[8] != tt.wantChange || row[9] != "2000m -> 1235m" {
				t.Fatalf("unexpected CPU cells: %v", row[7:10])
			}
			if !strings.Contains(text, "total request change across current pods; parentheses: change per container") {
				t.Fatal("total versus per-container diffs not explained")
			}
			after, err := json.Marshal(report)
			if err != nil || string(after) != string(before) {
				t.Fatal("table formatting changed underlying recommendation data")
			}
			cfg := config.Default("simple")
			cfg.Format = "csv-raw"
			raw, err := Render(report, cfg, false)
			if err != nil || !strings.Contains(raw, "1.23456789") {
				t.Fatalf("raw quantities lost precision: %s, %v", raw, err)
			}
		})
	}
}

func firstTableRow(t *testing.T, text string) []string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		cells := strings.Split(line, "│")
		if len(cells) < 2 || strings.TrimSpace(cells[1]) != "1." {
			continue
		}
		cells = cells[1 : len(cells)-1]
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		return cells
	}
	t.Fatalf("missing table row in %s", text)
	return nil
}

func TestTableMemoryAndReleaseEvidencePrecision(t *testing.T) {
	report := fixture()
	report.Scans[0].Object.Allocations.Requests[model.Memory] = model.Number(100 * 1024 * 1024)
	report.Scans[0].Recommended.Requests[model.Memory] = model.RecommendationValue{Value: model.Number(183.0880859375 * 1024 * 1024)}
	report.Scans[0].ReleaseComparisons = []model.ReleaseComparison{{
		Release: model.Release{ID: "new", Current: true},
		CPU:     model.ReleaseUsage{AggregatedUsage: analysis.Signal{Available: true, Value: 1234.56789}},
		Memory:  model.ReleaseUsage{AggregatedUsage: analysis.Signal{Available: true, Value: 183.0880859375 * 1024 * 1024}},
	}}
	text, err := Render(report, config.Default("simple"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(+83Mi) 100Mi -> 183Mi", "CPU aggregate 1235m", "memory peak 183Mi"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing rounded quantity %s in %s", want, text)
		}
	}
}

func TestTableUnknownAndUnsetQuantities(t *testing.T) {
	for _, format := range []func(float64) string{resource.FormatCPU, resource.FormatMemory} {
		if value(model.Unknown(), format) != "?" || value(model.Unset(), format) != "unset" {
			t.Fatal("formatting changed unknown/unset quantities")
		}
		for _, recommended := range []model.MaybeValue{model.Unknown(), model.Unset()} {
			if diff(model.Number(1), model.RecommendationValue{Value: recommended}, 1, format) != "" {
				t.Fatal("unknown/unset recommendation produced a numeric diff")
			}
		}
	}
}

func TestJSONContract(t *testing.T) {
	cfg := config.Default("simple")
	cfg.Format = "json"
	got, err := Render(fixture(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"scans", "score", "resources", "description", "strategy", "errors", "clusterSummary", "config"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
}
func TestCSVContract(t *testing.T) {
	cfg := config.Default("simple")
	cfg.Format = "csv"
	got, err := Render(fixture(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := "Namespace,Name,Pods,Old Pods,Type,Container,Severity,CPU Diff,CPU Requests,CPU Limits,Memory Diff,Memory Requests,Memory Limits,Notes"
	if strings.Join(rows[0], ",") != want {
		t.Fatalf("headers=%s", strings.Join(rows[0], ","))
	}
	if rows[1][2] != "1" || rows[1][3] != "1" {
		t.Fatalf("pod counts=%v", rows[1])
	}
}
func TestAllFormats(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml", "pprint", "csv", "csv-raw", "html"} {
		cfg := config.Default("simple")
		cfg.Format = format
		got, err := Render(fixture(), cfg, false)
		if err != nil || got == "" {
			t.Fatalf("%s: %q %v", format, got, err)
		}
	}
}

func TestDiagnosticsVisibleInEveryFormat(t *testing.T) {
	report := fixture()
	notice := "oom-kill-detected; potential-memory-leak"
	report.Scans[0].Recommended.Info[model.Memory] = &notice
	report.Scans[0].Object.Warnings = []string{"HistoricalOwnershipUnavailable:kube_pod_owner"}
	for _, format := range []string{"table", "json", "yaml", "pprint", "csv", "csv-raw", "html"} {
		cfg := config.Default("simple")
		cfg.Format = format
		text, err := Render(report, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range []string{"oom-kill-detected", "potential-memory-leak", "HistoricalOwnershipUnavailable:kube_pod_owner"} {
			if !strings.Contains(text, message) {
				t.Fatalf("%s hides %s", format, message)
			}
		}
	}
}

func TestReleaseComparisonsAreLabeledAndPreserved(t *testing.T) {
	report := fixture()
	report.Scans[0].ReleaseComparisons = []model.ReleaseComparison{
		{Release: model.Release{ID: "new", Name: "app-new", Image: "app:fixed", Current: true}, Memory: model.ReleaseUsage{AggregatedUsage: analysis.Signal{Available: true, Value: 128 * 1024 * 1024}, HistoryHours: 192}},
		{Release: model.Release{ID: "old", Name: "app-old", Image: "app:leaky"}, Memory: model.ReleaseUsage{AggregatedUsage: analysis.Signal{Available: true, Value: 1024 * 1024 * 1024}, HistoryHours: 24}},
	}
	for _, format := range []string{"table", "csv", "csv-raw", "html", "json", "yaml", "pprint"} {
		cfg := config.Default("simple")
		cfg.Format = format
		text, err := Render(report, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, image := range []string{"app:fixed", "app:leaky"} {
			if !strings.Contains(text, image) {
				t.Fatalf("%s hides release image %s", format, image)
			}
		}
		if format == "table" || format == "csv" || format == "csv-raw" || format == "html" {
			if !strings.Contains(text, "comparison only") || !strings.Contains(text, "sizing release") {
				t.Fatalf("%s does not distinguish comparison from recommendation", format)
			}
		} else if !strings.Contains(text, "releaseComparisons") {
			t.Fatalf("%s lost comparison evidence", format)
		}
	}
}
