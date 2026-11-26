package kubernetes

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kedify/kedr/internal/model"
)

func hpaOwner(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	owner := metav1.GetControllerOf(hpa)
	switch {
	case owner != nil && owner.APIVersion == "keda.sh/v1alpha1" && owner.Kind == "ScaledObject" && owner.UID != "" && owner.Name != "":
		return "ScaledObject " + owner.Name
	case owner != nil || hpa.Labels["app.kubernetes.io/managed-by"] == "keda-operator":
		return "unknown owner"
	default:
		return "standalone"
	}
}

func observeHPA(hpa *autoscalingv2.HorizontalPodAutoscaler) model.HPAObservation {
	result := model.HPAObservation{UID: string(hpa.UID), Name: hpa.Name, Owner: hpaOwner(hpa),
		MinReplicas: 1, MaxReplicas: hpa.Spec.MaxReplicas}
	if hpa.Spec.MinReplicas != nil {
		result.MinReplicas = *hpa.Spec.MinReplicas
	}
	// A controller may publish status without either HPA generation.
	statusCurrent := hpa.Status.ObservedGeneration != nil && *hpa.Status.ObservedGeneration == hpa.Generation ||
		hpa.Status.ObservedGeneration == nil && hpa.Generation == 0 && len(hpa.Status.Conditions) > 0
	if statusCurrent {
		result.CurrentReplicas, result.DesiredReplicas = &hpa.Status.CurrentReplicas, &hpa.Status.DesiredReplicas
	}
	for _, spec := range hpa.Spec.Metrics {
		var resource corev1.ResourceName
		var container string
		var target autoscalingv2.MetricTarget
		switch {
		case spec.Type == autoscalingv2.ResourceMetricSourceType && spec.Resource != nil:
			resource, target = spec.Resource.Name, spec.Resource.Target
		case spec.Type == autoscalingv2.ContainerResourceMetricSourceType && spec.ContainerResource != nil:
			resource, container, target = spec.ContainerResource.Name, spec.ContainerResource.Container, spec.ContainerResource.Target
		default:
			continue
		}
		if resource != corev1.ResourceCPU && resource != corev1.ResourceMemory {
			continue
		}
		metric := model.HPAMetricObservation{Type: string(spec.Type), Resource: model.ResourceType(resource), Container: container, TargetType: string(target.Type)}
		var current autoscalingv2.MetricValueStatus
		if statusCurrent {
			current = hpaCurrentMetric(hpa.Status.CurrentMetrics, spec.Type, resource, container)
		}
		switch {
		case target.Type == autoscalingv2.UtilizationMetricType && target.AverageUtilization != nil:
			metric.Target = float64(*target.AverageUtilization)
			if current.AverageUtilization != nil {
				value := float64(*current.AverageUtilization)
				metric.Current = &value
			}
		case target.Type == autoscalingv2.AverageValueMetricType && target.AverageValue != nil:
			metric.Target = target.AverageValue.AsApproximateFloat64()
			if current.AverageValue != nil {
				value := current.AverageValue.AsApproximateFloat64()
				metric.Current = &value
			}
		default:
			continue
		}
		result.Metrics = append(result.Metrics, metric)
	}
	return result
}

func hpaCurrentMetric(metrics []autoscalingv2.MetricStatus, source autoscalingv2.MetricSourceType, resource corev1.ResourceName, container string) autoscalingv2.MetricValueStatus {
	for _, status := range metrics {
		if status.Type != source {
			continue
		}
		if status.Resource != nil && status.Resource.Name == resource && source == autoscalingv2.ResourceMetricSourceType {
			return status.Resource.Current
		}
		if status.ContainerResource != nil && status.ContainerResource.Name == resource && status.ContainerResource.Container == container && source == autoscalingv2.ContainerResourceMetricSourceType {
			return status.ContainerResource.Current
		}
	}
	return autoscalingv2.MetricValueStatus{}
}

func recordContainerFailure(failure *model.PodFailureObservation, status corev1.ContainerStatus) {
	latest := status.State.Terminated
	previous := status.LastTerminationState.Terminated
	if previous != nil && (latest == nil || latest.FinishedAt.Before(&previous.FinishedAt)) {
		latest = previous
	}
	if latest != nil && !latest.FinishedAt.IsZero() && (status.RestartCount > 0 || latest.Reason != "Completed") {
		failure.TerminatedAt = latest.FinishedAt.UnixMilli()
		switch latest.Reason {
		case "OOMKilled", "Error", "Completed", "ContainerCannotRun", "StartError", "DeadlineExceeded":
			failure.TerminationReason = latest.Reason
		case "":
			failure.TerminationReason = "Unknown"
		default:
			failure.TerminationReason = "Other"
		}
	}
	if status.RestartCount > 0 || failure.TerminatedAt != 0 {
		failure.Restarts = &status.RestartCount
	}
}
