package explain

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/kedify/kedr/internal/config"
	kube "github.com/kedify/kedr/internal/kubernetes"
	prom "github.com/kedify/kedr/internal/prometheus"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/kedify/kedr/internal/strategy"
	"k8s.io/client-go/rest"
)

// Fetch never observes today's workload. Kubernetes is used only to obtain the
// saved context's transport/credentials, never to replace historical identity.
func Fetch(ctx context.Context, row runstore.Row, cfg *config.Config) (strategy.Metrics, []string, error) {
	endpoint := row.Connection.Endpoint
	if cfg.PrometheusURL != nil {
		endpoint = *cfg.PrometheusURL
	}
	if endpoint == "" {
		return strategy.Metrics{}, nil, errors.New("the saved endpoint is unavailable; supply --prometheus-url and any required credentials")
	}
	client, err := prom.New(ctx, cfg, endpoint, nil)
	if err != nil {
		return strategy.Metrics{}, nil, errors.New("could not initialize metrics authentication; check connection flags")
	}
	// A kube transport is required only for the original API proxy. An explicit
	// replacement endpoint uses explicit authentication, never kube credentials.
	sameEndpoint := endpoint == row.Connection.Endpoint
	if sameEndpoint {
		var cluster kube.Clients
		var loadErr error
		if row.Connection.Context != nil {
			cfg.ClusterValues = []string{*row.Connection.Context}
			clients, e := kube.NewLoader(cfg).Clients(ctx)
			loadErr = e
			if e == nil && len(clients) == 1 {
				cluster = clients[0]
			}
		} else {
			cluster.REST, loadErr = rest.InClusterConfig()
		}
		if loadErr == nil && cluster.REST != nil {
			if row.Connection.APIServer == "" || runstore.SafeEndpoint(cluster.REST.Host) != row.Connection.APIServer {
				if row.Connection.AutoDiscovered {
					return strategy.Metrics{}, nil, errors.New("saved Kubernetes API server does not match this context; supply --prometheus-url with explicit credentials")
				}
			} else if row.Connection.AutoDiscovered {
				httpClient, e := kube.KubernetesHTTPClient(cluster)
				if e != nil {
					return strategy.Metrics{}, nil, errors.New("could not create saved Kubernetes context transport")
				}
				client.UseHTTPClient(httpClient)
			} else {
				token := cluster.REST.BearerToken
				if token == "" && cluster.REST.BearerTokenFile != "" {
					if data, e := os.ReadFile(cluster.REST.BearerTokenFile); e == nil {
						token = string(data)
					}
				}
				client.SetBearerToken(token)
			}
		} else if row.Connection.AutoDiscovered {
			return strategy.Metrics{}, nil, errors.New("saved Kubernetes context credentials are unavailable; supply --kubeconfig or an explicit metrics endpoint")
		}
	}
	if row.WindowStart <= 0 || row.EvaluationTime <= row.WindowStart {
		return strategy.Metrics{}, nil, errors.New("saved query window is invalid")
	}
	if len(row.Identity.Pods) == 0 {
		return strategy.Metrics{}, nil, errors.New("no saved pod identities are available for historical queries")
	}
	metrics, warnings := client.GatherWindow(ctx, row.Object(), time.UnixMilli(row.WindowStart), time.UnixMilli(row.EvaluationTime))
	if err := ctx.Err(); err != nil {
		return strategy.Metrics{}, nil, err
	}
	if len(metrics.CPU) == 0 && len(metrics.Memory) == 0 && len(savedOOMEvents(row)) == 0 {
		return metrics, warnings, errors.New("no historical usage returned; verify credentials, source retention, and saved selectors")
	}
	return metrics, warnings, nil
}
