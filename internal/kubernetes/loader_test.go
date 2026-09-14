package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kedify/kedr/internal/config"
)

func TestDeploymentDiscovery(t *testing.T) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}}}}}}
	typed := fake.NewSimpleClientset(deployment)
	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cfg := config.Default("simple")
	cfg.NamespaceValues = []string{"default"}
	cfg.Namespaces = []string{"default"}
	cfg.ResourceValues = []string{"Deployment"}
	cfg.Resources = []string{"Deployment"}
	loader := NewLoader(cfg)
	name := "test"
	objects, err := loader.List(context.Background(), Clients{Name: &name, Typed: typed, Dynamic: dynamic})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[0].Kind != "Deployment" || objects[0].Selector != "app=api" {
		t.Fatalf("objects=%+v", objects)
	}
	data, err := json.Marshal(objects[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || objects[0].Pods == nil || objects[0].Labels == nil || objects[0].Annotations == nil {
		t.Fatalf("stable empty collections were not initialized: %s", data)
	}
}
