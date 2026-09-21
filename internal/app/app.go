// Package app orchestrates discovery, metrics collection, recommendations, and reports.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kedify/kedr/internal/config"
	kube "github.com/kedify/kedr/internal/kubernetes"
	"github.com/kedify/kedr/internal/logging"
	"github.com/kedify/kedr/internal/model"
	prom "github.com/kedify/kedr/internal/prometheus"
	"github.com/kedify/kedr/internal/recommend"
	"github.com/kedify/kedr/internal/report"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/kedr/internal/ui"
)

func Run(ctx context.Context, cfg *config.Config) error {
	log := logging.New(cfg)
	log.Infof("Running KEDR (Kubernetes Efficiency and Data-driven Recommender)")
	log.Infof("Using strategy: %s", cfg.Strategy)
	log.Infof("Using formatter: %s", cfg.Format)
	loader := kube.NewLoader(cfg)
	clusters, err := loader.Clients(ctx)
	if err != nil {
		return fmt.Errorf("could not load Kubernetes configuration: %w", err)
	}
	if len(clusters) > 1 && cfg.PrometheusURL != nil {
		return errors.New("cannot scan multiple Kubernetes contexts with one explicit Prometheus URL; select one context")
	}
	type clusterState struct {
		clients kube.Clients
		prom    *prom.Client
		objects []model.Object
	}
	states := make([]clusterState, 0, len(clusters))
	allObjects := 0
	reportErrors := []map[string]any{}
	for _, cluster := range clusters {
		endpoint := ""
		autoDiscovered := cfg.PrometheusURL == nil
		if cfg.PrometheusURL != nil {
			endpoint = *cfg.PrometheusURL
		} else {
			endpoint, err = kube.DiscoverMetricsURL(ctx, cluster)
			if err != nil {
				return err
			}
		}
		pc, createErr := prom.New(ctx, cfg, endpoint, log)
		if createErr != nil {
			return createErr
		}
		if autoDiscovered && cluster.Name != nil {
			kubeHTTP, clientErr := kube.KubernetesHTTPClient(cluster)
			if clientErr != nil {
				return clientErr
			}
			pc.UseHTTPClient(kubeHTTP)
		} else if !autoDiscovered && cluster.REST != nil {
			token := cluster.REST.BearerToken
			if token == "" && cluster.REST.BearerTokenFile != "" {
				if data, readErr := os.ReadFile(cluster.REST.BearerTokenFile); readErr == nil {
					token = string(data)
				}
			}
			pc.SetBearerToken(token)
		}
		if checkErr := pc.CheckConnection(ctx); checkErr != nil {
			return fmt.Errorf("connect to prometheus at %s: %w", endpoint, checkErr)
		}
		start, end, historyErr := pc.HistoryRange(ctx)
		if historyErr != nil {
			reportErrors = append(reportErrors, map[string]any{"name": "HistoryRangeError"})
		} else if end.Sub(start) < 3*time.Hour {
			reportErrors = append(reportErrors, map[string]any{"name": "NotEnoughHistoryAvailable", "retry_after": start.Add(time.Duration(cfg.HistoryDuration * float64(time.Hour)))})
		}
		objects, listErr := loader.List(ctx, cluster)
		if listErr != nil {
			return fmt.Errorf("list Kubernetes workloads: %w", listErr)
		}
		states = append(states, clusterState{cluster, pc, objects})
		allObjects += len(objects)
	}
	if allObjects == 0 {
		return errors.New("no objects available to scan; change the filters or verify permissions")
	}
	sem := make(chan struct{}, cfg.MaxWorkers)
	progress := ui.StartProgress(allObjects, !cfg.Quiet && isTerminal(os.Stderr))
	defer progress.Stop()
	group, groupCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	scans := make([]model.Scan, 0, allObjects)
	for stateIndex := range states {
		state := &states[stateIndex]
		for objectIndex := range state.objects {
			object := state.objects[objectIndex]
			group.Go(func() error {
				select {
				case sem <- struct{}{}:
				case <-groupCtx.Done():
					return groupCtx.Err()
				}
				defer func() { <-sem }()
				var observationErr error
				object, observationErr = loader.Observe(groupCtx, state.clients, object)
				if observationErr != nil {
					log.Warnf("identity/inventory lookup failed for %s/%s: %v", object.Namespace, object.Name, observationErr)
					object.Warnings = append(object.Warnings, "IdentityInventoryCollectionFailed")
				}
				object = state.prom.HistoricalPods(groupCtx, object)
				metrics, metricWarnings := state.prom.Gather(groupCtx, object)
				object.Warnings = append(object.Warnings, metricWarnings...)
				raw, analyzeErr := strategy.Run(cfg, metrics, object)
				if analyzeErr != nil {
					return analyzeErr
				}
				scan := recommend.Scan(object, raw)
				mu.Lock()
				scans = append(scans, scan)
				mu.Unlock()
				progress.Increment()
				return nil
			})
		}
	}
	if err = group.Wait(); err != nil {
		return err
	}
	progress.Stop()
	progress = ui.StartProgress(0, false)
	if len(scans) == 0 {
		return errors.New("no successful scans were made")
	}
	model.SortObjectsFromScans(scans)
	description := description(cfg)
	summary := map[string]any{}
	if len(states) == 1 {
		summary = states[0].prom.ClusterSummary(ctx)
	}
	result := model.Report{Scans: scans, Resources: []string{"cpu", "memory"}, Description: description, Strategy: model.StrategyData{Name: cfg.Strategy, Settings: cfg.OtherArgs}, Errors: reportErrors, ClusterSummary: summary, Config: cfg}
	result.CalculateScore()
	color := isTerminal(os.Stdout) && cfg.Format == "table"
	rendered, err := report.Render(result, cfg, color)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(os.Stdout, rendered); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if cfg.FileOutputDynamic || cfg.FileOutput != nil {
		name := ""
		if cfg.FileOutputDynamic {
			name = fmt.Sprintf("kedr-%s.%s", time.Now().Format("20060102150405"), cfg.Format)
		} else {
			name = *cfg.FileOutput
		}
		if err = os.WriteFile(filepath.Clean(name), []byte(rendered), 0o600); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
		log.Infof("Wrote report to %s", name)
	}
	return nil
}

