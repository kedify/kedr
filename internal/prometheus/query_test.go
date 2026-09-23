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
	"github.com/kedify/recommender/analysis"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCheckConnection(t *testing.T) {
	cfg := config.Default("simple")
	client, err := New(context.Background(), cfg, "https://prometheus.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != "vector(1)" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		body := `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err = client.CheckConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMetricQueries(t *testing.T) {
	cfg := config.Default("simple")
	label, value := "cluster", "prod"
	cfg.PrometheusLabel = &label
	cfg.PrometheusClusterLabel = &value
	object := model.Object{Namespace: "default", Container: "api", Pods: []model.Pod{{Name: "api-abc"}}}
	for _, name := range MetricsForStrategy(cfg) {
		q := BuildMetricQuery(name, object, cfg)
		if q == "" || !strings.Contains(q, `namespace="default"`) || !strings.Contains(q, `cluster="prod"`) {
			t.Fatalf("%s query=%s", name, q)
		}
	}
}

func TestConvertSeriesPrefersKubelet(t *testing.T) {
	input := []sampledSeries{{labels: map[string]string{"pod": "p", "job": "z", "id": "lifetime"}, samples: []analysis.Sample{{Timestamp: 1000, Value: 2}}}, {labels: map[string]string{"pod": "p", "job": "kubelet", "id": "lifetime"}, samples: []analysis.Sample{{Timestamp: 1000, Value: 3}}}}
	object := model.Object{UID: "workload", Container: "main", Pods: []model.Pod{{Name: "p", UID: "pod", Release: "A", CreatedAt: 1}}}
	got, excluded := convertUsage(input, object, analysis.SampleGauge)
	if excluded || len(got) != 1 || got[0].Samples[0].Value != 3 {
		t.Fatalf("got=%v", got)
	}
}

func TestMetricBatchesRunConcurrentlyWithinWorkerLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pods   int
		window time.Duration
		calls  int
	}{
		{"pod batches", 101, time.Hour, 1},
		{"time chunks", 1, 600 * time.Hour, 1},
		{"shared client limit", 1, 600 * time.Hour, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default("simple")
			cfg.MaxWorkers = 2
			client, err := New(context.Background(), cfg, "https://prometheus.example", nil)
			if err != nil {
				t.Fatal(err)
			}

			var active, maximum atomic.Int32
			started := make(chan struct{}, 16)
			release := make(chan struct{})
			client.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				current := active.Add(1)
				for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
				}
				started <- struct{}{}
				<-release
				active.Add(-1)
				body := `{"status":"success","data":{"resultType":"vector","result":[]}}`
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}

			pods := make([]model.Pod, tc.pods)
			for i := range pods {
				pods[i].Name = "pod-" + strconv.Itoa(i)
			}
			done := make(chan error, tc.calls)
			for range tc.calls {
				go func() {
					now := time.Now()
					_, loadErr := client.loadMetric(context.Background(), "MemoryUsage", model.Object{Namespace: "default", Container: "app", Pods: pods}, now.Add(-tc.window), now)
					done <- loadErr
				}()
			}

			for range cfg.MaxWorkers {
				select {
				case <-started:
				case <-time.After(time.Second):
					close(release)
					t.Fatal("metric batches did not run concurrently")
				}
			}
			close(release)
			for range tc.calls {
				if err = <-done; err != nil {
					t.Fatal(err)
				}
			}
			if got := maximum.Load(); int64(got) != int64(cfg.MaxWorkers) {
				t.Fatalf("maximum concurrent requests=%d, want %d", got, cfg.MaxWorkers)
			}
		})
	}
}
