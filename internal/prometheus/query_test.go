package prometheus

import (
	"context"
	"encoding/json"
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
	input := []series{{Metric: map[string]string{"pod": "p", "job": "z"}, Value: []json.RawMessage{json.RawMessage("1"), json.RawMessage(`"2"`)}}, {Metric: map[string]string{"pod": "p", "job": "kubelet"}, Value: []json.RawMessage{json.RawMessage("1"), json.RawMessage(`"3"`)}}}
	got := convertSeries(input)
	if got["p"][0].Value != 3 {
		t.Fatalf("got=%v", got)
	}
}

func TestMetricBatchesRunConcurrentlyWithinWorkerLimit(t *testing.T) {
	cfg := config.Default("simple")
	cfg.MaxWorkers = 2
	client, err := New(context.Background(), cfg, "https://prometheus.example", nil)
	if err != nil {
		t.Fatal(err)
	}

	var active, maximum atomic.Int32
	started := make(chan struct{}, 3)
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

	pods := make([]model.Pod, 101)
	for i := range pods {
		pods[i].Name = "pod-" + strconv.Itoa(i)
	}
	done := make(chan error, 1)
	go func() {
		_, loadErr := client.loadMetric(context.Background(), "MaxMemoryLoader", model.Object{Namespace: "default", Container: "app", Pods: pods})
		done <- loadErr
	}()

	for range cfg.MaxWorkers {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("metric batches did not run concurrently")
		}
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != int32(cfg.MaxWorkers) {
		t.Fatalf("maximum concurrent requests=%d, want %d", got, cfg.MaxWorkers)
	}
}
