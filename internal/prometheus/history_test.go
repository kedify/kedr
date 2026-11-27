package prometheus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

func metadata(labels map[string]string, value float64) series {
	return series{Metric: labels, Value: pair(1800000000, strconv.FormatFloat(value, 'f', -1, 64))}
}

func historyClient(t *testing.T, cfg *config.Config, data map[string][]series) *Client {
	t.Helper()
	client, err := New(context.Background(), cfg, "https://prometheus.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query().Get("query")
		if r.URL.Path != "/api/v1/query" || !strings.Contains(query, `namespace="ns"`) || r.URL.Query().Get("time") == "" {
			t.Errorf("unscoped historical metadata query: %s", r.URL)
		}
		var result []series
		for metric, rows := range data {
			if strings.HasPrefix(query, "max_over_time("+metric+"{") {
				result = rows
			}
		}
		body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	return client
}

func TestHistoricalOwnershipIncludesDeletedPodsAndReplicaSets(t *testing.T) {
	now := time.Now().UTC()
	data := map[string][]series{}
	for i, name := range []string{"ancient", "third", "older", "previous", "current"} {
		created := now.Add(-time.Duration(14-i*2) * 24 * time.Hour)
		data["kube_replicaset_owner"] = append(data["kube_replicaset_owner"], metadata(map[string]string{"namespace": "ns", "replicaset": name, "uid": "rs-" + name, "owner_uid": "current-workload-uid", "owner_kind": "Deployment", "owner_name": "app"}, 1))
		data["kube_replicaset_created"] = append(data["kube_replicaset_created"], metadata(map[string]string{"replicaset": name, "uid": "rs-" + name}, float64(created.Unix())))
		labels := map[string]string{"pod": name + "-deleted", "uid": name + "-uid", "owner_name": name, "owner_uid": "rs-" + name}
		data["kube_pod_owner"] = append(data["kube_pod_owner"], metadata(labels, 1), metadata(labels, 1)) // Duplicate KSM replicas.
		data["kube_pod_created"] = append(data["kube_pod_created"], metadata(labels, float64(created.Unix())))
		data["kube_pod_container_info"] = append(data["kube_pod_container_info"], metadata(map[string]string{"pod": name + "-deleted", "uid": name + "-uid", "image": "app:" + name}, 1))
	}
	// Current live pod is also in KSM: it must not become a duplicate old pod.
	data["kube_pod_owner"] = append(data["kube_pod_owner"], metadata(map[string]string{"pod": "current-live", "uid": "live", "owner_name": "current"}, 1))
	object := model.Object{Name: "app", Namespace: "ns", Kind: "Deployment", Container: "main", UID: "current-workload-uid", Release: "hash:current", Image: "app:current",
		Pods:     []model.Pod{{Name: "current-live", UID: "live", Release: "hash:current", CreatedAt: now.Add(-time.Hour).UnixMilli(), Image: "app:current"}},
		Releases: []model.Release{{ID: "hash:current", Name: "current", OwnerUID: "rs-current", Revision: 4}},
	}
	got := historyClient(t, config.Default("simple"), data).HistoricalPods(context.Background(), object)
	if len(got.Warnings) != 0 || got.CurrentPods() != 1 || got.DeletedPods() != 4 || len(got.Releases) != 4 {
		t.Fatalf("historical membership lost/duplicated: %+v", got)
	}
	if !got.Releases[0].Current || got.Releases[0].ID != object.Release || got.Releases[1].Name != "previous" || got.Releases[2].Name != "older" || got.Releases[3].Name != "third" {
		t.Fatalf("incorrect release order: %+v", got.Releases)
	}
	for _, pod := range got.Pods {
		if pod.Name == "current-deleted" && (pod.Release != object.Release || pod.CreatedAt >= now.Add(-24*time.Hour).UnixMilli()) {
			t.Fatalf("deleted predecessor did not recover current-release history: %+v", pod)
		}
		if strings.HasPrefix(pod.Name, "ancient") {
			t.Fatal("release beyond configured N retained")
		}
	}
	cfg := config.Default("simple")
	cfg.ReleaseHistory = 1
	got = historyClient(t, cfg, data).HistoricalPods(context.Background(), object)
	if len(got.Releases) != 1 || got.DeletedPods() != 1 {
		t.Fatalf("N=1 discarded deleted pods of the current release: %+v", got)
	}
}

func TestDirectOwnersUseHistoricalRevisionsAndBoundPodIncarnations(t *testing.T) {
	for _, kind := range []string{"StatefulSet", "DaemonSet"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			data := map[string][]series{
				"kube_pod_owner":          {metadata(map[string]string{"pod": "app-0", "uid": "old", "owner_name": "app", "owner_uid": "workload"}, 1)},
				"kube_pod_created":        {metadata(map[string]string{"pod": "app-0", "uid": "old"}, float64(now.Add(-8*24*time.Hour).Unix()))},
				"kube_pod_labels":         {metadata(map[string]string{"pod": "app-0", "uid": "old", "label_controller_revision_hash": "previous"}, 1)},
				"kube_pod_container_info": {metadata(map[string]string{"pod": "app-0", "uid": "old", "image": "app:1"}, 1)},
			}
			object := model.Object{Name: "app", Namespace: "ns", Kind: kind, Container: "main", UID: "workload", Release: "controller-revision-hash:current", Pods: []model.Pod{{Name: "app-0", UID: "new", Release: "controller-revision-hash:current", CreatedAt: now.Add(-time.Hour).UnixMilli(), Image: "app:2"}}}
			got := historyClient(t, config.Default("simple"), data).HistoricalPods(context.Background(), object)
			if len(got.Pods) != 2 || got.Pods[0].Release != "controller-revision-hash:previous" || got.Pods[0].EndedAt != got.Pods[1].CreatedAt || got.CurrentPods() != 1 {
				t.Fatalf("direct owner/reused pod name lost: %+v", got)
			}
		})
	}
}

