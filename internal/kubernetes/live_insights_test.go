package kubernetes

import (
	"testing"

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
