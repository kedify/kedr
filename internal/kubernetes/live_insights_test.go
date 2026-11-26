package kubernetes

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kedify/kedr/internal/model"
)

func TestRecordContainerFailureSkipsNormalCompletion(t *testing.T) {
	now := metav1.Now()
	for _, tt := range []struct {
		name     string
		reason   string
		restarts int32
		want     bool
	}{
		{name: "successful job", reason: "Completed"},
		{name: "failed job", reason: "Error", want: true},
		{name: "oom without retry", reason: "OOMKilled", want: true},
		{name: "completed after restart", reason: "Completed", restarts: 1, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status := corev1.ContainerStatus{RestartCount: tt.restarts, State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: tt.reason, FinishedAt: now},
			}}
			var got model.PodFailureObservation
			recordContainerFailure(&got, status)
			if (got.Restarts != nil) != tt.want || (got.TerminatedAt != 0) != tt.want {
				t.Fatalf("failure evidence = %+v, want present %v", got, tt.want)
			}
		})
	}
}

func TestRolloutHPAReadsStatusWhenObservedGenerationIsAbsent(t *testing.T) {
	item := autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-hpa", Generation: 1, UID: "rollout-hpa-uid"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "rollout"},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 2,
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.AbleToScale}}},
	}
	got := hpaV2(item)
	if len(got.Live) != 1 || got.Live[0].CurrentReplicas == nil || *got.Live[0].CurrentReplicas != 2 {
		t.Fatalf("live rollout HPA = %+v", got.Live)
	}
	generation := int64(0)
	item.Status.ObservedGeneration = &generation
	got = hpaV2(item)
	if got.Live[0].CurrentReplicas != nil {
		t.Fatalf("outdated status was used: %+v", got.Live)
	}
}
