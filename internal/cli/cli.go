// Package cli defines the public kedr command-line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/kedify/kedr/internal/app"
	"github.com/kedify/kedr/internal/config"
)

var Version = "dev"

func Execute() int {
	root := NewRoot()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		var usage usageError
		if errors.As(err, &usage) || isParseError(err) {
			return 2
		}
		return 1
	}
	return 0
}

type usageError struct{ error }

func isParseError(err error) bool {
	text := err.Error()
	return strings.Contains(text, "unknown flag") || strings.Contains(text, "flag needs an argument") || strings.Contains(text, "unknown shorthand flag")
}

func NewRoot() *cobra.Command {
	root := &cobra.Command{Use: "kedr", Short: "Kubernetes resource recommendation CLI", Long: "KEDR analyzes Kubernetes workload usage and recommends CPU and memory requests and limits.", SilenceErrors: true, SilenceUsage: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetGlobalNormalizationFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	root.AddCommand(&cobra.Command{Use: "version", Short: "Print the kedr version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), Version)
		return err
	}})
	root.AddCommand(strategyCommand("simple"), strategyCommand("simple_limit"), explainCommand())
	return root
}

type bindings struct {
	kubeconfig, as, asGroup, promURL, promAuth, promClusterLabel, promLabel, eksProfile, eksAccess, eksSecret, eksService, eksRegion, eksRole, coralogix, selector, fileOutput string
	width                                                                                                                                                                      int
	excludeSeverity                                                                                                                                                            bool
}

func strategyCommand(name string) *cobra.Command {
	cfg := config.Default(name)
	b := &bindings{}
	cmd := &cobra.Command{Use: name, Short: "Run the " + name + " recommendation strategy", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("cpu-limit") {
			return usageError{errors.New("--cpu-limit (percentile) is no longer supported; use --cpu-limit-ratio (limit/request multiplier, default 5)")}
		}
		cfg.KedrVersion = Version
		applyPointers(cmd, cfg, b)
		if err := cfg.Validate(); err != nil {
			return usageError{err}
		}
		return app.Run(cmd.Context(), cfg)
	}}
	f := cmd.Flags()
	f.StringVarP(&b.kubeconfig, "kubeconfig", "k", "", "Path to kubeconfig file")
	f.StringVar(&b.as, "as", "", "Impersonate a Kubernetes user")
	f.StringVar(&b.asGroup, "as-group", "", "Impersonate a Kubernetes group")
	f.StringSliceVarP(&cfg.ClusterValues, "context", "c", nil, "Kubernetes context to scan (repeatable)")
	f.StringSliceVar(&cfg.ClusterValues, "cluster", nil, "Alias for --context")
	f.BoolVar(&cfg.AllClusters, "all-clusters", false, "Scan every kubeconfig context")
	f.StringSliceVarP(&cfg.NamespaceValues, "namespace", "n", nil, "Namespace or namespace regex to scan (repeatable)")
	f.StringSliceVarP(&cfg.ResourceValues, "resource", "r", nil, "Resource kind to scan (repeatable)")
	f.StringVarP(&b.selector, "selector", "l", "", "Workload label selector")
	f.StringVarP(&b.promURL, "prometheus-url", "p", "", "Prometheus URL")
	f.StringVar(&b.promAuth, "prometheus-auth-header", "", "Prometheus Authorization header")
	f.StringSliceVarP(&cfg.PrometheusHeadersRaw, "prometheus-headers", "H", nil, "Additional Prometheus header in 'key: value' form")
	f.BoolVar(&cfg.PrometheusSSLEnabled, "prometheus-ssl-enabled", false, "Verify Prometheus TLS certificates")
	f.StringVar(&b.promClusterLabel, "prometheus-cluster-label", "", "Cluster value in centralized Prometheus")
	f.StringVar(&b.promLabel, "prometheus-label", "", "Label used to differentiate clusters")
	f.BoolVar(&cfg.EKSManagedProm, "eks-managed-prom", false, "Use Amazon Managed Prometheus SigV4 authentication")
	f.StringVar(&b.eksProfile, "eks-profile-name", "", "AWS profile name")
	f.StringVar(&b.eksAccess, "eks-access-key", "", "AWS access key")
	f.StringVar(&b.eksSecret, "eks-secret-key", "", "AWS secret key")
	f.StringVar(&b.eksService, "eks-service-name", "aps", "AWS signing service name")
	f.StringVar(&b.eksRegion, "eks-managed-prom-region", "", "AWS region")
	f.StringVar(&b.eksRole, "eks-assume-role", "", "AWS role ARN to assume")
	f.StringVar(&b.coralogix, "coralogix-token", "", "Coralogix token")
	f.BoolVar(&cfg.OpenShift, "openshift", false, "Use the OpenShift service-account token for Prometheus")
	f.IntVar(&cfg.CPUMinValue, "cpu-min", 10, "Minimum recommended CPU in millicores")
	f.IntVar(&cfg.MemoryMinValue, "mem-min", 100, "Minimum recommended memory in MiB")
	f.IntVarP(&cfg.MaxWorkers, "max-workers", "w", 10, "Maximum concurrent workers")
	f.StringVar(&cfg.JobGroupingRaw, "job-grouping-labels", "", "Comma-separated labels for GroupedJob recommendations")
	f.IntVar(&cfg.JobGroupingLimit, "job-grouping-limit", 500, "Maximum jobs/pods per GroupedJob")
	f.IntVar(&cfg.DiscoveryJobBatchSize, "discovery-job-batch-size", 5000, "Kubernetes job API page size")
	f.IntVar(&cfg.DiscoveryJobMaxBatches, "discovery-job-max-batches", 100, "Maximum Kubernetes job API pages")
	f.StringVarP(&cfg.Format, "formatter", "f", "table", "Output formatter (table, json, yaml, pprint, csv, csv-raw, html)")
	f.BoolVar(&cfg.NoSave, "no-save", false, "Do not save this scan for kedr explain")
	f.BoolVar(&cfg.Explain, "explain", false, "Show explanations, diagnostics, and score after the table")
	f.BoolVar(&cfg.Full, "full", false, "Show all table rows, including unchanged and unavailable recommendations")
	f.BoolVar(&cfg.ShowClusterName, "show-cluster-name", false, "Always show cluster name")
	f.BoolVar(&b.excludeSeverity, "exclude-severity", false, "Exclude severity from CSV output")
	f.BoolVarP(&cfg.Verbose, "verbose", "v", false, "Enable verbose logging and disable progress animation")
	f.BoolVarP(&cfg.Quiet, "quiet", "q", false, "Disable logs")
	f.BoolVar(&cfg.LogToStderr, "logtostderr", false, "Write logs to stderr")
	f.IntVar(&b.width, "width", 0, "Output width")
	f.StringVar(&b.fileOutput, "fileoutput", "", "Write the report to a file")
	f.BoolVar(&cfg.FileOutputDynamic, "fileoutput-dynamic", false, "Write kedr-<timestamp>.<format> in the current directory")
	addStrategyFlags(f, cfg, name)
	return cmd
}

