package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestVerboseEndpoint(t *testing.T) {
	for _, tc := range []struct{ endpoint, want string }{
		{"https://cluster/api/v1/namespaces/observability/services/prometheus:80/proxy", "https://cluster/api/v1/namespaces/observability/services/prometheus:80/proxy"},
		{"http://localhost:9090", "http://localhost:9090"},
		{"https://private-user:private-password@metrics.example/prometheus?token=private-token#private-fragment", "https://metrics.example/prometheus"},
		{"https://metrics.example/prometheus?", "https://metrics.example/prometheus"},
		{"https://private-password@%invalid", "[invalid URL]"},
	} {
		for _, verbose := range []bool{false, true} {
			for _, quiet := range []bool{false, true} {
				var out bytes.Buffer
				log := &Logger{out: &out, verbose: verbose, quiet: quiet}
				log.Debugf("Prometheus URL: %s", SafeURL(tc.endpoint))
				want := ""
				if verbose && !quiet {
					want = "Prometheus URL: " + tc.want + "\n"
				}
				if got := out.String(); got != want || strings.Contains(got, "private-") {
					t.Fatalf("verbose=%t quiet=%t: got %q, want %q", verbose, quiet, got, want)
				}
			}
		}
	}
}
