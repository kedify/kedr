// Package config defines and validates kedr's command configuration.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Secret string

func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte("null"), nil
	}
	return []byte(`"**********"`), nil
}

func (s Secret) MarshalYAML() (any, error) {
	if s == "" {
		return nil, nil
	}
	return "**********", nil
}

type Config struct {
	Quiet                     bool              `json:"quiet" yaml:"quiet"`
	Verbose                   bool              `json:"verbose" yaml:"verbose"`
	Clusters                  any               `json:"clusters" yaml:"clusters"`
	ClusterValues             []string          `json:"-" yaml:"-"`
	AllClusters               bool              `json:"-" yaml:"-"`
	Kubeconfig                *string           `json:"kubeconfig" yaml:"kubeconfig"`
	ImpersonateUser           *string           `json:"impersonate_user" yaml:"impersonate_user"`
	ImpersonateGroup          *string           `json:"impersonate_group" yaml:"impersonate_group"`
	Namespaces                any               `json:"namespaces" yaml:"namespaces"`
	NamespaceValues           []string          `json:"-" yaml:"-"`
	Resources                 any               `json:"resources" yaml:"resources"`
	ResourceValues            []string          `json:"-" yaml:"-"`
	Selector                  *string           `json:"selector" yaml:"selector"`
	CPUMinValue               int               `json:"cpu_min_value" yaml:"cpu_min_value"`
	MemoryMinValue            int               `json:"memory_min_value" yaml:"memory_min_value"`
	PrometheusURL             *string           `json:"prometheus_url" yaml:"prometheus_url"`
	PrometheusAuthHeader      Secret            `json:"prometheus_auth_header" yaml:"prometheus_auth_header"`
	PrometheusOtherHeaders    map[string]Secret `json:"prometheus_other_headers" yaml:"prometheus_other_headers"`
	PrometheusSSLEnabled      bool              `json:"prometheus_ssl_enabled" yaml:"prometheus_ssl_enabled"`
	PrometheusClusterLabel    *string           `json:"prometheus_cluster_label" yaml:"prometheus_cluster_label"`
	PrometheusLabel           *string           `json:"prometheus_label" yaml:"prometheus_label"`
	EKSManagedProm            bool              `json:"eks_managed_prom" yaml:"eks_managed_prom"`
	EKSManagedPromProfileName *string           `json:"eks_managed_prom_profile_name" yaml:"eks_managed_prom_profile_name"`
	EKSAccessKey              *string           `json:"eks_access_key" yaml:"eks_access_key"`
	EKSSecretKey              Secret            `json:"eks_secret_key" yaml:"eks_secret_key"`
	EKSServiceName            *string           `json:"eks_service_name" yaml:"eks_service_name"`
	EKSManagedPromRegion      *string           `json:"eks_managed_prom_region" yaml:"eks_managed_prom_region"`
	EKSAssumeRole             *string           `json:"eks_assume_role" yaml:"eks_assume_role"`
	CoralogixToken            Secret            `json:"coralogix_token" yaml:"coralogix_token"`
	OpenShift                 bool              `json:"openshift" yaml:"openshift"`
	MaxWorkers                int               `json:"max_workers" yaml:"max_workers"`
	DiscoveryJobBatchSize     int               `json:"-" yaml:"-"`
	DiscoveryJobMaxBatches    int               `json:"-" yaml:"-"`
	JobGroupingLabels         []string          `json:"job_grouping_labels" yaml:"job_grouping_labels"`
	JobGroupingLimit          int               `json:"job_grouping_limit" yaml:"job_grouping_limit"`
	Format                    string            `json:"format" yaml:"format"`
	ShowClusterName           bool              `json:"show_cluster_name" yaml:"show_cluster_name"`
	Strategy                  string            `json:"strategy" yaml:"strategy"`
	LogToStderr               bool              `json:"log_to_stderr" yaml:"log_to_stderr"`
	Width                     *int              `json:"width" yaml:"width"`
	ShowSeverity              bool              `json:"show_severity" yaml:"show_severity"`
	PublishScanURL            *string           `json:"publish_scan_url" yaml:"publish_scan_url"`
	StartTime                 *string           `json:"start_time" yaml:"start_time"`
	ScanID                    *string           `json:"scan_id" yaml:"scan_id"`
	NamedSinks                []string          `json:"named_sinks" yaml:"named_sinks"`
	FileOutput                *string           `json:"file_output" yaml:"file_output"`
	FileOutputDynamic         bool              `json:"file_output_dynamic" yaml:"file_output_dynamic"`
	SlackOutput               *string           `json:"slack_output" yaml:"slack_output"`
	SlackTitle                *string           `json:"slack_title" yaml:"slack_title"`
	AzureBlobOutput           *string           `json:"azureblob_output" yaml:"azureblob_output"`
	TeamsWebhook              *string           `json:"teams_webhook" yaml:"teams_webhook"`
	AzureSubscriptionID       *string           `json:"azure_subscription_id" yaml:"azure_subscription_id"`
	AzureResourceGroup        *string           `json:"azure_resource_group" yaml:"azure_resource_group"`
	OtherArgs                 map[string]any    `json:"other_args" yaml:"other_args"`
	InsideCluster             bool              `json:"inside_cluster" yaml:"inside_cluster"`

	PrometheusHeadersRaw []string `json:"-" yaml:"-"`
	JobGroupingRaw       string   `json:"-" yaml:"-"`
	HistoryDuration      float64  `json:"-" yaml:"-"`
	TimeframeDuration    float64  `json:"-" yaml:"-"`
	CPUPercentile        float64  `json:"-" yaml:"-"`
	CPURequest           float64  `json:"-" yaml:"-"`
	CPULimit             float64  `json:"-" yaml:"-"`
	MemoryBufferPercent  float64  `json:"-" yaml:"-"`
	PointsRequired       int      `json:"-" yaml:"-"`
	AllowHPA             bool     `json:"-" yaml:"-"`
	UseOOMKillData       bool     `json:"-" yaml:"-"`
	OOMMemoryBuffer      float64  `json:"-" yaml:"-"`
}

