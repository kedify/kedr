package report

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
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
	want := "Namespace,Name,Pods,Old Pods,Type,Container,Severity,CPU Diff,CPU Requests,CPU Limits,Memory Diff,Memory Requests,Memory Limits"
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
