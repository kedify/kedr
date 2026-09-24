package report

import (
	"encoding/csv"
	"encoding/json"
	"strconv"
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
		{"multiple pods", .004, .004 + 1.0417997505975267/6, 6, "+1042m", "4m -> 178m"},
		{"subtraction rounding", .1, .027, 1, "-73m", "100m -> 27m"},
		{"fractional recommendation", .1, .02755247433231, 1, "-72m", "100m -> 28m"},
		{"whole cores", 2, 2 - 1.45744752566769, 1, "-1457m", "2000m -> 543m"},
		{"round to nearest millicore", 2, .54255, 1, "-1457m", "2000m -> 543m"},
		{"display zero", .1, .1 - .0000001, 1, "", ""},
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
			cfg := config.Default("simple")
			cfg.Explain = true
			text, err := Render(report, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			row := firstTableRow(t, text)
			if row[7] != tt.wantDiff || row[8] != tt.wantChange || row[9] != "2000m -> 1235m" {
				t.Fatalf("unexpected CPU cells: %v", row[7:10])
			}
			if !strings.Contains(text, "total request change across current pods. Requests and limits are per container") || strings.Contains(text, "parentheses") {
				t.Fatal("total diffs versus per-container settings not explained")
			}
			after, err := json.Marshal(report)
			if err != nil || string(after) != string(before) {
				t.Fatal("table formatting changed underlying recommendation data")
			}
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
	cfg := config.Default("simple")
	cfg.Explain = true
	text, err := Render(report, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"100Mi -> 183Mi", "CPU aggregate 1235m", "memory peak 183Mi"} {
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

func TestTableRowVisibility(t *testing.T) {
	const mi = 1024 * 1024
	current := [4]model.MaybeValue{model.Number(2), model.Number(4), model.Number(100 * mi), model.Number(256 * mi)}
	unknown := [4]model.MaybeValue{model.Unknown(), model.Unknown(), model.Unknown(), model.Unknown()}
	for _, tc := range []struct {
		name                 string
		current, recommended [4]model.MaybeValue
		visible              bool
		cells                [4]string
	}{
		{"no evidence", current, unknown, false, [4]string{"2000m -> ?", "4000m -> ?", "100Mi -> ?", "256Mi -> ?"}},
		{"unknown current and recommendation", unknown, unknown, false, [4]string{"? -> ?", "? -> ?", "? -> ?", "? -> ?"}},
		{"unchanged", current, current, false, [4]string{}},
		{"unchanged and unknown", current, [4]model.MaybeValue{current[0], model.Unknown(), current[2], model.Unknown()}, false, [4]string{"", "4000m -> ?", "", "256Mi -> ?"}},
		{"unset unchanged", [4]model.MaybeValue{}, [4]model.MaybeValue{}, false, [4]string{}},
		{"rounded unchanged", current, [4]model.MaybeValue{model.Number(2.0001), current[1], current[2], current[3]}, false, [4]string{}},
		{"CPU request only", current, [4]model.MaybeValue{model.Number(1), current[1], current[2], current[3]}, true, [4]string{"2000m -> 1000m"}},
		{"CPU limit only", current, [4]model.MaybeValue{current[0], model.Number(3), current[2], current[3]}, true, [4]string{"", "4000m -> 3000m"}},
		{"memory request only", current, [4]model.MaybeValue{current[0], current[1], model.Number(150 * mi), current[3]}, true, [4]string{"", "", "100Mi -> 150Mi"}},
		{"memory limit only", current, [4]model.MaybeValue{current[0], current[1], current[2], model.Number(300 * mi)}, true, [4]string{"", "", "", "256Mi -> 300Mi"}},
		{"change and unknown", current, [4]model.MaybeValue{model.Number(1), model.Unknown(), model.Unknown(), model.Unknown()}, true, [4]string{"2000m -> 1000m", "4000m -> ?", "100Mi -> ?", "256Mi -> ?"}},
		{"initialize unset", [4]model.MaybeValue{}, [4]model.MaybeValue{model.Number(.01)}, true, [4]string{"unset -> 10m"}},
		{"unset differs from zero", [4]model.MaybeValue{}, [4]model.MaybeValue{model.Number(0)}, true, [4]string{"unset -> 0m"}},
		{"remove limit", current, [4]model.MaybeValue{current[0], model.Unset(), current[2], current[3]}, true, [4]string{"", "4000m -> unset"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture()
			scan := &r.Scans[0]
			scan.Object.Name = "visibility-target"
			scan.Object.Warnings = []string{"target-diagnostic"}
			for i, kind := range model.ResourceTypes {
				scan.Object.Allocations.Requests[kind], scan.Object.Allocations.Limits[kind] = tc.current[2*i], tc.current[2*i+1]
				scan.Recommended.Requests[kind] = model.RecommendationValue{Value: tc.recommended[2*i]}
				scan.Recommended.Limits[kind] = model.RecommendationValue{Value: tc.recommended[2*i+1]}
			}
			// The next row keeps number 2 even when the first row is hidden.
			r.Scans = append(r.Scans, fixture().Scans[0])
			before, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			for _, full := range []bool{false, true} {
				cfg := config.Default("simple")
				cfg.Full, cfg.Explain = full, true
				text, err := Render(r, cfg, false)
				if err != nil {
					t.Fatal(err)
				}
				want := tc.visible || full
				if strings.Contains(text, "visibility-target") != want || strings.Contains(text, "target-diagnostic") != want {
					t.Fatalf("full=%t: unexpected row or diagnostic visibility:\n%s", full, text)
				}
				rows := 0
				for _, line := range strings.Split(text, "\n") {
					cells := strings.Split(line, "│")
					if len(cells) != 15 || strings.TrimSpace(cells[1]) == "Number" {
						continue
					}
					rows++
					number := 2
					if strings.TrimSpace(cells[3]) == "visibility-target" {
						number = 1
					}
					if strings.TrimSpace(cells[1]) != strconv.Itoa(number)+"." {
						t.Fatalf("row number changed: %s", line)
					}
					if number == 1 {
						for i, col := range []int{9, 10, 12, 13} {
							if got := strings.TrimSpace(cells[col]); got != tc.cells[i] {
								t.Fatalf("full=%t column %d: got %q, want %q", full, col, got, tc.cells[i])
							}
							if i%2 == 0 && tc.cells[i] == "" && strings.TrimSpace(cells[col-1]) != "" {
								t.Fatalf("unchanged request has a diff: %s", line)
							}
						}
					}
				}
				wantRows := 1
				if want {
					wantRows++
				}
				if rows != wantRows {
					t.Fatalf("got %d table rows, want %d", rows, wantRows)
				}
			}
			// Filtering is only a table view; exports and saved scan data stay complete.
			for _, format := range []string{"json", "yaml", "pprint", "csv", "csv-raw", "html"} {
				cfg := config.Default("simple")
				cfg.Format = format
				text, err := Render(r, cfg, false)
				if err != nil || !strings.Contains(text, "visibility-target") {
					t.Fatalf("%s lost a row: %v", format, err)
				}
			}
			if !tc.visible {
				hidden := r
				hidden.Scans = r.Scans[:1]
				text, err := Render(hidden, config.Default("simple"), false)
				if err != nil || text != "No resource changes to display. Use --full to show all rows." {
					t.Fatalf("missing empty-table guidance: %q, %v", text, err)
				}
			}
			after, err := json.Marshal(r)
			if err != nil || string(after) != string(before) {
				t.Fatal("table filtering mutated the report")
			}
		})
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
		if format == "table" && !strings.HasSuffix(got, "╯") {
			t.Fatalf("default table output has trailing text: %s", got)
		}
	}
}

func TestDiagnosticsVisibility(t *testing.T) {
	report := fixture()
	notice := "oom-kill-detected; potential-memory-leak"
	report.Scans[0].Recommended.Info[model.Memory] = &notice
	report.Scans[0].Object.Warnings = []string{"HistoricalOwnershipUnavailable:kube_pod_owner"}
	for _, format := range []string{"table", "json", "yaml", "pprint", "csv", "csv-raw", "html"} {
		cfg := config.Default("simple")
		cfg.Format = format
		for _, explain := range []bool{false, true} {
			cfg.Explain = explain
			text, err := Render(report, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range []string{"oom-kill-detected", "potential-memory-leak", "HistoricalOwnershipUnavailable:kube_pod_owner"} {
				if strings.Contains(text, message) != (format != "table" || explain) {
					t.Fatalf("%s (explain=%t) has incorrect visibility for %s", format, explain, message)
				}
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
		cfg.Explain = true
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

func TestFallbackSizingSourceIsVisibleInReports(t *testing.T) {
	r := fixture()
	r.Scans[0].ReleaseComparisons = []model.ReleaseComparison{
		{Release: model.Release{ID: "current", Name: "app-current", Current: true}},
		{Release: model.Release{ID: "previous", Name: "app-previous"}, SizingResources: []model.ResourceType{model.Memory}},
	}
	r.Scans[0].Analysis = &analysis.Output{Results: []analysis.ResourceAnalysis{{Resource: analysis.ResourceMemory, RolloutFallback: &analysis.RolloutFallback{Release: "previous"}}}}
	for _, format := range []string{"table", "csv", "csv-raw", "html", "json", "yaml", "pprint"} {
		cfg := config.Default("simple")
		cfg.Format = format
		cfg.Explain = true
		text, err := Render(r, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		if format == "table" || format == "csv" || format == "csv-raw" || format == "html" {
			if !strings.Contains(text, "current release: app-current") || !strings.Contains(text, "fallback sizing release for memory: app-previous") {
				t.Fatalf("%s hid the sizing source: %s", format, text)
			}
		} else if !strings.Contains(text, "rolloutFallback") || !strings.Contains(text, "sizingResources") {
			t.Fatalf("%s lost structured fallback provenance", format)
		}
	}
}

func TestIneligibleCurrentReleaseIsNotLabeledAsSizingSource(t *testing.T) {
	r := fixture()
	r.Scans[0].ReleaseComparisons = []model.ReleaseComparison{
		{Release: model.Release{ID: "current", Name: "app-current", Current: true}},
	}
	r.Scans[0].Analysis = &analysis.Output{Results: []analysis.ResourceAnalysis{{Resource: analysis.ResourceMemory, DataQuality: analysis.DataQuality{Status: analysis.DataQualityUnavailable}}}}
	text := scanNotes(r.Scans[0], tableQuantity)
	if !strings.Contains(text, "current release: app-current") || strings.Contains(text, "sizing release") {
		t.Fatalf("ineligible rollout was labeled as a sizing source: %s", text)
	}
}