func addStrategyFlags(f *pflag.FlagSet, cfg *config.Config, name string) {
	f.Float64Var(&cfg.HistoryDuration, "history-duration-hours", cfg.HistoryDuration, "Prometheus history duration in hours")
	f.Float64Var(&cfg.TimeframeDuration, "timeframe-duration", 1.25, "Optional OOM query step in minutes; CPU/memory use native scrape samples")
	f.Float64Var(&cfg.MinimumHistoryHours, "minimum-history-hours", cfg.MinimumHistoryHours, "Minimum observed history required for sizing, independent of query duration")
	f.IntVar(&cfg.ReleaseHistory, "release-history", cfg.ReleaseHistory, "Total rollouts to retain (1-4): current plus up to three previous rollouts for fallback")
	f.BoolVar(&cfg.DetectMemoryLeaks, "detect-memory-leaks", false, "Enable advisory potential memory-leak detection")
	if name == "simple" {
		f.Float64Var(&cfg.CPUPercentile, "cpu-percentile", 95, "CPU recommendation percentile")
	} else {
		f.Float64Var(&cfg.CPURequest, "cpu-request", 66, "CPU request percentile")
		f.Float64Var(&cfg.CPULimitRatio, "cpu-limit-ratio", 5, "CPU limit/request multiplier (at least 1)")
		f.Float64("cpu-limit", 0, "Removed: use --cpu-limit-ratio instead of a percentile")
	}
	f.Float64Var(&cfg.MemoryBufferPercent, "memory-buffer-percentage", 15, "Memory peak buffer percentage")
	f.IntVar(&cfg.PointsRequired, "points-required", cfg.PointsRequired, "Minimum distinct observation times per resource")
	f.BoolVar(&cfg.AllowHPA, "allow-hpa", false, "Recommend resources managed by an HPA")
	f.BoolVar(&cfg.UseOOMKillData, "use-oomkill-data", false, "Include OOM-kill history")
	f.Float64Var(&cfg.OOMMemoryBuffer, "oom-memory-buffer-percentage", 25, "Memory increase percentage after OOMKilled (shared OOMKilledCoefficient)")
}

func applyPointers(cmd *cobra.Command, cfg *config.Config, b *bindings) {
	setString := func(name string, value string, target **string) {
		if cmd.Flags().Changed(name) {
			v := value
			*target = &v
		}
	}
	setString("kubeconfig", b.kubeconfig, &cfg.Kubeconfig)
	setString("as", b.as, &cfg.ImpersonateUser)
	setString("as-group", b.asGroup, &cfg.ImpersonateGroup)
	setString("selector", b.selector, &cfg.Selector)
	setString("prometheus-url", b.promURL, &cfg.PrometheusURL)
	if cmd.Flags().Changed("prometheus-auth-header") {
		cfg.PrometheusAuthHeader = config.Secret(b.promAuth)
	}
	setString("prometheus-cluster-label", b.promClusterLabel, &cfg.PrometheusClusterLabel)
	setString("prometheus-label", b.promLabel, &cfg.PrometheusLabel)
	setString("eks-profile-name", b.eksProfile, &cfg.EKSManagedPromProfileName)
	setString("eks-access-key", b.eksAccess, &cfg.EKSAccessKey)
	if cmd.Flags().Changed("eks-secret-key") {
		cfg.EKSSecretKey = config.Secret(b.eksSecret)
	}
	setString("eks-service-name", b.eksService, &cfg.EKSServiceName)
	setString("eks-managed-prom-region", b.eksRegion, &cfg.EKSManagedPromRegion)
	setString("eks-assume-role", b.eksRole, &cfg.EKSAssumeRole)
	if cmd.Flags().Changed("coralogix-token") {
		cfg.CoralogixToken = config.Secret(b.coralogix)
	}
	if cmd.Flags().Changed("width") {
		cfg.Width = &b.width
	}
	setString("fileoutput", b.fileOutput, &cfg.FileOutput)
	cfg.ShowSeverity = !b.excludeSeverity
}
