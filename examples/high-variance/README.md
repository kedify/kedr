# High variance CPU and memory demo

This Deployment runs one pod with three containers. `memory-variance` changes its touched anonymous memory, `cpu-variance` changes its CPU load, and `cpu-memory-variance` changes both. Each follows a repeating six-step cycle, holding each level for 20 seconds:

| Step | Touched memory | Target CPU |
| --- | ---: | ---: |
| 1 | 10 MiB | 30m |
| 2 | 150 MiB | 650m |
| 3 | 20 MiB | 70m |
| 4 | 300 MiB | 800m |
| 5 | 40 MiB | 120m |
| 6 | 240 MiB | 500m |

Only the relevant column applies to each single-resource container. The script releases the previous anonymous mapping before allocating the next one, so memory falls as well as rises. CPU work is paced in 100 ms slices. The official `python:3.13-slim-bookworm` image runs the script from a ConfigMap; no image build is needed.

The memory numbers are allocations, not total container usage: Python and the container runtime add overhead. Both memory-varying containers can reach 300 MiB at once. Each has a 384 MiB memory limit; the CPU-only container has a 96 MiB limit. The pod has a 768 MiB memory limit, leaving room above the roughly 600 MiB of peak allocations plus process overhead. The CPU-varying containers each target at most 800m against 900m limits; the pod limit is 2 CPUs. The manifest also specifies CPU and memory requests at both container and pod level.

Pod-level CPU and memory requests and limits require Kubernetes 1.34 or newer with `PodLevelResources` enabled (enabled by default in 1.34). See the [Kubernetes pod-level resources guide](https://kubernetes.io/docs/tasks/configure-pod-container/assign-pod-level-resources/).

## Run it

From the repository root:

```sh
kubectl apply -f examples/high-variance/manifest.yaml
kubectl -n kedr-high-variance-demo rollout status deployment/high-variance-demo
kubectl -n kedr-high-variance-demo logs deployment/high-variance-demo --all-containers=true --prefix --follow
```

In another terminal, inspect container usage (requires Metrics Server); repeat the command to see different phases:

```sh
kubectl -n kedr-high-variance-demo top pods -l app=high-variance-demo --containers
```

For time series, use Prometheus container CPU and memory metrics. A 5–10 second scrape interval captures the 20-second phases more clearly; CPU rates over long windows smooth the jumps. Let the pod run for at least an hour with enough samples before asking KEDR for recommendations:

```sh
kedr simple --namespace kedr-high-variance-demo --full --explain
```

KEDR reports one row per container. The `explain` chart for each row shows the observed CPU and memory pattern. Exact values depend on scrape timing, CPU throttling on a busy node, and process overhead. Remove the demo when finished:

```sh
kubectl delete namespace kedr-high-variance-demo
```

## Classification

Add `--classify-workloads` to the scan command to include advisory feature tags. After at least 35 minutes of usable observations, expect `high-variance-memory` for `memory-variance`, `high-variance-cpu` for `cpu-variance`, and both for `cpu-memory-variance`. Exact CPU behavior depends on node contention.
