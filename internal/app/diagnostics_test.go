package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/model"
)

type diagnosticLog struct{ warnings, debug []string }

func (l *diagnosticLog) Warnf(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}
func (l *diagnosticLog) Debugf(format string, args ...any) {
	l.debug = append(l.debug, fmt.Sprintf(format, args...))
}

func diagnosticScan(context, kind string, warnings ...string) model.Scan {
	return model.Scan{Object: model.Object{
		Cluster: &context, Namespace: "ns", Name: "app", Container: "main", Kind: kind,
		Pods: []model.Pod{{Name: "pod"}}, Warnings: warnings,
	}}
}

func TestScanWideMissingMetrics(t *testing.T) {
	ownership := []string{"HistoricalOwnershipUnavailable:kube_replicaset_owner", "HistoricalOwnershipUnavailable:kube_pod_owner"}
	usage := []string{"NoPrometheusCPUUsage", "NoPrometheusMemoryUsage"}
	scans := []model.Scan{
		diagnosticScan("empty", "Deployment", append(append([]string{}, ownership...), usage...)...),
		diagnosticScan("empty", "StatefulSet", append([]string{ownership[1]}, usage...)...),
		diagnosticScan("empty", "CronJob", usage...),
		diagnosticScan("empty", model.StandalonePodKind, usage...),
		diagnosticScan("healthy", "Deployment"),
	}
	scans[0].Object.Warnings = append(scans[0].Object.Warnings, "UnrelatedDiagnostic")
	before, err := json.Marshal(scans)
	if err != nil {
		t.Fatal(err)
	}
	log := &diagnosticLog{}
	endpoint := "https://metrics.example/prometheus"
	errors := logScanDiagnostics(log, scans, map[string]string{"empty": endpoint})
	if len(log.warnings) != 1 || len(errors) != 1 || errors[0]["name"] != "RequiredMetricsMissing" || errors[0]["context"] != "empty" || errors[0]["endpoint"] != endpoint {
		t.Fatalf("expected one warning for the affected context: %+v, %+v", log, errors)
	}
	for _, want := range []string{endpoint, "kube_replicaset_owner", "kube_pod_owner", "container_cpu_usage_seconds_total", "container_memory_working_set_bytes", "kube-state-metrics is installed", "scrape it", "kubelet/cAdvisor", "remote-writes", "tenant", "filters"} {
		if !strings.Contains(log.warnings[0], want) {
			t.Errorf("missing %q in %s", want, log.warnings[0])
		}
	}
	if len(log.debug) != 1 || log.debug[0] != "Diagnostics for ns/app/main: UnrelatedDiagnostic" {
		t.Fatalf("shared codes should be summarized and unrelated diagnostics retained: %+v", log.debug)
	}
	after, err := json.Marshal(scans)
	if err != nil || string(before) != string(after) {
		t.Fatal("summarizing diagnostics modified saved scan evidence")
	}
}

func TestMissingMetricDiagnosisScope(t *testing.T) {
	const cpu = "NoPrometheusCPUUsage"
	const pod = "HistoricalOwnershipUnavailable:kube_pod_owner"
	const replica = "HistoricalOwnershipUnavailable:kube_replicaset_owner"
	for _, tc := range []struct {
		name  string
		scans []model.Scan
		want  []string
	}{
		{"empty scan", nil, nil},
		{"healthy", []model.Scan{diagnosticScan("c", "Deployment")}, nil},
		{"isolated workload gap", []model.Scan{diagnosticScan("c", "Deployment", cpu, pod, replica), diagnosticScan("c", "Deployment")}, nil},
		{"KSM only", []model.Scan{diagnosticScan("c", "Deployment", pod, replica), diagnosticScan("c", "DaemonSet", pod)}, []string{"kube_replicaset_owner", "kube_pod_owner"}},
		{"CPU only", []model.Scan{diagnosticScan("c", "Deployment", cpu), diagnosticScan("c", model.StandalonePodKind, cpu)}, []string{"container_cpu_usage_seconds_total"}},
		{"query failed", []model.Scan{diagnosticScan("c", "Deployment", "CPUUsageCollectionFailed", "HistoricalMetadataCollectionFailed:kube_pod_owner")}, nil},
		{"attribution failed", []model.Scan{diagnosticScan("c", "Deployment", cpu, "CPUUsageUnattributedSeries", "NoPrometheusMemoryUsage", "MemoryUsageUnattributedSeries")}, nil},
		{"mixed missing and rejected", []model.Scan{diagnosticScan("c", "Deployment", cpu), diagnosticScan("c", "Deployment", cpu, "CPUUsageUnattributedSeries")}, nil},
		{"no pods", []model.Scan{{Object: model.Object{Kind: "Deployment", Warnings: []string{cpu, pod, replica}}}}, nil},
		{"release metadata without pods", []model.Scan{{Object: model.Object{Kind: "Deployment", Releases: []model.Release{{ID: "release"}}, Warnings: []string{cpu, pod, replica}}}}, []string{"kube_replicaset_owner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := commonMissingMetrics(tc.scans)
			got := make([]string, 0, len(metrics))
			for _, metric := range metrics {
				got = append(got, metric.name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			log := &diagnosticLog{}
			_ = logScanDiagnostics(log, tc.scans, nil)
			if tc.name == "CPU only" && (len(log.warnings) != 1 || strings.Contains(log.warnings[0], "is installed") || !strings.Contains(log.warnings[0], "kubelet/cAdvisor")) {
				t.Fatalf("CPU metrics incorrectly blamed on missing KSM: %+v", log.warnings)
			}
			if tc.name == "isolated workload gap" && (len(log.warnings) != 0 || len(log.debug) != 1 || !strings.Contains(log.debug[0], cpu)) {
				t.Fatalf("isolated problem should retain per-workload diagnostics: %+v", log)
			}
		})
	}
}
