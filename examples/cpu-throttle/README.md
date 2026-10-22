# CPU throttling demo

This Deployment keeps one pod calculating decimal digits of π without sleeping. It prints another 50 digits about every 30 seconds, then starts again after 2,000 digits so memory stays bounded. The official `python:3.13-slim-bookworm` image runs the script from a ConfigMap; no image build is needed.

The container requests 10m CPU and has a **20m CPU limit**. On a Linux node, the process continually asks for more CPU time than its cgroup quota allows. Kubernetes enforces CPU limits by throttling rather than terminating the container. Its observed usage should stay near 20m while its cAdvisor throttling counters rise. The 128 MiB memory limit is generous for this bounded calculation. See the [Kubernetes resource management guide](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/) for how CPU requests and limits are enforced.

## Run it

From the repository root:

```sh
kubectl apply -f examples/cpu-throttle/manifest.yaml
kubectl -n kedr-cpu-throttle-demo rollout status deployment/cpu-throttle-demo
kubectl -n kedr-cpu-throttle-demo logs deployment/cpu-throttle-demo -f
```

To compare reported usage with the limit:

```sh
kubectl -n kedr-cpu-throttle-demo top pod -l app=cpu-throttle-demo
```

`kubectl top` needs Metrics Server and shows CPU actually used, not CPU demand or throttling. To see the throttling, use a Prometheus-compatible service scraping the kubelet's `/metrics/cadvisor` endpoint. These example PromQL queries select the demo container:

```promql
# Share of CPU quota periods in which the container was throttled.
rate(container_cpu_cfs_throttled_periods_total{namespace="kedr-cpu-throttle-demo",container="cpu-throttle-demo"}[5m])
/
rate(container_cpu_cfs_periods_total{namespace="kedr-cpu-throttle-demo",container="cpu-throttle-demo"}[5m])

# Seconds spent throttled per second, averaged over five minutes.
rate(container_cpu_cfs_throttled_seconds_total{namespace="kedr-cpu-throttle-demo",container="cpu-throttle-demo"}[5m])

# Actual CPU usage in cores; expect roughly 0.02 when the limit is enforced.
rate(container_cpu_usage_seconds_total{namespace="kedr-cpu-throttle-demo",container="cpu-throttle-demo"}[5m])
```

The [cAdvisor metric reference](https://github.com/google/cadvisor/blob/master/docs/storage/prometheus.md) defines these counters. A high throttled-period share alongside usage near 0.02 cores shows the intended effect. Availability and labels depend on the cluster's metric pipeline; the [Kubernetes resource metrics pipeline](https://kubernetes.io/docs/tasks/debug/debug-cluster/resource-metrics-pipeline/) explains why `kubectl top` alone does not expose these counters.

KEDR can scan the pod's observed CPU usage with `kedr simple --namespace kedr-cpu-throttle-demo --full --explain`, but that usage is capped by the limit and cannot by itself reveal how much CPU the calculation wanted.

Remove the demo when finished:

```sh
kubectl delete namespace kedr-cpu-throttle-demo
```

## Classification

Add `--classify-workloads` to the scan command to include advisory feature tags. Expect `cpu-throttled` when the Prometheus source exposes the cAdvisor counters above. The saved `explain` evidence reports throttled seconds per second and throttled-period percentage when available.
