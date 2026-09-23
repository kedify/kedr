package prometheus

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

func TestGatherSavedWindowAndAttribution(t *testing.T) {
	cfg := config.Default("simple")
	label, value := "cluster", "original-cluster"
	cfg.PrometheusLabel = &label
	cfg.PrometheusClusterLabel = &value
	client, err := New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	start, end := time.Unix(1700000000, 0), time.Unix(1700000120, 0)
	var calls atomic.Int32
	client.UseHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		query := r.URL.Query().Get("query")
		for _, want := range []string{`namespace="saved-ns"`, `container="saved-container"`, `cluster="original-cluster"`, `pod=~"saved-pod"`, `[120000ms]`} {
			if !strings.Contains(query, want) {
				t.Errorf("query lacks %s: %s", want, query)
			}
		}
		at, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
		if at != float64(end.Unix()) {
			t.Errorf("query used current time: %v", at)
		}
		// A reused pod name with a different UID must never be used for the old run.
		body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"saved-ns","pod":"saved-pod","container":"saved-container","uid":"replacement-uid","id":"replacement"},"values":[[1700000060,"99"],[1700000120,"999"]]},{"metric":{"namespace":"saved-ns","pod":"saved-pod","container":"saved-container","uid":"saved-uid","id":"saved-lifetime"},"values":[[1700000060,"10"],[1700000120,"20"]]}]}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	object := model.Object{Namespace: "saved-ns", Container: "saved-container", UID: "saved-workload", Release: "saved-release", Pods: []model.Pod{{Name: "saved-pod", UID: "saved-uid", Release: "saved-release", CreatedAt: start.UnixMilli()}}}
	metrics, _ := client.GatherWindow(context.Background(), object, start, end)
	if calls.Load() != 2 || metrics.WindowStart != start.UnixMilli() || metrics.EvaluationTime != end.UnixMilli() || len(metrics.CPU) != 1 || len(metrics.Memory) != 1 {
		t.Fatalf("saved window: %+v calls=%d", metrics, calls.Load())
	}
	if metrics.CPU[0].PodUID != "saved-uid" || metrics.CPU[0].Samples[1].Value != 20 {
		t.Fatal("replacement pod contaminated old evidence")
	}
}
func TestGatherSavedWindowPartialFailure(t *testing.T) {
	cfg := config.Default("simple")
	client, err := New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.UseHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Query().Get("query"), "cpu_usage") {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("forbidden"))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))}, nil
	})})
	_, warnings := client.GatherWindow(context.Background(), model.Object{Pods: []model.Pod{{Name: "p"}}}, time.Unix(100, 0), time.Unix(200, 0))
	if !strings.Contains(strings.Join(warnings, ","), "CPUUsageCollectionFailed") || !strings.Contains(strings.Join(warnings, ","), "NoPrometheusMemoryUsage") {
		t.Fatalf("partial failures lost: %v", warnings)
	}
}
