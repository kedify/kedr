package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateDefaultsAndAliases(t *testing.T) {
	cfg := Default("simple")
	cfg.NamespaceValues = []string{"Default"}
	cfg.ResourceValues = []string{"deployment", "ROLLOUT"}
	cfg.PrometheusHeadersRaw = []string{"X-Tenant: acme"}
	cfg.JobGroupingRaw = "app, team"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.NamespaceValues[0]; got != "default" {
		t.Fatalf("namespace=%q", got)
	}
	if got := strings.Join(cfg.ResourceValues, ","); got != "Deployment,Rollout" {
		t.Fatalf("resources=%q", got)
	}
	if got := cfg.PrometheusOtherHeaders["x-tenant"]; got != "acme" {
		t.Fatalf("header=%q", got)
	}
	if got := strings.Join(cfg.JobGroupingLabels, ","); got != "app,team" {
		t.Fatalf("labels=%q", got)
	}
}

func TestUnsupportedWorkloads(t *testing.T) {
	for _, kind := range []string{"StrimziPodSet", "DeploymentConfig"} {
		cfg := Default("simple")
		cfg.ResourceValues = []string{kind}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected %s to fail", kind)
		}
	}
}

func TestAllClustersAndSecretSerialization(t *testing.T) {
	cfg := Default("simple")
	cfg.AllClusters = true
	cfg.PrometheusAuthHeader = "secret"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"clusters":"*"`) {
		t.Fatalf("wildcard missing: %s", text)
	}
	if strings.Contains(text, `:"secret"`) || !strings.Contains(text, "**********") {
		t.Fatalf("secret not redacted: %s", text)
	}
}

func TestEmptySlicesSerializeAsArrays(t *testing.T) {
	cfg := Default("simple")
	// Slice flag bindings use nil when the flag is not provided.
	cfg.ClusterValues = nil
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, field := range []string{`"clusters":[]`, `"named_sinks":[]`} {
		if !strings.Contains(text, field) {
			t.Fatalf("missing %s: %s", field, text)
		}
	}
	for _, field := range []string{`"history_duration":"336"`, `"points_required":"100"`, `"allow_hpa":false`} {
		if !strings.Contains(text, field) {
			t.Fatalf("missing KRR-compatible other_args field %s: %s", field, text)
		}
	}
	for _, field := range []string{"discovery_job_batch_size", "discovery_job_max_batches"} {
		if strings.Contains(text, field) {
			t.Fatalf("KEDR-only field %s leaked into config JSON: %s", field, text)
		}
	}
}

func TestExcludeSeverityOnlyCSV(t *testing.T) {
	cfg := Default("simple")
	cfg.ShowSeverity = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
	cfg.Format = "csv"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEKSQueryStepCompatibility(t *testing.T) {
	cfg := Default("simple")
	cfg.EKSManagedProm = true
	cfg.TimeframeDuration = 1
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.HistoryDuration*60/cfg.TimeframeDuration > 10000.001 {
		t.Fatalf("query step was not adjusted: %g", cfg.TimeframeDuration)
	}
}
