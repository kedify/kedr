package prometheus

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

func TestClassificationCollectsIndependentOptionalSignals(t *testing.T) {
	cfg := config.Default("simple")
	if len(MetricsForStrategy(cfg)) != 2 {
		t.Fatal("extra queries without opt-in")
	}
	cfg.ClassifyWorkloads = true
	names := MetricsForStrategy(cfg)
	for _, want := range []string{"OOMKilledTimestamp", "CPUThrottledSeconds", "CPUThrottledPeriods", "CPUPeriods"} {
		if !slices.Contains(names, want) {
			t.Fatalf("missing %s", want)
		}
	}
	client, err := New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.UseHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query().Get("query")
		if strings.Contains(query, "last_terminated") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"demo","pod":"pod","container":"app","uid":"pod-uid","id":"lifetime"},"values":[[1700000060,"1"],[1700000120,"3"]]}]}}`))}, nil
	})})
	object := model.Object{Namespace: "demo", Container: "app", UID: "workload", Release: "release", Pods: []model.Pod{{Name: "pod", UID: "pod-uid", Release: "release", CreatedAt: 1700000000000}}}
	metrics, _ := client.GatherWindow(context.Background(), object, time.Unix(1700000000, 0), time.Unix(1700000120, 0))
	if !metrics.ClassifyWorkloads || len(metrics.Throttling.Seconds) != 1 || len(metrics.Throttling.ThrottledPeriods) != 1 || len(metrics.Throttling.Periods) != 1 {
		t.Fatalf("throttling missing: %+v", metrics)
	}
	if metrics.Memory[0].Samples[1].Value != 3 || metrics.Throttling.Seconds[0].Samples[1].Value != 3 {
		t.Fatal("counter conversion happened in adapter")
	}
}
