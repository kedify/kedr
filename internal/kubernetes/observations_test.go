package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
	"github.com/kedify/kedr/internal/strategy"
	"github.com/kedify/recommender/analysis"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func controller(kind, name, uid string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, UID: types.UID(uid), Controller: &yes}}
}

func TestObserveStandalonePod(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shell", Namespace: "ns", UID: "pod-uid", Generation: 2, CreationTimestamp: metav1.NewTime(now.Add(-4 * time.Hour))},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:1", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")}}}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "main", RestartCount: 5, ContainerID: "containerd://current", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-30 * time.Minute))}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(now.Add(-time.Hour))}}}}},
	}
	loader := NewLoader(config.Default("simple"))
	object := loader.fromStandalonePod(nil, pod)[0]
	// Observe must use the current pod allocation, not an earlier discovery value.
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
	// The kubelet has applied only 25m during this in-place resize.
	pod.Status.ContainerStatuses[0].Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")}}
	neighbor := pod.DeepCopy()
	neighbor.Name, neighbor.UID = "unrelated", "other-uid"
	client := fake.NewSimpleClientset(pod, neighbor)
	got, err := loader.Observe(context.Background(), Clients{Typed: client}, object)
	if err != nil {
		t.Fatal(err)
	}
	if !got.InventoryAvailable || got.IdentityAmbiguous || got.CurrentPods() != 1 || got.DeletedPods() != 0 || got.UID != string(pod.UID) || got.ReleaseStartedAt != pod.CreationTimestamp.UnixMilli() {
		t.Fatalf("standalone pod identity or inventory lost: %+v", got)
	}
	observed := got.Pods[0]
	if observed.Name != pod.Name || observed.UID != got.UID || observed.Release != got.Release || observed.ContainerID != "containerd://current" || observed.ContainerStartedAt != now.Add(-30*time.Minute).UnixMilli() {
		t.Fatalf("pod/container lifetimes not preserved: %+v", observed)
	}
	if got.Allocations.Requests[model.CPU].Value != .05 || observed.Allocations.Requests[model.CPU].Value != .025 || len(got.Releases) != 1 || !got.Releases[0].Current {
		t.Fatalf("current allocations or release metadata lost: %+v", got)
	}
	if len(got.OOMKills) != 1 || got.OOMKills[0].PodUID != got.UID || got.OOMKills[0].WorkloadUID != got.UID || got.OOMKills[0].Release != got.Release {
		t.Fatalf("OOM evidence lost: %+v", got.OOMKills)
	}
	if actions := client.Actions(); len(actions) != 1 || actions[0].GetVerb() != "get" || actions[0].GetResource().Resource != "pods" {
		t.Fatalf("standalone observation queried unrelated resources: %+v", actions)
	}
}

func TestObserveStandalonePodRejectsChangedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
	}{
		{"recreated", func(p *corev1.Pod) { p.UID = "replacement-uid" }},
		{"adopted", func(p *corev1.Pod) { p.OwnerReferences = controller("ReplicaSet", "owner", "owner-uid") }},
		{"mirror", func(p *corev1.Pod) { p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "hash"} }},
		{"missing container", func(p *corev1.Pod) { p.Spec.Containers = nil }},
		{"deleted", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shell", Namespace: "ns", UID: "original-uid", CreationTimestamp: metav1.Now()}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}
			loader := NewLoader(config.Default("simple"))
			object := loader.fromStandalonePod(nil, pod)[0]
			client := fake.NewSimpleClientset()
			if tc.change != nil {
				tc.change(pod)
				client = fake.NewSimpleClientset(pod)
			}
			got, err := loader.Observe(context.Background(), Clients{Typed: client}, object)
			if err == nil || got.InventoryAvailable || len(got.Pods) != 0 || got.UID != object.UID {
				t.Fatalf("changed/missing pod was trusted: %+v, %v", got, err)
			}
		})
	}
}