func TestImageFallbackNeverMergesTwoKnownRevisionsWithSameImage(t *testing.T) {
	now := time.Now().UTC()
	data := map[string][]series{
		"kube_pod_created":        {metadata(map[string]string{"pod": "deleted", "uid": "old"}, float64(now.Add(-2*time.Hour).Unix()))},
		"kube_pod_owner":          {metadata(map[string]string{"pod": "deleted", "uid": "old", "owner_name": "app", "owner_uid": "workload"}, 1)},
		"kube_pod_container_info": {metadata(map[string]string{"pod": "deleted", "uid": "old", "image": "app:fixed"}, 1)},
	}
	object := model.Object{Name: "app", Namespace: "ns", Kind: "DaemonSet", Container: "main", UID: "workload", Release: "B",
		Pods:     []model.Pod{{Name: "live", UID: "new", Release: "B", Image: "app:fixed", CreatedAt: now.Add(-time.Hour).UnixMilli()}},
		Releases: []model.Release{{ID: "A", Image: "app:fixed"}, {ID: "B", Image: "app:fixed"}},
	}
	got := historyClient(t, config.Default("simple"), data).HistoricalPods(context.Background(), object)
	for _, pod := range got.Pods {
		if pod.Deleted && pod.Release == "B" {
			t.Fatal("configuration-only revisions merged by image")
		}
	}
	if !strings.Contains(strings.Join(got.Warnings, ","), "HistoricalReleaseIdentityUsesImage") {
		t.Fatalf("image fallback not disclosed: %v", got.Warnings)
	}
}

func TestMissingHistoricalMetricsPreserveLiveInventory(t *testing.T) {
	object := model.Object{Name: "app", Namespace: "ns", Kind: "Deployment", Release: "current", Releases: []model.Release{{ID: "current", Name: "rs"}}, Pods: []model.Pod{{Name: "live", UID: "pod", Release: "current", CreatedAt: time.Now().UnixMilli()}}}
	got := historyClient(t, config.Default("simple"), nil).HistoricalPods(context.Background(), object)
	if got.CurrentPods() != 1 || !strings.Contains(strings.Join(got.Warnings, ","), "HistoricalOwnershipUnavailable") || strings.Contains(strings.Join(got.Warnings, ","), "HistoricalPodsRequireVerifiedIdentity") {
		t.Fatalf("missing metadata handled incorrectly: %+v", got)
	}
}

