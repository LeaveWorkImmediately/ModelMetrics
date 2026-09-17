# ModelMetrics

ModelMetrics is ModelPilot's internal metrics storage and query capability. The MVP uses one
Prometheus instance, but consumers address it as `model-metrics` rather than depending on the
implementation name.

## Deploy

```bash
kubectl apply -f deploy/kubernetes.yaml
kubectl -n monitoring rollout status deployment/model-metrics
kubectl -n monitoring get pvc model-metrics-data
```

The internal query endpoint is:

```text
http://model-metrics.monitoring.svc.cluster.local:9090
```

The service has no NodePort or Ingress. It discovers each running ModelFlow Router pod in the
`default` namespace and scrapes its named `metrics` port every 15 seconds.

Before deploying to a cluster with different Service or Pod CIDRs, update `NO_PROXY` and the
Kubernetes API `ipBlock` in the NetworkPolicy.