// MarshalJSON preserves KRR's config contract, where numeric strategy
// arguments are represented as strings under other_args. Strategy settings
// remain numeric in the report's strategy section.
func (c Config) MarshalJSON() ([]byte, error) {
	type configAlias Config
	var otherArgs map[string]any
	if c.OtherArgs != nil {
		otherArgs = make(map[string]any, len(c.OtherArgs))
		for key, value := range c.OtherArgs {
			switch typed := value.(type) {
			case float64:
				otherArgs[key] = strconv.FormatFloat(typed, 'f', -1, 64)
			case int:
				otherArgs[key] = strconv.Itoa(typed)
			default:
				otherArgs[key] = value
			}
		}
	}
	return json.Marshal(&struct {
		*configAlias
		OtherArgs map[string]any `json:"other_args"`
	}{configAlias: (*configAlias)(&c), OtherArgs: otherArgs})
}

func Default(strategy string) *Config {
	service := "aps"
	return &Config{
		Clusters:               []string{},
		ClusterValues:          []string{},
		NamespaceValues:        []string{},
		ResourceValues:         []string{},
		NamedSinks:             []string{},
		CPUMinValue:            10,
		MemoryMinValue:         100,
		PrometheusOtherHeaders: map[string]Secret{},
		EKSServiceName:         &service,
		MaxWorkers:             10,
		DiscoveryJobBatchSize:  5000,
		DiscoveryJobMaxBatches: 100,
		JobGroupingLimit:       500,
		Format:                 "table",
		ShowSeverity:           true,
		Strategy:               strategy,
		OtherArgs:              map[string]any{},
		HistoryDuration:        336,
		TimeframeDuration:      1.25,
		CPUPercentile:          95,
		CPURequest:             66,
		CPULimit:               96,
		MemoryBufferPercent:    15,
		PointsRequired:         100,
		OOMMemoryBuffer:        25,
	}
}

var supportedResources = map[string]string{
	"deployment": "Deployment", "statefulset": "StatefulSet", "daemonset": "DaemonSet",
	"job": "Job", "cronjob": "CronJob", "groupedjob": "GroupedJob", "rollout": "Rollout",
}