func TestObserveChecksOwnerNameAndSelectsCurrentRelease(t *testing.T) {
	now := time.Unix(1800000000, 0)
	makeSet := func(name, uid, workloadUID, revision, hash string) *appsv1.ReplicaSet {
		ownerName := "app"
		return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid), OwnerReferences: controller("Deployment", ownerName, workloadUID), Annotations: map[string]string{"deployment.kubernetes.io/revision": revision}}, Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pod-template-hash": hash}}}}}
	}
	makePod := func(name, setUID string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name + "-uid"), CreationTimestamp: metav1.NewTime(now.Add(-24 * time.Hour)), OwnerReferences: controller("ReplicaSet", "rs", setUID)}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "app:1"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: "runtime://abc", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(now.Add(-2 * time.Hour))}}}}}}
	}
	client := fake.NewSimpleClientset(
		makeSet("old", "rs-old", "workload", "1", "a"),
		makeSet("new", "rs-new", "workload", "2", "b"),
		makeSet("recreated", "rs-wrong", "other-workload", "2", "wrong"),
		makePod("old-pod", "rs-old"), makePod("new-pod", "rs-new"), makePod("wrong-pod", "rs-wrong"), makePod("unresolved", "nonexistent-uid"),
	)
	object := model.Object{Name: "app", Namespace: "ns", Container: "main", Kind: "Deployment", UID: "workload", ObservedAt: now.UnixMilli(), Generation: 2, ObservedGeneration: 2, Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}
	got, err := NewLoader(config.Default("simple")).Observe(context.Background(), Clients{Typed: client}, object)
	if err != nil {
		t.Fatal(err)
	}
	if got.Release != "pod-template-hash:b" || len(got.Pods) != 2 || !got.InventoryAvailable || got.IdentityAmbiguous {
		t.Fatalf("identity attribution failed: %+v", got)
	}
	if got.ObservedAt != object.ObservedAt || got.ReleaseStartedAt != 0 {
		t.Fatal("API observation time or inferred release boundary was fabricated")
	}
	for _, pod := range got.Pods {
		if pod.UID == "" || pod.CreatedAt == 0 || pod.ContainerStartedAt == 0 || pod.ContainerID == "" {
			t.Fatalf("missing lifetime metadata: %+v", pod)
		}
	}
	if len(got.OOMKills) != 2 {
		t.Fatalf("OOM status not converted: %+v", got.OOMKills)
	}
	for _, kill := range got.OOMKills {
		if kill.Timestamp != now.Add(-2*time.Hour).UnixMilli() || kill.MemoryLimitBytes != 0 || kill.WorkloadUID != "workload" {
			t.Fatalf("unsafe OOM conversion: %+v", kill)
		}
	}
	object.Annotations["deployment.kubernetes.io/revision"] = "3"
	got, err = NewLoader(config.Default("simple")).Observe(context.Background(), Clients{Typed: client}, object)
	if err != nil {
		t.Fatal(err)
	}
	if got.Release != "" {
		t.Fatal("old pods were relabeled as an unobserved new rollout")
	}
}

func TestObserveUnknownRevisionAndUnobservedGeneration(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pod", CreationTimestamp: metav1.Now(), OwnerReferences: controller("StatefulSet", "db", "workload")}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}
	client := fake.NewSimpleClientset(pod)
	object := model.Object{Name: "db", Namespace: "ns", Container: "main", Kind: "StatefulSet", UID: "workload", Release: "controller-revision-hash:new", Generation: 2, ObservedGeneration: 1}
	got, err := NewLoader(config.Default("simple")).Observe(context.Background(), Clients{Typed: client}, object)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExcludedPods != 1 || len(got.Pods) != 0 || !got.IdentityAmbiguous {
		t.Fatalf("unknown revision/generation ignored: %+v", got)
	}
}

func TestObserveDeniedInventoryDoesNotInventPods(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") })
	object := model.Object{UID: "job", Kind: "Job", Namespace: "ns", Release: "job-release"}
	got, err := NewLoader(config.Default("simple")).Observe(context.Background(), Clients{Typed: client}, object)
	if err == nil || got.InventoryAvailable || len(got.Pods) != 0 {
		t.Fatalf("denied collection became complete inventory: %+v, %v", got, err)
	}
}

func TestCronJobTemplateIdentityAndJobUIDOwnership(t *testing.T) {
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "app:1"}}}}
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "scheduled", Namespace: "ns", UID: "cron"}, Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: template}}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "ns", UID: "job", OwnerReferences: controller("CronJob", "scheduled", "cron")}, Spec: batchv1.JobSpec{Template: *template.DeepCopy()}}
	job.Spec.Template.Labels = map[string]string{"batch.kubernetes.io/controller-uid": "job", "batch.kubernetes.io/job-name": "run"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "run-pod", Namespace: "ns", UID: "pod", CreationTimestamp: metav1.Now(), OwnerReferences: controller("Job", "run", "job")}, Spec: template.Spec}
	loader := NewLoader(config.Default("simple"))
	object := loader.fromCronJob(nil, cron, nil)[0]
	got, err := loader.Observe(context.Background(), Clients{Typed: fake.NewSimpleClientset(job, pod)}, object)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pods) != 1 || got.Pods[0].Release != object.Release || got.UID != "cron" {
		t.Fatalf("generated Job labels changed release identity: %+v", got)
	}
}

