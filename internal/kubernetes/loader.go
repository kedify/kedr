// Package kubernetes discovers scannable Kubernetes workloads.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kedify/kedr/internal/config"
	"github.com/kedify/kedr/internal/model"
)

type Clients struct {
	Name    *string
	Typed   kubernetes.Interface
	Dynamic dynamic.Interface
	REST    *rest.Config
}

type Loader struct{ cfg *config.Config }

func NewLoader(cfg *config.Config) *Loader { return &Loader{cfg: cfg} }

func (l *Loader) Clients(ctx context.Context) ([]Clients, error) {
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	if l.cfg.Kubeconfig != nil {
		loading.ExplicitPath = *l.cfg.Kubeconfig
	}
	raw, rawErr := loading.Load()
	contexts := append([]string(nil), l.cfg.ClusterValues...)
	if l.cfg.AllClusters {
		contexts = contexts[:0]
		for name := range raw.Contexts {
			contexts = append(contexts, name)
		}
		sort.Strings(contexts)
	}
	inside := false
	if len(contexts) == 0 {
		if rawErr == nil && raw.CurrentContext != "" {
			contexts = []string{raw.CurrentContext}
		} else {
			inside = true
		}
	}
	result := make([]Clients, 0, len(contexts)+1)
	if inside {
		restCfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
		}
		applyImpersonation(restCfg, l.cfg)
		clients, err := newClients(nil, restCfg)
		if err != nil {
			return nil, err
		}
		result = append(result, clients)
		l.cfg.InsideCluster = true
		return result, nil
	}
	if rawErr != nil {
		return nil, rawErr
	}
	for _, name := range contexts {
		if _, ok := raw.Contexts[name]; !ok {
			return nil, fmt.Errorf("kubernetes context %q not found", name)
		}
		overrides := &clientcmd.ConfigOverrides{CurrentContext: name}
		restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides).ClientConfig()
		if err != nil {
			return nil, err
		}
		applyImpersonation(restCfg, l.cfg)
		n := name
		clients, err := newClients(&n, restCfg)
		if err != nil {
			return nil, err
		}
		result = append(result, clients)
	}
	return result, nil
}

func applyImpersonation(restCfg *rest.Config, cfg *config.Config) {
	if cfg.ImpersonateUser != nil {
		restCfg.Impersonate.UserName = *cfg.ImpersonateUser
	}
	if cfg.ImpersonateGroup != nil {
		restCfg.Impersonate.Groups = []string{*cfg.ImpersonateGroup}
	}
}
func newClients(name *string, restCfg *rest.Config) (Clients, error) {
	typed, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return Clients{}, err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return Clients{}, err
	}
	return Clients{Name: name, Typed: typed, Dynamic: dyn, REST: restCfg}, nil
}

