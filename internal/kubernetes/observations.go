package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/recommender/analysis"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func templateHash(template corev1.PodTemplateSpec) string {
	template = *template.DeepCopy()
	for _, key := range []string{"controller-uid", "job-name", "batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name"} {
		delete(template.Labels, key)
	}
	data, _ := json.Marshal(template)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// Observe captures the live inventory and release metadata. The Prometheus
// adapter subsequently adds deleted pods using historical KSM ownership.
func (l *Loader) Observe(ctx context.Context, clients Clients, object model.Object) (model.Object, error) {
	object.Pods = []model.Pod{}
	object.InventoryAvailable = false
	if object.UID == "" {
		return object, nil
	}
	if object.Generation > object.ObservedGeneration {
		object.IdentityAmbiguous = true
	}
	// Intermediate owner UID -> release. Direct owners use the workload UID.
	owners := map[string]string{}
	ownerKind := object.Kind
	switch object.Kind {
	case "Deployment", "Rollout":
		sets, err := clients.Typed.AppsV1().ReplicaSets(object.Namespace).List(ctx, metav1.ListOptions{LabelSelector: object.Selector})
		if err != nil {
			return object, fmt.Errorf("resolve ReplicaSet identities: %w", err)
		}
		ownerKind = "ReplicaSet"
		label := "pod-template-hash"
		if object.Kind == "Rollout" {
			label = "rollouts-pod-template-hash"
		}
		for _, set := range sets.Items {
			owner := metav1.GetControllerOf(&set)
			if owner == nil || owner.Name != object.Name || owner.Kind != object.Kind {
				continue
			}
			hash := set.Spec.Template.Labels[label]
			if hash == "" {
				continue
			}
			release := label + ":" + hash
			owners[string(set.UID)] = release
			revision, _ := strconv.ParseInt(set.Annotations["deployment.kubernetes.io/revision"], 10, 64)
			info := model.Release{ID: release, Name: set.Name, Revision: revision}
			if !set.CreationTimestamp.IsZero() {
				info.CreatedAt = set.CreationTimestamp.UnixMilli()
			}
			for _, container := range set.Spec.Template.Spec.Containers {
				if container.Name == object.Container {
					info.Image = container.Image
				}
			}
			object.Releases = append(object.Releases, info)
			if object.Kind == "Deployment" && object.Annotations["deployment.kubernetes.io/revision"] != "" && set.Annotations["deployment.kubernetes.io/revision"] == object.Annotations["deployment.kubernetes.io/revision"] {
				if object.Release != "" && object.Release != release {
					object.IdentityAmbiguous = true
				}
				object.Release = release
			}
		}
	case "CronJob":
		jobs, err := clients.Typed.BatchV1().Jobs(object.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return object, fmt.Errorf("resolve Job identities: %w", err)
		}
		ownerKind = "Job"
		for _, job := range jobs.Items {
			owner := metav1.GetControllerOf(&job)
			if owner != nil && owner.Kind == "CronJob" && string(owner.UID) == object.UID && job.UID != "" {
				owners[string(job.UID)] = "template:" + templateHash(job.Spec.Template)
			}
		}
	default:
		owners[object.UID] = object.Release
	}
	items, err := clients.Typed.CoreV1().Pods(object.Namespace).List(ctx, metav1.ListOptions{LabelSelector: object.Selector})
	if err != nil {
		return object, fmt.Errorf("read pod identities: %w", err)
	}
	object.InventoryAvailable = true
	currentReleases := map[string]bool{}
	for _, pod := range items.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != ownerKind {
			continue
		}
		release, owned := owners[string(owner.UID)]
		if !owned {
			continue
		}
		if object.Kind == "StatefulSet" || object.Kind == "DaemonSet" {
			if hash := pod.Labels["controller-revision-hash"]; hash != "" {
				release = "controller-revision-hash:" + hash
			} else {
				release = ""
			}
		}
		var container *corev1.Container
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == object.Container {
				container = &pod.Spec.Containers[i]
				break
			}
		}
		if container == nil || pod.UID == "" || release == "" || pod.CreationTimestamp.IsZero() {
			object.ExcludedPods++
			continue
		}
		deleted := pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
		p := model.Pod{Name: pod.Name, UID: string(pod.UID), Release: release, CreatedAt: pod.CreationTimestamp.UnixMilli(), Deleted: deleted, Image: container.Image, Allocations: allocations(*container)}
		if !deleted {
			currentReleases[release] = true
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != object.Container {
				continue
			}
			p.ContainerID = status.ContainerID
			if status.State.Running != nil && !status.State.Running.StartedAt.IsZero() {
				p.ContainerStartedAt = status.State.Running.StartedAt.UnixMilli()
			}
			for _, terminated := range []*corev1.ContainerStateTerminated{status.State.Terminated, status.LastTerminationState.Terminated} {
				if terminated == nil || terminated.Reason != "OOMKilled" || terminated.FinishedAt.IsZero() {
					continue
				}
				ts := terminated.FinishedAt.UnixMilli()
				object.OOMKills = append(object.OOMKills, analysis.OOMKill{ID: fmt.Sprintf("%s/%s/%d", p.UID, object.Container, ts), PodUID: p.UID, WorkloadUID: object.UID, Release: release, Timestamp: ts})
			}
		}
		object.Pods = append(object.Pods, p)
	}
	if object.Release == "" && object.Kind == "DaemonSet" {
		if len(currentReleases) == 1 {
			for release := range currentReleases {
				object.Release = release
			}
		} else if len(currentReleases) > 1 {
			object.IdentityAmbiguous = true
		}
	}
	if object.Kind == "StatefulSet" || object.Kind == "DaemonSet" {
		revisions, err := clients.Typed.AppsV1().ControllerRevisions(object.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			object.Warnings = append(object.Warnings, "ControllerRevisionMetadataUnavailable")
		} else {
			var latest model.Release
			for _, revision := range revisions.Items {
				owner := metav1.GetControllerOf(&revision)
				if owner == nil || owner.Kind != object.Kind || owner.Name != object.Name {
					continue
				}
				hash := revision.Name
				if object.Kind == "DaemonSet" {
					hash = strings.TrimPrefix(hash, object.Name+"-")
				}
				var data struct {
					Spec struct {
						Template corev1.PodTemplateSpec `json:"template"`
					} `json:"spec"`
				}
				if err := json.Unmarshal(revision.Data.Raw, &data); err != nil {
					continue
				}
				info := model.Release{ID: "controller-revision-hash:" + hash, Name: revision.Name, Revision: revision.Revision}
				if !revision.CreationTimestamp.IsZero() {
					info.CreatedAt = revision.CreationTimestamp.UnixMilli()
				}
				for _, container := range data.Spec.Template.Spec.Containers {
					if container.Name == object.Container {
						info.Image = container.Image
					}
				}
				object.Releases = append(object.Releases, info)
				if info.Revision > latest.Revision {
					latest = info
				}
			}
			if object.Kind == "DaemonSet" && latest.ID != "" {
				object.Release = latest.ID
				object.IdentityAmbiguous = object.Generation > object.ObservedGeneration
			}
		}
	}
	sort.Slice(object.Pods, func(i, j int) bool { return object.Pods[i].Name < object.Pods[j].Name })
	return object, nil
}
