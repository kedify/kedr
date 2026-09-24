package ui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestChooseMetricsEndpoint(t *testing.T) {
	options := []string{"Mimir — ns/mimir\n     https://mimir.example/prometheus", "Prometheus — ns/prom\n     https://prometheus.example"}
	for _, tc := range []struct {
		name, input string
		interactive bool
		options     []string
		want        int
		errorText   string
	}{
		{"choose Prometheus", "2\n", true, options, 1, ""},
		{"choose Mimir", " 1 \r\n", true, options, 0, ""},
		{"retry invalid and empty", "\n0\n3\nbad\n2\n", true, options, 1, ""},
		{"cancel", "q\n", true, options, -1, "cancelled"},
		{"EOF", "", true, options, -1, "no metrics endpoint selected"},
		{"invalid then EOF", "0\n", true, options, -1, "no metrics endpoint selected"},
		{"noninteractive", "2\n", false, options, -1, "--prometheus-url"},
		{"single endpoint", "", false, options[:1], 0, ""},
		{"none", "", false, nil, -1, "no metrics endpoints"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			input := strings.NewReader(tc.input)
			got, err := chooseMetricsEndpoint(context.Background(), "my-context", tc.options, input, &out, tc.interactive)
			if got != tc.want || (err == nil) != (tc.errorText == "") || err != nil && !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("got %d, %v; want %d, %q", got, err, tc.want, tc.errorText)
			}
			text := out.String()
			if !tc.interactive && (text != "" || input.Len() != len(tc.input)) {
				t.Fatal("noninteractive selection wrote a prompt or consumed stdin")
			}
			if tc.name == "noninteractive" {
				text = err.Error()
			}
			if len(tc.options) > 1 {
				for _, want := range []string{"my-context", "1) Mimir", "2) Prometheus", "https://mimir.example/prometheus", "https://prometheus.example"} {
					if !strings.Contains(text, want) {
						t.Errorf("missing %q in %s", want, text)
					}
				}
			}
		})
	}
}

func TestEndpointSelectionCancellation(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := &promptSignal{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := chooseMetricsEndpoint(ctx, "cluster", []string{"Mimir", "Prometheus"}, reader, ready, true)
		done <- err
	}()
	select {
	case <-ready.ready:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left the prompt waiting for input")
	}
}

type promptSignal struct{ ready chan struct{} }

func (p *promptSignal) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "Choose an endpoint") {
		close(p.ready)
	}
	return len(data), nil
}
