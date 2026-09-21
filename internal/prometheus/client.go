// Package prometheus queries Prometheus-compatible metrics services.
package prometheus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/strategy"
)

type Logger interface {
	Debugf(string, ...any)
	Warnf(string, ...any)
}

type Client struct {
	baseURL   string
	http      *http.Client
	headers   http.Header
	cfg       *config.Config
	logger    Logger
	signer    *v4.Signer
	creds     aws.CredentialsProvider
	awsRegion string
	querySem  chan struct{}
}

// UseHTTPClient replaces the transport, primarily for kube-apiserver service proxies.
func (c *Client) UseHTTPClient(client *http.Client) {
	if client != nil {
		c.http = client
	}
}

func (c *Client) SetBearerToken(token string) {
	if token != "" && c.headers.Get("Authorization") == "" {
		c.headers.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}

func New(ctx context.Context, cfg *config.Config, endpoint string, logger Logger) (*Client, error) {
	httpClient, err := cfg.HTTPClient()
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	for key, value := range cfg.PrometheusOtherHeaders {
		headers.Set(key, string(value))
	}
	if cfg.PrometheusAuthHeader != "" {
		headers.Set("Authorization", string(cfg.PrometheusAuthHeader))
	}
	if cfg.CoralogixToken != "" && headers.Get("Authorization") == "" {
		headers.Set("Authorization", "Bearer "+string(cfg.CoralogixToken))
	}
	if cfg.OpenShift && headers.Get("Authorization") == "" {
		if token, readErr := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token"); readErr == nil {
			headers.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		}
	}
	if strings.HasSuffix(strings.TrimRight(endpoint, "/"), "/prometheus") && headers.Get("X-Scope-OrgID") == "" {
		headers.Set("X-Scope-OrgID", "anonymous")
	}
	c := &Client{
		baseURL:  strings.TrimRight(endpoint, "/"),
		http:     httpClient,
		headers:  headers,
		cfg:      cfg,
		logger:   logger,
		querySem: make(chan struct{}, cfg.MaxWorkers),
	}
	if cfg.EKSManagedProm {
		opts := []func(*awsconfig.LoadOptions) error{}
		if cfg.EKSManagedPromRegion != nil {
			opts = append(opts, awsconfig.WithRegion(*cfg.EKSManagedPromRegion))
		}
		if cfg.EKSManagedPromProfileName != nil {
			opts = append(opts, awsconfig.WithSharedConfigProfile(*cfg.EKSManagedPromProfileName))
		}
		if cfg.EKSAccessKey != nil && cfg.EKSSecretKey != "" {
			opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(*cfg.EKSAccessKey, string(cfg.EKSSecretKey), "")))
		}
		awsCfg, loadErr := awsconfig.LoadDefaultConfig(ctx, opts...)
		if loadErr != nil {
			return nil, fmt.Errorf("load AWS configuration: %w", loadErr)
		}
		if awsCfg.Region == "" {
			return nil, errors.New("no AWS region configured; use --eks-managed-prom-region or an AWS profile")
		}
		provider := awsCfg.Credentials
		if cfg.EKSAssumeRole != nil {
			provider = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(awsCfg), *cfg.EKSAssumeRole))
		}
		c.signer, c.creds, c.awsRegion = v4.NewSigner(), provider, awsCfg.Region
	}
	return c, nil
}

type apiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string   `json:"resultType"`
		Result     []series `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

type series struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value"`
	Values [][]json.RawMessage `json:"values"`
}

func (c *Client) do(ctx context.Context, path string, params url.Values) ([]series, error) {
	select {
	case c.querySem <- struct{}{}:
		defer func() { <-c.querySem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	u := c.baseURL + path + "?" + params.Encode()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt+1) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header = c.headers.Clone()
		if c.signer != nil {
			creds, credErr := c.creds.Retrieve(ctx)
			if credErr != nil {
				return nil, credErr
			}
			h := sha256.Sum256(nil)
			service := "aps"
			if c.cfg.EKSServiceName != nil {
				service = *c.cfg.EKSServiceName
			}
			if err = c.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(h[:]), service, c.awsRegion, time.Now()); err != nil {
				return nil, err
			}
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if closeErr := resp.Body.Close(); readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode/100 != 2 {
			lastErr = fmt.Errorf("prometheus returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
			continue
		}
		var decoded apiResponse
		if err = json.Unmarshal(body, &decoded); err != nil {
			lastErr = err
			continue
		}
		if decoded.Status != "success" {
			lastErr = fmt.Errorf("prometheus query failed: %s", decoded.Error)
			continue
		}
		return decoded.Data.Result, nil
	}
	return nil, lastErr
}

func (c *Client) Query(ctx context.Context, query string) ([]series, error) {
	return c.do(ctx, "/api/v1/query", url.Values{"query": {query}})
}

func (c *Client) queryAt(ctx context.Context, query string, at time.Time) ([]series, error) {
	return c.do(ctx, "/api/v1/query", url.Values{"query": {query}, "time": {strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64)}})
}

// QuerySamples uses an instant range-vector selector, not query_range: every
// stored scrape (and its original timestamp) survives, regardless of query step.
func (c *Client) QuerySamples(ctx context.Context, selector string, start, end time.Time) ([]series, error) {
	return c.queryAt(ctx, fmt.Sprintf("%s[%dms]", selector, end.Sub(start).Milliseconds()), end)
}

func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]series, error) {
	return c.do(ctx, "/api/v1/query_range", url.Values{
		"query": {query}, "start": {strconv.FormatInt(start.Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)},
		"step": {strconv.FormatInt(int64(step.Seconds()), 10)},
	})
}

// CheckConnection verifies that the configured endpoint serves the Prometheus query API.
func (c *Client) CheckConnection(ctx context.Context) error {
	rows, err := c.Query(ctx, "vector(1)")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("prometheus health query returned no data")
	}
	return nil
}

func parsePair(raw []json.RawMessage) (strategy.Point, error) {
	if len(raw) != 2 {
		return strategy.Point{}, errors.New("invalid Prometheus sample")
	}
	var timestamp float64
	if err := json.Unmarshal(raw[0], &timestamp); err != nil {
		return strategy.Point{}, err
	}
	var value string
	if err := json.Unmarshal(raw[1], &value); err != nil {
		return strategy.Point{}, err
	}
	number, err := strconv.ParseFloat(value, 64)
	return strategy.Point{Time: timestamp, Value: number}, err
}

func history(cfg *config.Config) time.Duration {
	return time.Duration(cfg.HistoryDuration * float64(time.Hour))
}
func timeframe(cfg *config.Config) time.Duration {
	return time.Duration(cfg.TimeframeDuration * float64(time.Minute))
}
