// Package runstore persists bounded, private decision snapshots, never raw samples.
package runstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
)

const SchemaVersion = 1
const RetainedRuns = 10

type Run struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"createdAt"`
	KedrVersion   string    `json:"kedrVersion"`
	Strategy      string    `json:"strategy"`
	Rows          []Row     `json:"rows"`
}

type Row struct {
	Number         int        `json:"number"`
	Scan           model.Scan `json:"scan"`
	WindowStart    int64      `json:"windowStart"`
	EvaluationTime int64      `json:"evaluationTime"`
	Digest         string     `json:"digest,omitempty"`
	Identity       Identity   `json:"identity"`
	Connection     Connection `json:"connection"`
}

// Identity contains only the hidden discovery fields needed for attribution.
type Identity struct {
	UID     string `json:"uid"`
	Release string `json:"release"`
	Pods    []Pod  `json:"pods"`
}
type Pod struct {
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Release            string `json:"release"`
	CreatedAt          int64  `json:"createdAt"`
	EndedAt            int64  `json:"endedAt"`
	ContainerStartedAt int64  `json:"containerStartedAt"`
	ContainerID        string `json:"containerID"`
}

// Connection deliberately does not contain tokens, arbitrary headers, or a Config.
type Connection struct {
	Context          *string `json:"context"`
	Kubeconfig       *string `json:"kubeconfig,omitempty"`
	APIServer        string  `json:"apiServer,omitempty"`
	Endpoint         string  `json:"endpoint,omitempty"`
	AutoDiscovered   bool    `json:"autoDiscovered"`
	ClusterLabel     *string `json:"clusterLabel,omitempty"`
	ClusterValue     *string `json:"clusterValue,omitempty"`
	VerifyTLS        bool    `json:"verifyTLS"`
	OOM              bool    `json:"oom"`
	OOMStepMinutes   float64 `json:"oomStepMinutes"`
	ImpersonateUser  *string `json:"impersonateUser,omitempty"`
	ImpersonateGroup *string `json:"impersonateGroup,omitempty"`
	EKS              bool    `json:"eks"`
	Profile          *string `json:"profile,omitempty"`
	Region           *string `json:"region,omitempty"`
	Role             *string `json:"role,omitempty"`
	Service          *string `json:"service,omitempty"`
	OpenShift        bool    `json:"openshift"`
}

// SafeEndpoint refuses credentials and query/fragment values rather than persisting
// even a partially working secret-bearing URL. Users can supply it again explicitly.
func SafeEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return u.String()
}
func SavedConnection(cfg *config.Config, context *string, endpoint, apiServer string, auto bool) Connection {
	return Connection{Context: context, Kubeconfig: cfg.Kubeconfig, APIServer: SafeEndpoint(apiServer), Endpoint: SafeEndpoint(endpoint), AutoDiscovered: auto, ClusterLabel: cfg.PrometheusLabel, ClusterValue: cfg.PrometheusClusterLabel, VerifyTLS: cfg.PrometheusSSLEnabled, OOM: cfg.UseOOMKillData, OOMStepMinutes: cfg.TimeframeDuration, ImpersonateUser: cfg.ImpersonateUser, ImpersonateGroup: cfg.ImpersonateGroup, EKS: cfg.EKSManagedProm, Profile: cfg.EKSManagedPromProfileName, Region: cfg.EKSManagedPromRegion, Role: cfg.EKSAssumeRole, Service: cfg.EKSServiceName, OpenShift: cfg.OpenShift}
}
func NewRow(scan model.Scan, metrics strategy.Metrics, conn Connection) Row {
	row := Row{Scan: scan, WindowStart: metrics.WindowStart, EvaluationTime: metrics.EvaluationTime, Identity: Identity{UID: scan.Object.UID, Release: scan.Object.Release}, Connection: conn}
	// Arbitrary annotations and labels are not required for explanations and may
	// contain credentials. Do not include them in local snapshots.
	row.Scan.Object.Labels, row.Scan.Object.Annotations = nil, nil
	for _, pod := range scan.Object.Pods {
		row.Identity.Pods = append(row.Identity.Pods, Pod{Name: pod.Name, UID: pod.UID, Release: pod.Release, CreatedAt: pod.CreatedAt, EndedAt: pod.EndedAt, ContainerStartedAt: pod.ContainerStartedAt, ContainerID: pod.ContainerID})
	}
	row.Digest, _ = MetricsDigest(metrics)
	return row
}
func (r Row) Object() model.Object {
	o := r.Scan.Object
	o.UID, o.Release = r.Identity.UID, r.Identity.Release
	o.Pods = make([]model.Pod, len(r.Identity.Pods))
	for i, pod := range r.Identity.Pods {
		o.Pods[i] = model.Pod{Name: pod.Name, UID: pod.UID, Release: pod.Release, CreatedAt: pod.CreatedAt, EndedAt: pod.EndedAt, ContainerStartedAt: pod.ContainerStartedAt, ContainerID: pod.ContainerID}
	}
	return o
}

type Store struct{ Root string }

func Default() (Store, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return Store{}, err
	}
	return Store{Root: filepath.Join(root, "kedr", "runs")}, nil
}

var validID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{12}$`)