func (c *Config) Validate() error {
	if c.CPUMinValue < 0 || c.MemoryMinValue < 0 {
		return errors.New("resource minimums cannot be negative")
	}
	if c.MaxWorkers < 1 || c.DiscoveryJobBatchSize < 1 || c.DiscoveryJobMaxBatches < 1 || c.JobGroupingLimit < 1 {
		return errors.New("worker and discovery limits must be positive")
	}
	if c.Width != nil && *c.Width < 1 {
		return errors.New("--width must be positive")
	}
	if c.PrometheusURL != nil {
		u, err := url.Parse(*c.PrometheusURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("--prometheus-url must start with https:// or http://")
		}
		trimmed := strings.TrimSuffix(*c.PrometheusURL, "/")
		c.PrometheusURL = &trimmed
	}
	if c.PrometheusClusterLabel != nil && c.PrometheusLabel == nil {
		return errors.New("--prometheus-label is required with --prometheus-cluster-label")
	}
	formats := map[string]bool{"table": true, "json": true, "yaml": true, "pprint": true, "csv": true, "csv-raw": true, "html": true}
	if !formats[c.Format] {
		return fmt.Errorf("unknown formatter %q", c.Format)
	}
	if !c.ShowSeverity && c.Format != "csv" {
		return errors.New("--exclude-severity works only with format=csv")
	}
	for i, namespace := range c.NamespaceValues {
		if strings.HasPrefix(namespace, "*") {
			return errors.New("namespace values cannot start with an asterisk (*)")
		}
		c.NamespaceValues[i] = strings.ToLower(namespace)
	}
	if len(c.NamespaceValues) == 0 || contains(c.NamespaceValues, "*") {
		c.Namespaces = "*"
	} else {
		c.Namespaces = c.NamespaceValues
	}
	if len(c.ResourceValues) == 0 || contains(c.ResourceValues, "*") {
		c.Resources = "*"
	} else {
		canonical := make([]string, 0, len(c.ResourceValues))
		for _, value := range c.ResourceValues {
			kind, ok := supportedResources[strings.ToLower(value)]
			if !ok {
				return fmt.Errorf("unsupported resource %q", value)
			}
			canonical = append(canonical, kind)
		}
		c.ResourceValues, c.Resources = canonical, canonical
	}
	if c.AllClusters {
		c.Clusters = "*"
	} else {
		c.Clusters = append([]string{}, c.ClusterValues...)
	}
	for _, raw := range c.PrometheusHeadersRaw {
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid Prometheus header %q; expected key: value", raw)
		}
		c.PrometheusOtherHeaders[strings.ToLower(strings.TrimSpace(parts[0]))] = Secret(strings.TrimSpace(parts[1]))
	}
	if c.JobGroupingRaw != "" {
		for _, label := range strings.Split(c.JobGroupingRaw, ",") {
			if label = strings.TrimSpace(label); label != "" {
				c.JobGroupingLabels = append(c.JobGroupingLabels, label)
			}
		}
	}
	if c.HistoryDuration < 1 || c.TimeframeDuration <= 0 || c.PointsRequired < 1 {
		return errors.New("invalid strategy duration or point count")
	}
	if c.MemoryBufferPercent <= 0 || c.OOMMemoryBuffer < 0 {
		return errors.New("invalid memory buffer percentage")
	}
	if c.Strategy == "simple" && (c.CPUPercentile <= 0 || c.CPUPercentile > 100) {
		return errors.New("--cpu-percentile must be in (0,100]")
	}
	if c.Strategy == "simple_limit" && (c.CPURequest <= 0 || c.CPURequest > 100 || c.CPULimit <= 0 || c.CPULimit > 100) {
		return errors.New("--cpu-request and --cpu-limit must be in (0,100]")
	}
	if c.EKSManagedProm && c.HistoryDuration*60/c.TimeframeDuration > 11000 {
		c.TimeframeDuration = c.HistoryDuration * 60 / 10000
	}
	c.OtherArgs = map[string]any{
		"history_duration": c.HistoryDuration, "timeframe_duration": c.TimeframeDuration,
		"memory_buffer_percentage": c.MemoryBufferPercent, "points_required": c.PointsRequired,
		"allow_hpa": c.AllowHPA, "use_oomkill_data": c.UseOOMKillData,
		"oom_memory_buffer_percentage": c.OOMMemoryBuffer,
	}
	if c.Strategy == "simple" {
		c.OtherArgs["cpu_percentile"] = c.CPUPercentile
	} else {
		c.OtherArgs["cpu_request"], c.OtherArgs["cpu_limit"] = c.CPURequest, c.CPULimit
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func IsNamespacePattern(value string) bool {
	return regexp.MustCompile(`[\\*|\(.*?\)|\[.*?\]|\^|\$]`).MatchString(value)
}

func (c *Config) HTTPClient() (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	if encoded := os.Getenv("CERTIFICATE"); encoded != "" {
		pem, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode CERTIFICATE: %w", decodeErr)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CERTIFICATE contains no valid PEM certificate")
		}
	}
	// KRR disables verification unless --prometheus-ssl-enabled is set; keep that CLI contract.
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: pool, InsecureSkipVerify: !c.PrometheusSSLEnabled, //nolint:gosec
	}}}, nil
}
