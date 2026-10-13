# OOM kill demo

This Deployment uses the same retained-memory pattern as the [memory leak demo](../memory-leak/README.md), but with a 128 MiB container limit so it can run on a small cluster such as k3d. It allocates and touches 24 MiB at startup, then another 21 MiB every minute. The retained blocks total 108 MiB after four minutes and exceed the limit (129 MiB) on the fifth step. Python's own memory usage may trigger the OOM kill a little earlier. Kubernetes restarts the container, so the cycle repeats.

## Run it

From the repository root:

```sh
kubectl apply -f examples/oom-kill/manifest.yaml
kubectl -n kedr-oom-kill-demo rollout status deployment/oom-kill-demo
kubectl -n kedr-oom-kill-demo get pods --watch
```

After about five minutes, check the previous container's termination reason and logs:

```sh
kubectl -n kedr-oom-kill-demo get pods -l app=oom-kill-demo -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].restartCount}{"\t"}{.status.containerStatuses[0].lastState.terminated.reason}{"\n"}{end}'
kubectl -n kedr-oom-kill-demo logs deployment/oom-kill-demo --previous
```

The reason should be `OOMKilled`. To see the event in KEDR, run a scan while the restarted pod still exists. A configured Prometheus endpoint is needed for normal resource sizing, but Kubernetes pod status can supply the OOM event directly:

```sh
kedr simple --namespace kedr-oom-kill-demo --use-oomkill-data --full --explain
```

The `OOMKilled` column shows the latest event. The default OOM memory buffer may recommend a higher request, but KEDR does not apply resource changes.

Remove the demo when finished:

```sh
kubectl delete namespace kedr-oom-kill-demo
```