func TestDaemonSetSelectsNewestControllerRevisionDuringRollout(t *testing.T) {
	now := time.Now().UTC()
	objects := make([]runtime.Object, 0, 4)
	for i, hash := range []string{"old", "new"} {
		template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "app:" + hash}}}}
		data, err := json.Marshal(map[string]any{"spec": map[string]any{"template": template}})
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects,
			&appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "app-" + hash, Namespace: "ns", CreationTimestamp: metav1.NewTime(now.Add(time.Duration(i-2) * time.Hour)), OwnerReferences: controller("DaemonSet", "app", "workload")}, Revision: int64(i + 1), Data: runtime.RawExtension{Raw: data}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + hash, Namespace: "ns", UID: types.UID("pod-" + hash), CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)), Labels: map[string]string{"controller-revision-hash": hash}, OwnerReferences: controller("DaemonSet", "app", "workload")}, Spec: template.Spec},
		)
	}
	object := model.Object{Name: "app", Namespace: "ns", Kind: "DaemonSet", Container: "main", UID: "workload", Generation: 2, ObservedGeneration: 2, IdentityAmbiguous: true}
	got, err := NewLoader(config.Default("simple")).Observe(context.Background(), Clients{Typed: fake.NewSimpleClientset(objects...)}, object)
	if err != nil || got.Release != "controller-revision-hash:new" || got.IdentityAmbiguous || len(got.Releases) != 2 || len(got.Pods) != 2 {
		t.Fatalf("newest revision not isolated: %+v, %v", got, err)
	}
}

