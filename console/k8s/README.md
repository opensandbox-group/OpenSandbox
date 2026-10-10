# Console Kubernetes manifests

Example resources for the **same cluster and namespace** as the lifecycle server (default **`opensandbox-system`**; change as needed).

## Files

| File | Purpose |
|------|---------|
| `console-bff-secret.example.yaml` | `BFF_SESSION_SECRET`, `BFF_ADMIN_TOKEN` |
| `tenants-configmap.example.yaml` | Example `opensandbox-tenants` shape |
| `console-bff-deployment.example.yaml` | BFF Deployment + Service |
| `console-web-deployment.example.yaml` | Web Deployment + Service |
| `console-ingress.example.yaml` | Ingress example |

Build and image push: [../docs/06-build-and-deploy.md](../docs/06-build-and-deploy.md).

## Apply order

```bash
cd console

kubectl apply -f k8s/console-bff-secret.example.yaml
kubectl apply -f k8s/console-bff-deployment.example.yaml
kubectl apply -f k8s/console-web-deployment.example.yaml
kubectl apply -f k8s/console-ingress.example.yaml
```

## Before apply

- Replace `YOUR_REGISTRY/...` image tags
- Set `LIFECYCLE_API_BASE`, `BFF_CORS_ORIGINS`, `BFF_K8S_*_DEPLOYMENT`
- Set Ingress `host`
- Align [../web/nginx.conf](../web/nginx.conf) BFF upstream with Service DNS

## Verify

```bash
kubectl rollout status deployment/opensandbox-console-bff -n opensandbox-system
kubectl rollout status deployment/opensandbox-console-web -n opensandbox-system
kubectl port-forward -n opensandbox-system svc/opensandbox-console-bff 8091:8091
curl -s http://127.0.0.1:8091/health
```
