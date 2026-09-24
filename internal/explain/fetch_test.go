package explain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/runstore"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestFetchSelectedMetricsEndpoint(t *testing.T) {
	const proxyPath = "/api/v1/namespaces/observability/services/prometheus:80/proxy"
	const testToken = "test-kubernetes-token" // #nosec G101 -- Synthetic credential for a local TLS test server.
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		wantAuth := ""
		if strings.HasPrefix(r.URL.Path, proxyPath) {
			wantAuth = "Bearer " + testToken
		}
		if r.Header.Get("Authorization") != wantAuth {
			t.Errorf("incorrect authentication for %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	contextName := "saved-context"
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	raw := clientcmdapi.Config{
		CurrentContext: contextName,
		Contexts:       map[string]*clientcmdapi.Context{contextName: {Cluster: "cluster", AuthInfo: "user"}},
		Clusters:       map[string]*clientcmdapi.Cluster{"cluster": {Server: server.URL, InsecureSkipTLSVerify: true}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"user": {Token: testToken}},
	}
	if err := clientcmd.WriteToFile(raw, kubeconfig); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		auto       bool
	}{
		{"selected service proxy", proxyPath, true},
		{"explicit service proxy", proxyPath, false},
		{"selected ingress", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, row, _ := oomFixture(t)
			row.Connection = runstore.Connection{Context: &contextName, APIServer: server.URL, Endpoint: server.URL + tc.path, AutoDiscovered: tc.auto}
			cfg := config.Default("simple")
			cfg.Kubeconfig = &kubeconfig
			if tc.path == "" {
				// Public ingress access must not depend on the saved context's credentials.
				missing := filepath.Join(t.TempDir(), "missing-kubeconfig")
				cfg.Kubeconfig = &missing
			}
			before := calls.Load()
			if _, _, err := Fetch(context.Background(), row, cfg); err != nil || calls.Load() == before {
				t.Fatalf("could not fetch from selected endpoint: %v", err)
			}
		})
	}
}