func description(cfg *config.Config) string {
	percentile, limit := cfg.CPUPercentile, "unchanged (requests only)"
	if cfg.Strategy == "simple_limit" {
		percentile, limit = cfg.CPURequest, fmt.Sprintf("request × %g", cfg.CPULimitRatio)
	}
	description := fmt.Sprintf("[b]Kedify Recommender — %s[/b]\n\nCPU request: per-series nearest-rank P%g, maximum across replicas; limit: %s\nMemory: selected-release peak + %g%%; request/limit ratio 1\nQuery history: %g hours; minimum sizing history: %g hours; native CPU/memory scrape samples\nRetain up to %d releases: newest for sizing, previous releases for comparison only.\nShared analyzer history, coverage, freshness, inventory and material-change guards apply.", cfg.Strategy, percentile, limit, cfg.MemoryBufferPercent, cfg.HistoryDuration, cfg.MinimumHistoryHours, cfg.ReleaseHistory)
	if cfg.UseOOMKillData {
		description += "\nOOMKilled observations are included; unknown event-time limits use the analyzer's conservative fallback."
	}
	if cfg.DetectMemoryLeaks {
		description += "\nPotential memory-leak detection is enabled (advisory; does not change sizing)."
	}
	if !cfg.AllowHPA {
		description += "\nHPA-managed resources are suppressed in the report; --allow-hpa overrides this. Raw analysis remains available for auditing."
	}
	return description
}

var isTerminal = func(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}
