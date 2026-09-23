package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

// A full 14-day history for one series at a 15-second scrape interval.
func BenchmarkNativeHistoryDecode(b *testing.B) {
	var body strings.Builder
	body.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"p","id":"lifetime"},"values":[`)
	for i := range 14 * 24 * 60 * 4 {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `[%d.125,"1234.5678"]`, 1800000000+i*15)
	}
	body.WriteString(`]}]}}`)
	data := []byte(body.String())
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var response apiResponse
		if err := json.Unmarshal(data, &response); err != nil {
			b.Fatal(err)
		}
		if _, err := nativeSamples(response.Data.Result); err != nil {
			b.Fatal(err)
		}
	}
}

// Isolate round-trip scheduling from server work and network variance.
func BenchmarkHistoryRoundTrips(b *testing.B) {
	cfg := config.Default("simple")
	client, err := New(context.Background(), cfg, "https://metrics.example", nil)
	if err != nil {
		b.Fatal(err)
	}
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-time.After(time.Millisecond):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"result":[]}}`))}, nil
	})}
	object := model.Object{Namespace: "ns", Container: "app", Pods: []model.Pod{{Name: "p"}}}
	start := time.Unix(1800000000, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, metric := range MetricsForStrategy(cfg) {
			if _, err := client.loadMetric(context.Background(), metric, object, start, start.Add(14*24*time.Hour)); err != nil {
				b.Fatal(err)
			}
		}
	}
}
