# HTTP and metrics classification demo

The `api` container serves `/api`, `/healthz`, and Prometheus samples at `/metrics` on port `http-api`. A second container serves only `/healthz` on port `http-healthz`. The Service selects both ports so the example exercises container attribution and probe exclusions.

```sh
kubectl apply -f examples/http-metrics/manifest.yaml
kubectl -n kedr-http-metrics-demo rollout status deployment/http-metrics-demo
kedr simple --namespace kedr-http-metrics-demo --classify-workloads --full
```

Expect `has-http-traffic, has-metrics` on the `api` row and neither tag on `health-only`. Classification can appear before enough history exists for sizing. HTTP is inferred from Service metadata; metrics require a successful response with valid metric samples. `kedr explain <row> --offline` shows the saved verification evidence without probing again.

The scanning identity needs permission to list Services and to `get` `pods/proxy` for endpoint verification. Probe failures are recorded in the classification evidence.

```sh
kubectl delete namespace kedr-http-metrics-demo
```