func TestRecreatedOwnersAndMissingUIDCannotSupplyHistory(t *testing.T) {
	obj := model.Object{Name: "app", Namespace: "ns", Kind: "Deployment", UID: "current", Release: "current-release",
		Releases: []model.Release{{ID: "current-release", Name: "rs", OwnerUID: "live-rs"}}}
	data := map[string][]series{
		"kube_replicaset_owner": {metadata(map[string]string{"replicaset": "previous", "uid": "old-rs", "owner_uid": "recreated"}, 1)},
	}
	for _, owner := range []string{"old-rs", "", "live-rs"} {
		labels := map[string]string{"pod": "retired-" + owner, "uid": "pod-" + owner, "owner_name": "rs", "owner_uid": owner}
		data["kube_pod_owner"] = append(data["kube_pod_owner"], metadata(labels, 1))
		data["kube_pod_created"] = append(data["kube_pod_created"], metadata(labels, 1800000000))
	}
	got := historyClient(t, config.Default("simple"), data).HistoricalPods(context.Background(), obj)
	if len(got.Pods) != 1 || got.Pods[0].UID != "pod-live-rs" || !got.Pods[0].Deleted {
		t.Fatalf("wrong historical membership: %+v", got.Pods)
	}
}

func TestStockKSMHistoryRequiresLiveControllerLifetime(t *testing.T) {
	const born = 1800000000
	for _, kind := range []string{"Deployment", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			const release = "controller-revision-hash:current"
			object := model.Object{Name: "app", Namespace: "ns", Container: "main", UID: "current-workload", Kind: kind, Release: release, WorkloadCreatedAt: born * 1000}
			ownerName, ownerUID, ownerKind := "app", object.UID, kind
			if kind == "Deployment" {
				ownerName, ownerUID, ownerKind = "app-rs", "current-rs", "ReplicaSet"
				object.Releases = []model.Release{{ID: release, Name: ownerName, OwnerUID: ownerUID, CreatedAt: born * 1000}}
			}
			data := map[string][]series{}
			for _, tc := range []struct {
				name, uid, parent string
				created           int
			}{
				{"native-after", "pod-ok", "", born + 1}, {"native-before", "pod-old", "", born - 1}, {"same-second", "pod-uncertain", "", born},
				{"explicit-other", "pod-other", "foreign-owner", born + 1}, {"no-pod-uid", "", "", born + 1}, {"no-birth", "pod-undated", "", 0},
				{"birth-conflict", "pod-date", "", born - 1}, {"birth-conflict", "pod-date", "", born + 1},
				{"mixed-mirror", "pod-mixed", "foreign-owner", born + 1}, {"mixed-mirror", "pod-mixed", "", born + 1},
				{"conflict", "pod-conflict", ownerUID, born + 1}, {"conflict", "pod-conflict", "foreign-owner", born + 1},
			} {
				labels := map[string]string{"pod": tc.name, "uid": tc.uid, "owner_name": ownerName, "owner_kind": ownerKind, "owner_is_controller": "true"}
				if tc.parent != "" {
					labels["owner_uid"] = tc.parent
				}
				data["kube_pod_owner"] = append(data["kube_pod_owner"], metadata(labels, 1))
				data["kube_pod_created"] = append(data["kube_pod_created"], metadata(map[string]string{"pod": tc.name, "uid": tc.uid}, float64(tc.created)))
				data["kube_pod_labels"] = append(data["kube_pod_labels"], metadata(map[string]string{"pod": tc.name, "uid": tc.uid, "label_controller_revision_hash": "current"}, 1))
			}
			got := historyClient(t, config.Default("simple"), data).HistoricalPods(context.Background(), object)
			if len(got.Pods) != 1 || got.Pods[0].UID != "pod-ok" || got.Pods[0].Release != release {
				t.Fatalf("wrong temporal attribution: %+v", got.Pods)
			}
		})
	}
}
