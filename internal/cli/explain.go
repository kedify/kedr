package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/explain"
	"github.com/kedify/kedr/internal/runstore"
	"github.com/spf13/cobra"
)

func explainCommand() *cobra.Command { return newExplainCommand(runstore.Default) }

func newExplainCommand(openStore func() (runstore.Store, error)) *cobra.Command {
	cfg := config.Default("simple")
	b := &bindings{}
	var runID, format, output string
	var offline bool
	cmd := &cobra.Command{Use: "explain <row>", Short: "Explain one row from a saved scan, with an interactive HTML report", Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usageError{fmt.Errorf("explain requires one positive row number")}
		}
		row, err := strconv.Atoi(args[0])
		if err != nil || row < 1 {
			return usageError{fmt.Errorf("row must be a positive integer")}
		}
		return nil
	}, RunE: func(cmd *cobra.Command, args []string) error {
		if format != "html" && format != "text" {
			return usageError{fmt.Errorf("--format must be html or text")}
		}
		if format == "text" && output != "" {
			return usageError{fmt.Errorf("--output requires --format html")}
		}
		store, err := openStore()
		if err != nil {
			return err
		}
		run, err := store.Load(runID)
		if err != nil {
			return err
		}
		number, _ := strconv.Atoi(args[0])
		if number > len(run.Rows) {
			return usageError{fmt.Errorf("row %d is out of range; run %s has %d rows", number, run.ID, len(run.Rows))}
		}
		row := run.Rows[number-1]
		restoreConnection(cmd, cfg, row.Connection)
		applyPointers(cmd, cfg, b)
		if err = cfg.Validate(); err != nil {
			return usageError{err}
		}
		d := explain.Build(run, row)
		d.SetQueries(row, cfg)
		if format == "text" || offline {
			d.ChartStatus = "Offline: usage samples were not saved. Saved OOM events and reference lines are shown when available; the original decision evidence remains below."
		} else {
			fmt.Fprintln(cmd.ErrOrStderr(), "Fetching historical chart data for the saved scan window…")
			metrics, warnings, fetchErr := explain.Fetch(cmd.Context(), row, cfg)
			if cmd.Context().Err() != nil {
				return cmd.Context().Err()
			}
			if fetchErr != nil {
				d.ChartStatus = "Charts unavailable: " + fetchErr.Error()
				if len(d.OOMEvents) > 0 {
					d.ChartStatus = "Usage samples unavailable: " + fetchErr.Error() + ". Saved OOM events and allocation reference lines remain visible."
				}
				d.Warnings = append(d.Warnings, warnings...)
			} else {
				d.AddMetrics(row, metrics, warnings)
			}
		}
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), d.Text(format == "text")); err != nil {
			return err
		}
		if format == "text" {
			return nil
		}
		if output == "" {
			output = filepath.Join(store.Root, run.ID, fmt.Sprintf("explain-%d.html", number))
		}
		output, err = filepath.Abs(output)
		if err != nil {
			return err
		}
		data, err := explain.HTML(d)
		if err != nil {
			return err
		}
		if err = runstore.WritePrivate(output, data); err != nil {
			return fmt.Errorf("write HTML explanation: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), d.ChartStatus)
		link := (&url.URL{Scheme: "file", Path: output}).String()
		// Use OSC 8 only on a terminal; redirected output remains plain text.
		if f, ok := cmd.OutOrStdout().(*os.File); ok {
			if info, e := f.Stat(); e == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb" {
				fmt.Fprintf(cmd.OutOrStdout(), "\x1b]8;;%s\x1b\\Open explanation\x1b]8;;\x1b\\\n", link)
			}
		}
		// Quote control characters in the fallback path, while leaving ordinary paths readable.
		path := output
		if strings.ContainsAny(path, "\n\r\x1b\t") {
			path = strconv.Quote(path)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "HTML: %s\n%s\n", path, link)
		return err
	}}
	f := cmd.Flags()
	f.StringVar(&runID, "run", "", "Saved run ID (default: latest completed saved run)")
	f.StringVar(&format, "format", "html", "Explanation format: html or text (text uses saved evidence only)")
	f.StringVar(&output, "output", "", "HTML destination (default: inside the saved run directory)")
	f.BoolVar(&offline, "offline", false, "Use saved evidence without contacting Kubernetes or Prometheus")
	f.StringVarP(&b.kubeconfig, "kubeconfig", "k", "", "Path to kubeconfig containing the saved context")
	f.StringVar(&b.as, "as", "", "Impersonate a Kubernetes user")
	f.StringVar(&b.asGroup, "as-group", "", "Impersonate a Kubernetes group")
	f.StringVarP(&b.promURL, "prometheus-url", "p", "", "Override the saved Prometheus endpoint")
	f.StringVar(&b.promAuth, "prometheus-auth-header", "", "Prometheus Authorization header (never saved)")
	f.StringSliceVarP(&cfg.PrometheusHeadersRaw, "prometheus-headers", "H", nil, "Additional header in 'key: value' form (never saved)")
	f.BoolVar(&cfg.PrometheusSSLEnabled, "prometheus-ssl-enabled", false, "Verify Prometheus TLS certificates")
	f.StringVar(&b.promClusterLabel, "prometheus-cluster-label", "", "Cluster value in centralized Prometheus")
	f.StringVar(&b.promLabel, "prometheus-label", "", "Label used to differentiate clusters")
	f.BoolVar(&cfg.EKSManagedProm, "eks-managed-prom", false, "Use Amazon Managed Prometheus SigV4 authentication")
	f.StringVar(&b.eksProfile, "eks-profile-name", "", "AWS profile name")
	f.StringVar(&b.eksAccess, "eks-access-key", "", "AWS access key")
	f.StringVar(&b.eksSecret, "eks-secret-key", "", "AWS secret key (never saved)")
	f.StringVar(&b.eksService, "eks-service-name", "aps", "AWS signing service name")
	f.StringVar(&b.eksRegion, "eks-managed-prom-region", "", "AWS region")
	f.StringVar(&b.eksRole, "eks-assume-role", "", "AWS role ARN to assume")
	f.StringVar(&b.coralogix, "coralogix-token", "", "Coralogix token (never saved)")
	f.BoolVar(&cfg.OpenShift, "openshift", false, "Use the OpenShift service-account token for Prometheus")
	return cmd
}
func restoreConnection(cmd *cobra.Command, cfg *config.Config, c runstore.Connection) {
	cfg.Kubeconfig = c.Kubeconfig
	cfg.ImpersonateUser, cfg.ImpersonateGroup = c.ImpersonateUser, c.ImpersonateGroup
	cfg.PrometheusLabel, cfg.PrometheusClusterLabel = c.ClusterLabel, c.ClusterValue
	cfg.EKSManagedPromProfileName, cfg.EKSManagedPromRegion, cfg.EKSAssumeRole, cfg.EKSServiceName = c.Profile, c.Region, c.Role, c.Service
	cfg.UseOOMKillData = c.OOM
	cfg.TimeframeDuration = c.OOMStepMinutes
	if cfg.TimeframeDuration <= 0 {
		cfg.TimeframeDuration = 1.25
	}
	if !cmd.Flags().Changed("prometheus-ssl-enabled") {
		cfg.PrometheusSSLEnabled = c.VerifyTLS
	}
	if !cmd.Flags().Changed("eks-managed-prom") {
		cfg.EKSManagedProm = c.EKS
	}
	if !cmd.Flags().Changed("openshift") {
		cfg.OpenShift = c.OpenShift
	}
}
