package model

import "testing"

func TestSortObjectsFromScansSortsObjectsAndPodsByName(t *testing.T) {
	clusterA, clusterB := "a", "b"
	scans := []Scan{
		{Object: Object{Name: "zeta", Namespace: "a", Pods: []Pod{{Name: "zeta-pod"}, {Name: "alpha-pod"}}, Warnings: []string{"NoPrometheusMemoryMetrics", "NoPrometheusCPUMetrics"}}},
		{Object: Object{Name: "alpha", Cluster: &clusterB, Namespace: "z"}},
		{Object: Object{Name: "alpha", Cluster: &clusterA, Namespace: "a"}},
	}

	SortObjectsFromScans(scans)

	if scans[0].Object.Name != "alpha" || scans[0].Object.Cluster == nil || *scans[0].Object.Cluster != "a" {
		t.Fatalf("unexpected first scan: %+v", scans[0].Object)
	}
	if scans[1].Object.Name != "alpha" || scans[1].Object.Cluster == nil || *scans[1].Object.Cluster != "b" {
		t.Fatalf("unexpected second scan: %+v", scans[1].Object)
	}
	if scans[2].Object.Name != "zeta" {
		t.Fatalf("unexpected last scan: %+v", scans[2].Object)
	}
	if got := scans[2].Object.Pods; got[0].Name != "alpha-pod" || got[1].Name != "zeta-pod" {
		t.Fatalf("pods not sorted by name: %+v", got)
	}
	if got := scans[2].Object.Warnings; got[0] != "NoPrometheusCPUMetrics" || got[1] != "NoPrometheusMemoryMetrics" {
		t.Fatalf("warnings not sorted: %+v", got)
	}
}
