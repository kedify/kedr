package prometheus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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