func (s Store) Save(run Run) (Run, error) {
	run.SchemaVersion = SchemaVersion
	run.CreatedAt = time.Now().UTC()
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return Run{}, err
	}
	run.ID = run.CreatedAt.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(nonce)
	for i := range run.Rows {
		run.Rows[i].Number = i + 1
	}
	if len(run.Rows) == 0 {
		return Run{}, errors.New("cannot save an empty run")
	}
	data, err := json.Marshal(run)
	if err != nil {
		return Run{}, err
	}
	if err = os.MkdirAll(s.Root, 0700); err != nil {
		return Run{}, err
	}
	dir := filepath.Join(s.Root, run.ID)
	if err = os.Mkdir(dir, 0700); err != nil {
		return Run{}, err
	}
	if err = WritePrivate(filepath.Join(dir, "run.json"), data); err != nil {
		_ = os.RemoveAll(dir)
		return Run{}, err
	}
	// Committed run files are the index. Selecting by their immutable completion
	// timestamp avoids a shared latest pointer racing between simultaneous writers.
	s.prune()
	return run, nil
}
func (s Store) completed() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && validID.MatchString(e.Name()) {
			if _, err := os.Stat(filepath.Join(s.Root, e.Name(), "run.json")); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}
func (s Store) prune() {
	ids, err := s.completed()
	if err != nil {
		return
	}
	for _, id := range ids[min(RetainedRuns, len(ids)):] {
		_ = os.RemoveAll(filepath.Join(s.Root, id))
	}
}
func (s Store) Load(id string) (Run, error) {
	if id == "" {
		ids, err := s.completed()
		if errors.Is(err, os.ErrNotExist) || err == nil && len(ids) == 0 {
			return Run{}, errors.New("no saved scan; run kedr simple or kedr simple_limit first")
		}
		if err != nil {
			return Run{}, err
		}
		id = ids[0]
	}
	if !validID.MatchString(id) {
		return Run{}, errors.New("invalid run ID")
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return Run{}, fmt.Errorf("open saved runs: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Join(id, "run.json"))
	if err != nil {
		return Run{}, fmt.Errorf("read saved run %s: %w", id, err)
	}
	var run Run
	if err = json.Unmarshal(data, &run); err != nil {
		return Run{}, fmt.Errorf("corrupt saved run %s: %w", id, err)
	}
	if run.SchemaVersion != SchemaVersion {
		return Run{}, fmt.Errorf("unsupported saved-run schema %d; run a new scan", run.SchemaVersion)
	}
	if run.ID != id || len(run.Rows) == 0 {
		return Run{}, errors.New("corrupt saved run identity or rows")
	}
	for i, r := range run.Rows {
		if r.Number != i+1 {
			return Run{}, errors.New("corrupt saved row mapping")
		}
	}
	return run, nil
}

// WritePrivate atomically replaces a file with mode 0600, including when an old
// destination is a symlink or has broader permissions.
func WritePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".kedr-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
