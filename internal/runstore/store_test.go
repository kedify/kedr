package runstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
)

func testRow() Row {
	a := model.EmptyAllocations()
	a.Requests[model.CPU] = model.Number(.123)
	a.Requests[model.Memory] = model.Unknown()
	return NewRow(model.Scan{Object: model.Object{Name: "app", Namespace: "ns", Kind: "Deployment", Container: "main", UID: "workload-uid", Release: "release", Allocations: a, Annotations: map[string]string{"password": "secret-value"}, Pods: []model.Pod{{Name: "pod", UID: "pod-uid", Release: "release", CreatedAt: 100, EndedAt: 900, ContainerID: "lifetime", ContainerStartedAt: 120}}}}, strategy.Metrics{WindowStart: 100, EvaluationTime: 800}, Connection{})
}
func TestRoundTripPrivateSnapshot(t *testing.T) {
	store := Store{Root: filepath.Join(t.TempDir(), "runs")}
	row := testRow()
	run, err := store.Save(Run{Strategy: "simple", Rows: []Row{row}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != run.ID || got.Rows[0].Number != 1 {
		t.Fatal("row identity lost")
	}
	a := got.Rows[0].Scan.Object.Allocations
	if a.Requests[model.CPU] != model.Number(.123) || !a.Requests[model.Memory].Unknown || a.Limits[model.CPU].Set {
		t.Fatalf("allocation states lost: %+v", a)
	}
	object := got.Rows[0].Object()
	if object.UID != "workload-uid" || object.Pods[0].UID != "pod-uid" || object.Pods[0].EndedAt != 900 || object.Pods[0].ContainerID != "lifetime" {
		t.Fatalf("query identity lost: %+v", object)
	}
	path := filepath.Join(store.Root, run.ID, "run.json")
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secret-value") {
		t.Fatal("annotation persisted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file permissions: %v", info.Mode())
	}
	info, _ = os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v", info.Mode())
	}
}
func TestRetentionCorruptionAndIncompleteWrites(t *testing.T) {
	store := Store{Root: t.TempDir()}
	for i := 0; i < RetainedRuns+2; i++ {
		if _, err := store.Save(Run{Rows: []Row{testRow()}}); err != nil {
			t.Fatal(err)
		}
	}
	ids, _ := store.completed()
	if len(ids) != RetainedRuns {
		t.Fatalf("retained %d", len(ids))
	}
	unfinished := "99991231T235959.999999999Z-aaaaaaaaaaaa"
	if err := os.Mkdir(filepath.Join(store.Root, unfinished), 0700); err != nil {
		t.Fatal(err)
	}
	run, err := store.Load("")
	if err != nil || run.ID != ids[0] {
		t.Fatalf("incomplete run became latest: %+v %v", run, err)
	}
	if _, err = store.Load("../escape"); err == nil {
		t.Fatal("invalid run ID accepted")
	}
	run.SchemaVersion++
	data, _ := json.Marshal(run)
	if err = os.WriteFile(filepath.Join(store.Root, run.ID, "run.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(""); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("schema: %v", err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, run.ID, "run.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(""); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corruption: %v", err)
	}
}
func TestConnectionAllowlist(t *testing.T) {
	cfg := config.Default("simple")
	cfg.PrometheusAuthHeader = "secret-auth"
	cfg.PrometheusOtherHeaders = map[string]config.Secret{"X-Token": "secret-header"}
	cfg.EKSSecretKey = "secret-aws"
	cfg.CoralogixToken = "secret-coralogix"
	conn := SavedConnection(cfg, nil, "https://user:secret-password@host/path?token=secret-query", "https://api", false)
	data, _ := json.Marshal(conn)
	if strings.Contains(string(data), "secret-") || conn.Endpoint != "" {
		t.Fatalf("credentials leaked: %s", data)
	}
	if SafeEndpoint("https://host/api/v1/namespaces/ns/services/prom/proxy") == "" {
		t.Fatal("service proxy not retained")
	}
}
func TestDigestCanonicalAndSensitiveToSamples(t *testing.T) {
	a := analysis.Series{ID: "a", Kind: analysis.SampleCPUCounterSeconds, Samples: []analysis.Sample{{Timestamp: 1000, Value: 10}, {Timestamp: 2000, Value: 11}, {Timestamp: 3000, Value: .5}}}
	b := analysis.Series{ID: "b", Kind: analysis.SampleGauge, Samples: []analysis.Sample{{Timestamp: 1000, Value: 1}}}
	metrics := strategy.Metrics{WindowStart: 1000, EvaluationTime: 3000, CPU: []analysis.Series{a, b}}
	first, err := MetricsDigest(metrics)
	if err != nil {
		t.Fatal(err)
	}
	metrics.CPU = []analysis.Series{b, a}
	second, _ := MetricsDigest(metrics)
	if first != second {
		t.Fatal("ordering changed digest")
	}
	metrics.CPU[1].Samples[2].Value = .6
	third, _ := MetricsDigest(metrics)
	if first == third {
		t.Fatal("changed metric not detected")
	}
}
