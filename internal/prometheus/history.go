package prometheus

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kedify/kedr/internal/model"
)

// HistoricalPods reconstructs deleted Pods only when their controller UID chain is verified.
func (c *Client) HistoricalPods(ctx context.Context, object model.Object) model.Object {
	end := time.Now().UTC()
	warn := func(message string) {
		for _, existing := range object.Warnings {
			if existing == message {
				return
			}
		}
		object.Warnings = append(object.Warnings, message)
	}
	values := func(metric, matchers string) ([]series, bool) {
		query := fmt.Sprintf("max_over_time(%s{namespace=%q%s%s}[%dms])", metric, object.Namespace, clusterMatcher(c.cfg), matchers, history(c.cfg).Milliseconds())
		rows, err := c.queryAt(ctx, query, end)
		if err != nil {
			warn("HistoricalMetadataCollectionFailed:" + metric)
			if c.logger != nil {
				c.logger.Warnf("historical %s for %s/%s: %v", metric, object.Namespace, object.Name, err)
			}
			return nil, false
		}
		return rows, true
	}
	releases := map[string]model.Release{}
	ownerRelease := map[string]string{}
	for _, release := range object.Releases {
		releases[release.ID] = release
		if release.OwnerUID != "" {
			ownerRelease[release.OwnerUID] = release.ID
		}
	}
	var ownerMatcher string
	switch object.Kind {
	case "Deployment", "Rollout":
		rows, ok := values("kube_replicaset_owner", fmt.Sprintf(",owner_kind=%q,owner_name=%q,owner_is_controller=\"true\"", object.Kind, object.Name))
		for _, row := range rows {
			if !positive(row) || row.Metric["replicaset"] == "" {
				continue
			}
			name := row.Metric["replicaset"]
			if row.Metric["owner_uid"] != object.UID || object.UID == "" || row.Metric["uid"] == "" {
				continue
			}
			uid := row.Metric["uid"]
			if _, exists := ownerRelease[uid]; !exists {
				id := "replicaset:" + uid
				ownerRelease[uid] = id
				releases[id] = model.Release{ID: id, Name: name, OwnerUID: uid}
			}
		}
		if ok && len(rows) == 0 {
			warn("HistoricalOwnershipUnavailable:kube_replicaset_owner")
		}
		if len(ownerRelease) == 0 {
			warn("HistoricalPodIdentityUnverified")
			return object
		}
		names := make([]string, 0, len(ownerRelease))
		for _, id := range ownerRelease {
			names = append(names, regexp.QuoteMeta(releases[id].Name))
		}
		sort.Strings(names)
		pattern := strings.Join(names, "|")
		created, _ := values("kube_replicaset_created", fmt.Sprintf(",replicaset=~%q", pattern))
		for _, row := range created {
			id := ownerRelease[row.Metric["uid"]]
			if id == "" {
				continue
			}
			release := releases[id]
			if ts := timestampValue(row); ts > 0 && release.CreatedAt == 0 && row.Metric["replicaset"] == release.Name {
				release.CreatedAt = ts
			}
			releases[id] = release
		}
		ownerMatcher = fmt.Sprintf(",owner_kind=\"ReplicaSet\",owner_name=~%q,owner_is_controller=\"true\"", pattern)
	case "StatefulSet", "DaemonSet":
		ownerMatcher = fmt.Sprintf(",owner_kind=%q,owner_name=%q,owner_is_controller=\"true\"", object.Kind, object.Name)
	default:
		return object
	}
	owners, ok := values("kube_pod_owner", ownerMatcher)
	if ok && len(owners) == 0 {
		warn("HistoricalOwnershipUnavailable:kube_pod_owner")
	}
	// Metadata is batched by discovered pod names, not guessed name prefixes.
	names := map[string]bool{}
	for _, row := range owners {
		if positive(row) && row.Metric["pod"] != "" {
			names[row.Metric["pod"]] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	var created, labels, containers []series
	for first := 0; first < len(ordered); first += 50 {
		var escaped []string
		for _, name := range ordered[first:min(first+50, len(ordered))] {
			escaped = append(escaped, regexp.QuoteMeta(name))
		}
		matcher := fmt.Sprintf(",pod=~%q", strings.Join(escaped, "|"))
		rows, _ := values("kube_pod_created", matcher)
		created = append(created, rows...)
		rows, _ = values("kube_pod_container_info", matcher+fmt.Sprintf(",container=%q", object.Container))
		containers = append(containers, rows...)
		if object.Kind == "StatefulSet" || object.Kind == "DaemonSet" {
			rows, _ = values("kube_pod_labels", matcher)
			labels = append(labels, rows...)
		}
	}
	key := func(labels map[string]string) string { return labels["pod"] + "\x00" + labels["uid"] }
	births := map[string]int64{}
	for _, row := range created {
		if ts := timestampValue(row); ts > 0 {
			births[key(row.Metric)] = ts
		}
	}
	images := map[string]string{}
	for _, row := range containers {
		if positive(row) {
			images[key(row.Metric)] = row.Metric["image"]
		}
	}
	revisions := map[string]string{}
	for _, row := range labels {
		if revision := row.Metric["label_controller_revision_hash"]; positive(row) && revision != "" {
			revisions[key(row.Metric)] = "controller-revision-hash:" + revision
		}
	}
	known := map[string]bool{}
	for _, pod := range object.Pods {
		known[pod.Name+"\x00"+pod.UID] = true
	}
	for _, row := range owners {
		name, uid := row.Metric["pod"], row.Metric["uid"]
		if !positive(row) || name == "" || known[key(row.Metric)] {
			continue
		}
		parent := row.Metric["owner_uid"]
		release := ownerRelease[parent]
		verified := parent != "" && parent == object.UID && object.Name == row.Metric["owner_name"]
		if object.Kind == "Deployment" || object.Kind == "Rollout" {
			verified = release != "" && releases[release].Name == row.Metric["owner_name"]
		}
		createdAt := births[key(row.Metric)]
		if uid == "" || !verified || createdAt == 0 {
			warn("HistoricalPodIdentityUnverified")
			continue
		}
		image := images[key(row.Metric)]
		if object.Kind == "StatefulSet" || object.Kind == "DaemonSet" {
			release = revisions[key(row.Metric)]
			if release == "" && image != "" {
				// An image alone cannot identify configuration-only rollouts.
				release = "image:" + image
				warn("HistoricalReleaseIdentityUsesImage")
			}
		}
		if release == "" {
			warn("HistoricalReleaseIdentityUnavailable")
			continue
		}
		object.Pods = append(object.Pods, model.Pod{Name: name, UID: uid, Release: release, CreatedAt: createdAt, Deleted: true, Image: image})
		known[key(row.Metric)] = true
		info := releases[release]
		info.ID = release
		if info.Name == "" {
			info.Name = strings.TrimPrefix(release, "controller-revision-hash:")
		}
		if info.Image == "" {
			info.Image = image
		}
		if info.CreatedAt == 0 || (strings.HasPrefix(release, "image:") && createdAt < info.CreatedAt) {
			info.CreatedAt = createdAt
		}
		releases[release] = info
	}
	for _, pod := range object.Pods {
		if pod.Release == "" {
			continue
		}
		info := releases[pod.Release]
		info.ID = pod.Release
		if info.Image == "" {
			info.Image = pod.Image
		}
		if info.CreatedAt == 0 {
			info.CreatedAt = pod.CreatedAt
		}
		releases[pod.Release] = info
	}
	// Always retain the desired/current release, even when it has no pods yet.
	if object.Release != "" {
		info := releases[object.Release]
		info.ID, info.Current = object.Release, true
		if info.Image == "" {
			info.Image = object.Image
		}
		releases[object.Release] = info
	}
	object.Releases = nil
	for _, info := range releases {
		object.Releases = append(object.Releases, info)
	}
	// API rollout revisions preserve activation order across rollbacks. When
	// historical KSM-only releases lack revisions, fall back to creation time.
	haveRevisions := true
	for _, release := range object.Releases {
		haveRevisions = haveRevisions && release.Revision > 0
	}
	sort.Slice(object.Releases, func(i, j int) bool {
		a, b := object.Releases[i], object.Releases[j]
		if a.Current != b.Current {
			return a.Current
		}
		if haveRevisions && a.Revision != b.Revision {
			return a.Revision > b.Revision
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		if a.Revision != b.Revision {
			return a.Revision > b.Revision
		}
		return a.ID < b.ID
	})
	object.Releases = object.Releases[:min(max(1, c.cfg.ReleaseHistory), 4, len(object.Releases))]
	selected := map[string]bool{}
	for _, release := range object.Releases {
		selected[release.ID] = true
	}
	var pods []model.Pod
	for _, pod := range object.Pods {
		if selected[pod.Release] {
			pods = append(pods, pod)
		}
	}
	object.Pods = pods
	// Bound reused pod names (normal for StatefulSets) by their next creation.
	sort.Slice(object.Pods, func(i, j int) bool {
		a, b := object.Pods[i], object.Pods[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.CreatedAt < b.CreatedAt
	})
	for i := 0; i+1 < len(object.Pods); i++ {
		if object.Pods[i].Name == object.Pods[i+1].Name {
			object.Pods[i].EndedAt = object.Pods[i+1].CreatedAt
		}
	}
	return object
}

func positive(row series) bool {
	p, err := parsePair(row.Value)
	return err == nil && p.Value > 0 && !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0)
}

func timestampValue(row series) int64 {
	p, err := parsePair(row.Value)
	if err != nil || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value <= 0 || p.Value >= float64(math.MaxInt64)/1000 {
		return 0
	}
	return int64(math.Round(p.Value * 1000))
}
