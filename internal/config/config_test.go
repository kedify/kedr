package config

import (
	"encoding/json"
	"math"
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

func TestAnalyzerPolicyMapping(t *testing.T) {
	for _, name := range []string{"simple", "simple_limit"} {
		cfg := Default(name)
		cfg.DetectMemoryLeaks = true
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		policy, err := cfg.AnalysisPolicy()
		if err != nil {
			t.Fatal(err)
		}
		if policy.Memory.LeakDetection == nil || policy.Memory.OOMKilledCoefficient != 1.25 || policy.Memory.LimitsToRequestsRatio != 1 || policy.Evidence.MinimumHistorySeconds != 7*86400 || policy.CPU.RequestsOnly != (name == "simple") {
			t.Fatalf("wrong policy: %+v", policy)
		}
		cfg.HistoryDuration = 24
		policy, _ = cfg.AnalysisPolicy()
		if policy.Evidence.MinimumHistorySeconds != 7*86400 {
			t.Fatal("short query weakened sizing guard")
		}
	}
}

func TestInvalidAnalyzerSettings(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.CPULimitRatio = .5 },
		func(c *Config) { c.MinimumHistoryHours = -1 },
		func(c *Config) { c.CPUMinValue = 0 },
		func(c *Config) { c.MemoryMinValue = 2 * 1024 * 1024 },
		func(c *Config) { c.PointsRequired = 1 },
		func(c *Config) { c.ReleaseHistory = 0 },
		func(c *Config) { c.MemoryBufferPercent = math.NaN() },
		func(c *Config) { c.HistoryDuration = math.Inf(1) },
	} {
		cfg := Default("simple_limit")
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid policy accepted: %+v", cfg)
		}
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