func (l *Loader) List(ctx context.Context, clients Clients) ([]model.Object, error) {
	namespaces, err := l.namespaces(ctx, clients.Typed)
	if err != nil {
		return nil, err
	}
	hpas := l.hpas(ctx, clients.Typed)
	var result []model.Object
	for _, namespace := range namespaces {
		if l.enabled("Deployment") {
			items, e := clients.Typed.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{LabelSelector: l.selector()})
			if e != nil {
				return nil, e
			}
			for i := range items.Items {
				result = append(result, l.fromDeployment(clients.Name, &items.Items[i], hpas)...)
			}
		}
		if l.enabled("StatefulSet") {
			items, e := clients.Typed.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: l.selector()})
			if e != nil {
				return nil, e
			}
			for i := range items.Items {
				result = append(result, l.fromStatefulSet(clients.Name, &items.Items[i], hpas)...)
			}
		}
		if l.enabled("DaemonSet") {
			items, e := clients.Typed.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: l.selector()})
			if e != nil {
				return nil, e
			}
			for i := range items.Items {
				result = append(result, l.fromDaemonSet(clients.Name, &items.Items[i], hpas)...)
			}
		}
		jobs, jobErr := l.jobs(ctx, clients.Typed, namespace)
		if jobErr != nil && (l.enabled("Job") || l.enabled("GroupedJob")) {
			return nil, jobErr
		}
		if l.enabled("Job") {
			for i := range jobs {
				if !ownedByCron(&jobs[i]) && !l.grouped(&jobs[i]) {
					result = append(result, l.fromJob(clients.Name, &jobs[i], "Job", hpas)...)
				}
			}
		}
		if l.enabled("GroupedJob") {
			result = append(result, l.groupJobs(clients.Name, jobs, hpas)...)
		}
		if l.enabled("CronJob") {
			items, e := clients.Typed.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: l.selector()})
			if e != nil {
				return nil, e
			}
			for i := range items.Items {
				result = append(result, l.fromCronJob(clients.Name, &items.Items[i], hpas)...)
			}
		}
		if l.enabled("Rollout") {
			rollouts, e := l.rollouts(ctx, clients, namespace, hpas)
			if e != nil && !isOptionalAPIError(e) {
				return nil, e
			}
			result = append(result, rollouts...)
		}
	}
	if l.cfg.Namespaces == "*" {
		filtered := result[:0]
		for _, item := range result {
			if item.Namespace != "kube-system" {
				filtered = append(filtered, item)
			}
		}
		result = filtered
	}
	model.SortObjects(result)
	return result, nil
}

func (l *Loader) selector() string {
	if l.cfg.Selector == nil {
		return ""
	}
	return *l.cfg.Selector
}
func (l *Loader) enabled(kind string) bool {
	if l.cfg.Resources == "*" {
		return true
	}
	for _, v := range l.cfg.ResourceValues {
		if v == kind {
			return true
		}
	}
	return false
}

func (l *Loader) namespaces(ctx context.Context, client kubernetes.Interface) ([]string, error) {
	if l.cfg.Namespaces == "*" {
		return []string{metav1.NamespaceAll}, nil
	}
	result := make([]string, 0, len(l.cfg.NamespaceValues))
	patterns := []*regexp.Regexp{}
	for _, value := range l.cfg.NamespaceValues {
		if config.IsNamespacePattern(value) {
			pattern, err := regexp.Compile(value)
			if err != nil {
				return nil, fmt.Errorf("invalid namespace pattern %q: %w", value, err)
			}
			patterns = append(patterns, pattern)
		} else {
			result = append(result, value)
		}
	}
	if len(patterns) > 0 {
		items, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, item := range items.Items {
			for _, pattern := range patterns {
				if pattern.MatchString(item.Name) && !contains(result, item.Name) {
					result = append(result, item.Name)
					break
				}
			}
		}
	}
	return result, nil
}

type hpaKey struct{ namespace, kind, name string }

func (l *Loader) hpas(ctx context.Context, client kubernetes.Interface) map[hpaKey]*model.HPA {
	result := map[hpaKey]*model.HPA{}
	items, err := client.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, item := range items.Items {
			h := hpaV2(item)
			result[hpaKey{item.Namespace, item.Spec.ScaleTargetRef.Kind, item.Spec.ScaleTargetRef.Name}] = h
		}
		return result
	}
	v1items, err := client.AutoscalingV1().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, item := range v1items.Items {
			result[hpaKey{item.Namespace, item.Spec.ScaleTargetRef.Kind, item.Spec.ScaleTargetRef.Name}] = hpaV1(item)
		}
	}
	return result
}
func hpaV2(item autoscalingv2.HorizontalPodAutoscaler) *model.HPA {
	h := &model.HPA{MinReplicas: item.Spec.MinReplicas, MaxReplicas: item.Spec.MaxReplicas, CurrentReplicas: &item.Status.CurrentReplicas, DesiredReplicas: item.Status.DesiredReplicas}
	for _, metric := range item.Spec.Metrics {
		if metric.Type != autoscalingv2.ResourceMetricSourceType || metric.Resource == nil || metric.Resource.Target.AverageUtilization == nil {
			continue
		}
		switch metric.Resource.Name { //nolint:exhaustive // Only CPU and memory are relevant to kedr.
		case corev1.ResourceCPU:
			v := float64(*metric.Resource.Target.AverageUtilization)
			h.TargetCPUPercent = &v
		case corev1.ResourceMemory:
			v := float64(*metric.Resource.Target.AverageUtilization)
			h.TargetMemoryPercent = &v
		default:
			continue
		}
	}
	return h
}
func hpaV1(item autoscalingv1.HorizontalPodAutoscaler) *model.HPA {
	h := &model.HPA{MinReplicas: item.Spec.MinReplicas, MaxReplicas: item.Spec.MaxReplicas, CurrentReplicas: &item.Status.CurrentReplicas, DesiredReplicas: item.Status.DesiredReplicas}
	if item.Spec.TargetCPUUtilizationPercentage != nil {
		v := float64(*item.Spec.TargetCPUUtilizationPercentage)
		h.TargetCPUPercent = &v
	}
	return h
}

