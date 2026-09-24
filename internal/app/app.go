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

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sync/errgroup"

	"github.com/kedify/kedr/internal/config"
	kube "github.com/kedify/kedr/internal/kubernetes"
	"github.com/kedify/kedr/internal/logging"
	"github.com/kedify/kedr/internal/model"
	prom "github.com/kedify/kedr/internal/prometheus"
	"github.com/kedify/kedr/internal/recommend"
	"github.com/kedify/kedr/internal/report"
	"github.com/kedify/kedr/internal/runstore"
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
		clients    kube.Clients
		prom       *prom.Client
		objects    []model.Object
		connection runstore.Connection
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
			started := time.Now()
			endpoint, err = kube.DiscoverMetricsURL(ctx, cluster)
			if err != nil {
				return err
			}
			log.Debugf("Metrics endpoint discovery: %s", time.Since(started).Round(time.Millisecond))
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
		started := time.Now()
		objects, listErr := loader.List(ctx, cluster)
		if listErr != nil {
			return fmt.Errorf("list Kubernetes workloads: %w", listErr)
		}
		log.Debugf("Workload discovery: %s (%d containers)", time.Since(started).Round(time.Millisecond), len(objects))
		apiServer := ""
		if cluster.REST != nil {
			apiServer = cluster.REST.Host
		}
		states = append(states, clusterState{cluster, pc, objects, runstore.SavedConnection(cfg, cluster.Name, endpoint, apiServer, autoDiscovered)})
		allObjects += len(objects)
	}
	if allObjects == 0 {
		return errors.New("no objects available to scan; change the filters or verify permissions")
	}
	sem := make(chan struct{}, cfg.MaxWorkers)
	if cfg.Verbose {
		log.Infof("Calculating recommendations")
	}
	progress := ui.StartProgress(allObjects, !cfg.Quiet && !cfg.Verbose && ui.IsTerminal(os.Stderr), cfg.NoColor)
	defer progress.Stop()
	group, groupCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	scans := make([]model.Scan, 0, allObjects)
	savedRows := make(map[string]runstore.Row)
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
				started := time.Now()
				var observationErr error
				object, observationErr = loader.Observe(groupCtx, state.clients, object)
				if observationErr != nil {
					log.Warnf("identity/inventory lookup failed for %s/%s: %v", object.Namespace, object.Name, observationErr)
					object.Warnings = append(object.Warnings, "IdentityInventoryCollectionFailed")
				}
				inventoryTime := time.Since(started)
				started = time.Now()
				object = state.prom.HistoricalPods(groupCtx, object)
				historyTime := time.Since(started)
				started = time.Now()
				metrics, metricWarnings := state.prom.Gather(groupCtx, object)
				metricsTime := time.Since(started)
				object.Warnings = append(object.Warnings, metricWarnings...)
				started = time.Now()
				raw, analyzeErr := strategy.Run(cfg, metrics, object)
				if analyzeErr != nil {
					return analyzeErr
				}
				log.Debugf("Scanned %s/%s/%s: inventory=%s history=%s metrics=%s analysis=%s", object.Namespace, object.Name, object.Container,
					inventoryTime.Round(time.Millisecond), historyTime.Round(time.Millisecond), metricsTime.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
				scan := recommend.Scan(object, raw)
				var saved runstore.Row
				if !cfg.NoSave {
					saved = runstore.NewRow(scan, metrics, state.connection)
				}
				mu.Lock()
				if !cfg.NoSave {
					savedRows[rowKey(object)] = saved
				}
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
	progress = ui.StartProgress(0, false, cfg.NoColor)
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
	output := &colorprofile.Writer{Forward: os.Stdout, Profile: ui.ColorProfile(os.Stdout, cfg.NoColor)}
	color := output.Profile >= colorprofile.ANSI && cfg.Format == "table"
	rendered, err := report.Render(result, cfg, color)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(output, rendered); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if cfg.FileOutputDynamic || cfg.FileOutput != nil {
		name := ""
		if cfg.FileOutputDynamic {
			name = fmt.Sprintf("kedr-%s.%s", time.Now().Format("20060102150405"), cfg.Format)
		} else {
			name = *cfg.FileOutput
		}
		fileRendered := rendered
		if cfg.Format == "table" {
			fileRendered = ansi.Strip(fileRendered)
		}
		if err = os.WriteFile(filepath.Clean(name), []byte(fileRendered), 0o600); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
		log.Infof("Wrote report to %s", name)
	}
	if !cfg.NoSave {
		rows := make([]runstore.Row, len(scans))
		for i, scan := range scans {
			rows[i] = savedRows[rowKey(scan.Object)]
		}
		store, saveErr := runstore.Default()
		var saved runstore.Run
		if saveErr == nil {
			saved, saveErr = store.Save(runstore.Run{KedrVersion: cfg.KedrVersion, Strategy: cfg.Strategy, Rows: rows})
		}
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: this scan could not be saved for kedr explain: %v\n", saveErr)
		} else if cfg.Format == "table" {
			if _, err = fmt.Fprintf(os.Stdout, "\nSaved run %s · Explain a row: kedr explain <row>\n", saved.ID); err != nil {
				return fmt.Errorf("write saved run reference: %w", err)
			}
		}
	}

	return nil
}

func description(cfg *config.Config) string {
	percentile, limit := cfg.CPUPercentile, "unchanged (requests only)"
	if cfg.Strategy == "simple_limit" {
		percentile, limit = cfg.CPURequest, fmt.Sprintf("request × %g", cfg.CPULimitRatio)
	}
	description := fmt.Sprintf("[b]Kedify Recommender — %s[/b]\n\nCPU request: per-series nearest-rank P%g, maximum across replicas; limit: %s\nMemory: selected-release peak + %g%%; request/limit ratio 1\nQuery history: %g hours; minimum sizing history: %g hours; native CPU/memory scrape samples\nRetain up to %d rollouts: use current usage, falling back through at most three previous rollouts when history or samples are insufficient.\nShared analyzer history, coverage, freshness, inventory and material-change guards apply.", cfg.Strategy, percentile, limit, cfg.MemoryBufferPercent, cfg.HistoryDuration, cfg.MinimumHistoryHours, cfg.ReleaseHistory)
	if cfg.UseOOMKillData {
		description += "\nCurrent-rollout OOM kills can trigger memory increases without usage history; unknown event-time limits fall back to current memory settings."
	}
	if cfg.DetectMemoryLeaks {
		description += "\nPotential memory-leak detection is enabled (advisory; does not change sizing)."
	}
	if !cfg.AllowHPA {
		description += "\nHPA-managed resources are suppressed in the report; --allow-hpa overrides this. Raw analysis remains available for auditing."
	}
	return description
}

// Length-prefixed components prevent collisions in multi-cluster row mapping.
func rowKey(o model.Object) string {
	cluster := ""
	if o.Cluster != nil {
		cluster = *o.Cluster
	}
	return fmt.Sprintf("%q/%q/%q/%q/%q", cluster, o.Namespace, o.Kind, o.Name, o.Container)
}