func TestDeploymentOOMAcrossRolloutAndRollback(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	at := func(hoursAgo int) metav1.Time { return metav1.NewTime(now.Add(-time.Duration(hoursAgo) * time.Hour)) }
	const revision = "deployment.kubernetes.io/revision"
	for _, tc := range []struct {
		name, history, completedSet          string
		completionHours, killHours           int
		lagging, ambiguous, incomplete       bool
		prunedPredecessor, reusedPredecessor bool
		retired                              bool
		wantKills                            int
	}{
		{name: "new set fails before completing", completedSet: "app-a", completionHours: 2, killHours: 3, wantKills: 1},
		{name: "new set with previous completion", completedSet: "app-b", completionHours: 6, killHours: 3, wantKills: 1},
		{name: "rollback retains safety veto for surviving Pod", history: "1", completedSet: "app-a", completionHours: 2, killHours: 7, incomplete: true},
		{name: "rollback excludes retired Pod before intervening set", history: "1", completedSet: "app-a", completionHours: 2, killHours: 7, retired: true},
		{name: "rollback kill during uncertain interval holds memory", history: "1", completedSet: "app-a", completionHours: 2, killHours: 3, incomplete: true},
		{name: "rollback keeps new kill", history: "1", completedSet: "app-a", completionHours: 2, killHours: 1, wantKills: 1},
		{name: "rollback previous set completed", history: "1", completedSet: "app-b", completionHours: 4, killHours: 3, ambiguous: true},
		{name: "rollback original completion predates intervening set", history: "1", completedSet: "app-a", completionHours: 8, killHours: 3, ambiguous: true},
		{name: "rollback predecessor pruned", history: "1", completedSet: "app-a", completionHours: 8, killHours: 3, prunedPredecessor: true, ambiguous: true},
		{name: "rollback predecessor previously reused", history: "1", completedSet: "app-a", completionHours: 8, killHours: 3, reusedPredecessor: true, ambiguous: true},
		{name: "generation not yet observed", history: "1", completedSet: "app-a", completionHours: 2, killHours: 3, lagging: true, ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			container := corev1.Container{Name: "main", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			}}
			template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}
			d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns", UID: "workload", Generation: 3, Annotations: map[string]string{revision: "3"}}, Spec: appsv1.DeploymentSpec{Template: template}}
			d.Status.ObservedGeneration = d.Generation
			if tc.lagging {
				d.Status.ObservedGeneration--
			}
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable", LastUpdateTime: at(tc.completionHours), Message: fmt.Sprintf("ReplicaSet %q has successfully progressed.", tc.completedSet)}}
			set := func(name, rev string, born int) *appsv1.ReplicaSet {
				rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "app-" + name, Namespace: "ns", UID: types.UID("rs-" + name), CreationTimestamp: at(born), Annotations: map[string]string{revision: rev}, OwnerReferences: controller("Deployment", "app", "workload")}, Spec: appsv1.ReplicaSetSpec{Template: template}}
				rs.Spec.Template.Labels = map[string]string{"pod-template-hash": name}
				return rs
			}
			birth := 4
			if tc.history != "" {
				birth = 10
			}
			a := set("a", "3", birth)
			a.Annotations["deployment.kubernetes.io/revision-history"] = tc.history
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "p", CreationTimestamp: at(tc.killHours + 1), OwnerReferences: controller("ReplicaSet", "app-a", "rs-a")}, Spec: template.Spec, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: at(tc.killHours)}}}}}}
			cfg := config.Default("simple_limit")
			cfg.MinimumHistoryHours, cfg.PointsRequired = .5, 2
			loader := NewLoader(cfg)
			objects := []runtime.Object{a, pod}
			usageUID := "p"
			if tc.retired {
				pod.Status.Phase = corev1.PodFailed
				fresh := pod.DeepCopy()
				fresh.Name, fresh.UID = "replacement", "replacement"
				fresh.CreationTimestamp = at(tc.completionHours)
				fresh.Status = corev1.PodStatus{Phase: corev1.PodRunning}
				objects = append(objects, fresh)
				usageUID = string(fresh.UID)
			}
			if !tc.prunedPredecessor {
				b := set("b", "2", 5)
				if tc.reusedPredecessor {
					b.CreationTimestamp = at(12)
					b.Annotations["deployment.kubernetes.io/revision-history"] = "1"
				}
				objects = append(objects, b)
			}
			object, err := loader.Observe(context.Background(), Clients{Typed: fake.NewSimpleClientset(objects...)}, loader.fromDeployment(nil, d, nil)[0])
			if err != nil {
				t.Fatal(err)
			}
			if object.IdentityAmbiguous != tc.ambiguous || object.RolloutBoundaryVerified == tc.ambiguous {
				t.Fatalf("unexpected identity: %+v", object)
			}
			if tc.ambiguous {
				return
			}
			metrics := strategy.Metrics{WindowStart: at(12).UnixMilli(), EvaluationTime: object.ObservedAt}
			memory := analysis.Series{ID: usageUID + "/main", PodUID: usageUID, WorkloadUID: object.UID, Release: object.Release, Kind: analysis.SampleGauge}
			for ts := max(object.ReleaseStartedAt, object.Pods[0].CreatedAt); ts <= metrics.EvaluationTime; ts += 60000 {
				memory.Samples = append(memory.Samples, analysis.Sample{Timestamp: ts, Value: 100 << 20})
			}
			metrics.Memory = []analysis.Series{memory}
			in := strategy.Input(cfg, metrics, object)
			if in.Containers[0].OOMCoverageIncomplete != tc.incomplete {
				t.Fatalf("wrong OOM coverage: %+v", in.Containers[0])
			}
			result, err := strategy.Run(cfg, metrics, object)
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(result.Analysis.Results, func(r analysis.ResourceAnalysis) bool { return r.Resource == analysis.ResourceMemory })
			if i < 0 || len(result.Analysis.Results[i].Evidence.OOMKills) != tc.wantKills {
				t.Fatalf("unexpected selected OOMs: %+v", result.Analysis.Results)
			}
			wantDecrease := tc.wantKills == 0 && !tc.incomplete
			if (len(result.Analysis.Results[i].Recommendations) > 0) != wantDecrease {
				t.Fatalf("wanted memory decrease=%t, got %+v", wantDecrease, result.Analysis.Results[i])
			}
		})
	}
}

func TestPendingResizeWithoutAppliedStatusDoesNotIntroduceCap(t *testing.T) {
	cfg := config.Default("simple_limit")
	loader := NewLoader(cfg)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pod", CreationTimestamp: metav1.Now()}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodResizeInProgress, Status: corev1.ConditionTrue}}}}
	object, err := loader.Observe(context.Background(), Clients{Typed: fake.NewSimpleClientset(pod)}, loader.fromStandalonePod(nil, pod)[0])
	if err != nil {
		t.Fatal(err)
	}
	c := strategy.Input(cfg, strategy.Metrics{}, object).Containers[0]
	if c.CPU.CurrentLimit.Available || c.CPU.CurrentLimit.Unset {
		t.Fatalf("pending applied limit became known-unset: %+v", c.CPU.CurrentLimit)
	}
}