func allocations(container corev1.Container) model.Allocations {
	a := model.EmptyAllocations()
	if q, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
		a.Requests[model.CPU] = model.Number(q.AsApproximateFloat64())
	}
	if q, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
		a.Requests[model.Memory] = model.Number(q.AsApproximateFloat64())
	}
	if q, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
		a.Limits[model.CPU] = model.Number(q.AsApproximateFloat64())
	}
	if q, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
		a.Limits[model.Memory] = model.Number(q.AsApproximateFloat64())
	}
	return a
}
func selectorString(selector *metav1.LabelSelector) string {
	if selector == nil {
		return ""
	}
	parsed, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return ""
	}
	return parsed.String()
}
func (l *Loader) objects(cluster *string, meta metav1.ObjectMeta, kind, selector string, containers []corev1.Container, hpas map[hpaKey]*model.HPA) []model.Object {
	out := make([]model.Object, 0, len(containers))
	objectLabels := meta.Labels
	if objectLabels == nil {
		objectLabels = map[string]string{}
	}
	objectAnnotations := meta.Annotations
	if objectAnnotations == nil {
		objectAnnotations = map[string]string{}
	}
	for _, container := range containers {
		out = append(out, model.Object{Cluster: cluster, Name: meta.Name, Namespace: meta.Namespace, Kind: kind, Container: container.Name, Image: container.Image, Pods: []model.Pod{}, Allocations: allocations(container), HPA: hpas[hpaKey{meta.Namespace, kind, meta.Name}], Warnings: []string{}, Labels: objectLabels, Annotations: objectAnnotations, Selector: selector, UID: string(meta.UID), Generation: meta.Generation, ObservedAt: time.Now().UnixMilli()})
	}
	return out
}
func (l *Loader) fromDeployment(c *string, x *appsv1.Deployment, h map[hpaKey]*model.HPA) []model.Object {
	out := l.objects(c, x.ObjectMeta, "Deployment", selectorString(x.Spec.Selector), x.Spec.Template.Spec.Containers, h)
	for i := range out {
		out[i].ObservedGeneration = x.Status.ObservedGeneration
		// Completion is a conservative activation boundary, including rollbacks
		// that reuse an old ReplicaSet. Pod replacement does not move it forward.
		for _, condition := range x.Status.Conditions {
			if condition.Type == appsv1.DeploymentProgressing && condition.Reason == "NewReplicaSetAvailable" && condition.Status == corev1.ConditionTrue && !condition.LastUpdateTime.IsZero() {
				out[i].ReleaseStartedAt = condition.LastUpdateTime.UnixMilli()
				out[i].ReleaseStartInferred = true
			}
		}
	}
	return out
}
func (l *Loader) fromStatefulSet(c *string, x *appsv1.StatefulSet, h map[hpaKey]*model.HPA) []model.Object {
	out := l.objects(c, x.ObjectMeta, "StatefulSet", selectorString(x.Spec.Selector), x.Spec.Template.Spec.Containers, h)
	for i := range out {
		out[i].ObservedGeneration = x.Status.ObservedGeneration
		if x.Status.UpdateRevision != "" {
			out[i].Release = "controller-revision-hash:" + x.Status.UpdateRevision
		}
	}
	return out
}
func (l *Loader) fromDaemonSet(c *string, x *appsv1.DaemonSet, h map[hpaKey]*model.HPA) []model.Object {
	out := l.objects(c, x.ObjectMeta, "DaemonSet", selectorString(x.Spec.Selector), x.Spec.Template.Spec.Containers, h)
	for i := range out {
		out[i].ObservedGeneration = x.Status.ObservedGeneration
		out[i].IdentityAmbiguous = x.Status.UpdatedNumberScheduled < x.Status.DesiredNumberScheduled
	}
	return out
}
func (l *Loader) fromJob(c *string, x *batchv1.Job, kind string, h map[hpaKey]*model.HPA) []model.Object {
	out := l.objects(c, x.ObjectMeta, kind, selectorString(x.Spec.Selector), x.Spec.Template.Spec.Containers, h)
	for i := range out {
		out[i].Release = "template:" + templateHash(x.Spec.Template)
		out[i].ObservedGeneration = x.Generation
	}
	return out
}
func (l *Loader) fromCronJob(c *string, x *batchv1.CronJob, h map[hpaKey]*model.HPA) []model.Object {
	out := l.objects(c, x.ObjectMeta, "CronJob", "", x.Spec.JobTemplate.Spec.Template.Spec.Containers, h)
	for i := range out {
		out[i].TemplateHash = templateHash(x.Spec.JobTemplate.Spec.Template)
		out[i].Release = "template:" + out[i].TemplateHash
		out[i].ObservedGeneration = x.Generation
	}
	return out
}

