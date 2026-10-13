# Leader election demo

This Deployment runs three identical pods. They use a Kubernetes `coordination.k8s.io/v1` Lease to choose one leader. The leader touches and retains 128 MiB of anonymous memory and burns about 200m CPU; the two followers only poll the Lease. The script runs from a ConfigMap in the official `python:3.13-slim-bookworm` image, so there is no image build.

The leader gives up its turn after ten minutes, releasing its memory and CPU work before competing again. Any healthy replica, including the previous leader, may win the next turn. The Lease itself is renewed about every five seconds and expires after 20 seconds. This shorter expiry matters when a pod is forcibly killed and cannot release the Lease. Normal pod deletion sends SIGTERM; the process releases its memory, stops the CPU work, and clears the Lease for an immediate handoff. After a forced kill, another pod normally takes over within about 20–26 seconds, depending on the last renewal and poll timing.

## Run it

From the repository root:

```sh
kubectl apply -f examples/leader-election/manifest.yaml
kubectl -n kedr-leader-election-demo rollout status deployment/leader-election-demo
kubectl -n kedr-leader-election-demo get lease leader-election-demo -w
```

In another terminal, compare the pods' CPU and memory usage after the leader starts:

```sh
kubectl -n kedr-leader-election-demo top pods -l app=leader-election-demo
kubectl -n kedr-leader-election-demo logs -l app=leader-election-demo --prefix --max-log-requests=3 -f
```

`kubectl top` needs Metrics Server. Prometheus container metrics can show the same difference over time, and KEDR can scan the Deployment with:

```sh
kedr simple --namespace kedr-leader-election-demo --full --explain
```

To see failover, delete the pod named in the Lease's `holderIdentity` field:

```sh
kubectl -n kedr-leader-election-demo get lease leader-election-demo -o jsonpath='{.spec.holderIdentity}{"\n"}'
kubectl -n kedr-leader-election-demo delete pod "$(kubectl -n kedr-leader-election-demo get lease leader-election-demo -o jsonpath='{.spec.holderIdentity}')"
```

For a forced kill, add `--force --grace-period=0` to the delete command. The remaining replicas then wait for the Lease to expire. A brief delay around a handoff is expected; Kubernetes Lease election does not fence a former leader from doing work during a network partition, so this is a resource-usage demonstration rather than a production coordination mechanism.

Remove the demo when finished:

```sh
kubectl delete namespace kedr-leader-election-demo
```