func (l *Loader) jobs(ctx context.Context, client kubernetes.Interface, namespace string) ([]batchv1.Job, error) {
	var all []batchv1.Job
	token := ""
	for batch := 0; batch < l.cfg.DiscoveryJobMaxBatches; batch++ {
		items, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: l.selector(), Limit: int64(l.cfg.DiscoveryJobBatchSize), Continue: token})
		if err != nil {
			return nil, err
		}
		all = append(all, items.Items...)
		token = items.Continue
		if token == "" {
			return all, nil
		}
	}
	return all, fmt.Errorf("job discovery exceeded %d batches", l.cfg.DiscoveryJobMaxBatches)
}
func ownedByCron(job *batchv1.Job) bool {
	for _, owner := range job.OwnerReferences {
		if owner.Kind == "CronJob" {
			return true
		}
	}
	return false
}
func (l *Loader) grouped(job *batchv1.Job) bool {
	for _, label := range l.cfg.JobGroupingLabels {
		if _, ok := job.Labels[label]; ok {
			return true
		}
	}
	return false
}
func (l *Loader) groupJobs(cluster *string, jobs []batchv1.Job, h map[hpaKey]*model.HPA) []model.Object {
	type group struct {
		label, value string
		jobs         []*batchv1.Job
	}
	groups := map[string]*group{}
	for i := range jobs {
		job := &jobs[i]
		if ownedByCron(job) {
			continue
		}
		for _, label := range l.cfg.JobGroupingLabels {
			value, ok := job.Labels[label]
			if !ok {
				continue
			}
			key := label + "=" + value
			g := groups[key]
			if g == nil {
				g = &group{label: label, value: value}
				groups[key] = g
			}
			if len(g.jobs) < l.cfg.JobGroupingLimit {
				g.jobs = append(g.jobs, job)
			}
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []model.Object
	for _, key := range keys {
		g := groups[key]
		if len(g.jobs) == 0 {
			continue
		}
		template := *g.jobs[0]
		template.Name = key
		template.Labels = map[string]string{g.label: g.value}
		objects := l.fromJob(cluster, &template, "GroupedJob", h)
		names := make([]string, 0, len(g.jobs))
		for _, job := range g.jobs {
			names = append(names, job.Name)
		}
		for i := range objects {
			objects[i].GroupedJobs = names
			objects[i].GroupingExpr = labels.Set{g.label: g.value}.String()
			// A name-based group of different Job UIDs is not a workload identity.
			objects[i].UID, objects[i].Release = "", ""
			objects[i].Warnings = append(objects[i].Warnings, "GroupedJobIdentityUnsupported")
		}
		out = append(out, objects...)
	}
	return out
}

var rolloutGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}

func (l *Loader) rollouts(ctx context.Context, clients Clients, namespace string, h map[hpaKey]*model.HPA) ([]model.Object, error) {
	items, err := clients.Dynamic.Resource(rolloutGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: l.selector()})
	if err != nil {
		return nil, err
	}
	var out []model.Object
	for _, item := range items.Items {
		raw := item.Object
		spec, _ := raw["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		templateSpec, _ := template["spec"].(map[string]any)
		containersRaw, _ := templateSpec["containers"].([]any)
		containers := make([]corev1.Container, 0, len(containersRaw))
		for _, entry := range containersRaw {
			blob, marshalErr := jsonMarshal(entry)
			if marshalErr != nil {
				continue
			}
			var container corev1.Container
			if jsonUnmarshal(blob, &container) == nil {
				containers = append(containers, container)
			}
		}
		if len(containers) == 0 {
			if ref, ok := spec["workloadRef"].(map[string]any); ok {
				if name, ok := ref["name"].(string); ok && name != "" {
					deployment, readErr := clients.Typed.AppsV1().Deployments(item.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
					if readErr != nil {
						return nil, readErr
					}
					containers = deployment.Spec.Template.Spec.Containers
				}
			}
		}
		selector := ""
		if rawSelector, ok := spec["selector"].(map[string]any); ok {
			if matches, ok := rawSelector["matchLabels"].(map[string]any); ok {
				set := labels.Set{}
				for k, v := range matches {
					set[k] = fmt.Sprint(v)
				}
				selector = set.String()
			}
		}
		meta := metav1.ObjectMeta{
			Name: item.GetName(), Namespace: item.GetNamespace(), UID: item.GetUID(),
			Labels: item.GetLabels(), Annotations: item.GetAnnotations(),
		}
		meta.Generation = item.GetGeneration()
		objects := l.objects(clients.Name, meta, "Rollout", selector, containers, h)
		status, _ := raw["status"].(map[string]any)
		for i := range objects {
			if hash, ok := status["currentPodHash"].(string); ok && hash != "" {
				objects[i].Release = "rollouts-pod-template-hash:" + hash
			}
			if generation, ok := status["observedGeneration"].(int64); ok {
				objects[i].ObservedGeneration = generation
			}
		}
		out = append(out, objects...)
	}
	return out, nil
}

func isOptionalAPIError(err error) bool {
	return strings.Contains(err.Error(), "the server could not find the requested resource") || strings.Contains(err.Error(), "forbidden") || strings.Contains(err.Error(), "not found")
}

func (l *Loader) LoadCurrentPods(ctx context.Context, clients Clients, object model.Object) ([]model.Pod, error) {
	if object.Kind == "CronJob" {
		jobs, err := clients.Typed.BatchV1().Jobs(object.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		uids := make([]string, 0)
		for _, job := range jobs.Items {
			for _, owner := range job.OwnerReferences {
				if owner.Kind == "CronJob" && string(owner.UID) == object.UID {
					uids = append(uids, string(job.UID))
					break
				}
			}
		}
		if len(uids) == 0 {
			return nil, nil
		}
		selector := "batch.kubernetes.io/controller-uid in (" + strings.Join(uids, ",") + ")"
		items, err := clients.Typed.CoreV1().Pods(object.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, err
		}
		return pods(items.Items), nil
	}
	if object.Kind == "GroupedJob" && object.GroupingExpr != "" {
		items, err := clients.Typed.CoreV1().Pods(object.Namespace).List(ctx, metav1.ListOptions{LabelSelector: object.GroupingExpr, Limit: int64(l.cfg.JobGroupingLimit)})
		if err != nil {
			return nil, err
		}
		return pods(items.Items), nil
	}
	if object.Selector == "" {
		return nil, nil
	}
	items, err := clients.Typed.CoreV1().Pods(object.Namespace).List(ctx, metav1.ListOptions{LabelSelector: object.Selector})
	if err != nil {
		return nil, err
	}
	return pods(items.Items), nil
}
func pods(items []corev1.Pod) []model.Pod {
	out := make([]model.Pod, 0, len(items))
	for _, item := range items {
		out = append(out, model.Pod{Name: item.Name})
	}
	return out
}

func DiscoverMetricsURL(ctx context.Context, clients Clients) (string, error) {
	selectors := []string{"app.kubernetes.io/name=vmsingle", "app.kubernetes.io/name=victoria-metrics-single", "app.kubernetes.io/name=vmselect", "app.kubernetes.io/component=vmselect", "app=vmselect", "app.kubernetes.io/component=query,app.kubernetes.io/name=thanos", "app.kubernetes.io/name=thanos-query", "app=thanos-query", "app=thanos-querier", "app.kubernetes.io/name=mimir,app.kubernetes.io/component=query-frontend", "app=kube-prometheus-stack-prometheus", "app=prometheus,component=server", "app=prometheus-server", "app=prometheus-operator-prometheus", "app=rancher-monitoring-prometheus", "app=prometheus-prometheus", "app.kubernetes.io/name=prometheus,app.kubernetes.io/component=server", "app=stack-prometheus"}
	// Fetch each inventory once and evaluate selector priority locally. Repeating
	// LIST for every selector adds dozens of API round trips before a scan starts.
	services, err := clients.Typed.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("discover metrics services: %w", err)
	}
	var ingresses *networkingv1.IngressList
	ingressesLoaded := false
	for _, selector := range selectors {
		matcher, err := labels.Parse(selector)
		if err != nil {
			return "", err
		}
		for _, svc := range services.Items {
			if !matcher.Matches(labels.Set(svc.Labels)) || len(svc.Spec.Ports) == 0 {
				continue
			}
			url := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc.Name, svc.Namespace, svc.Spec.Ports[0].Port)
			if clients.Name != nil {
				url = fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s:%d/proxy", strings.TrimRight(clients.REST.Host, "/"), svc.Namespace, svc.Name, svc.Spec.Ports[0].Port)
			}
			if strings.Contains(selector, "vmselect") {
				url += "/select/0/prometheus"
			}
			if strings.Contains(selector, "mimir") {
				url += "/prometheus"
			}
			return url, nil
		}
		if clients.Name != nil {
			if !ingressesLoaded {
				ingresses, _ = clients.Typed.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
				ingressesLoaded = true
			}
			if ingresses == nil {
				continue
			}
			for _, ingress := range ingresses.Items {
				if !matcher.Matches(labels.Set(ingress.Labels)) || len(ingress.Spec.Rules) == 0 {
					continue
				}
				host := ingress.Spec.Rules[0].Host
				if host != "" {
					url := "http://" + host
					if strings.Contains(selector, "mimir") {
						url += "/prometheus"
					}
					return url, nil
				}
			}
		}
	}
	return "", errors.New("prometheus-compatible service was not found")
}

func KubernetesHTTPClient(clients Clients) (*http.Client, error) {
	return rest.HTTPClientFor(clients.REST)
}

var jsonMarshal = func(v any) ([]byte, error) { return json.Marshal(v) }
var jsonUnmarshal = func(data []byte, v any) error { return json.Unmarshal(data, v) }

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
