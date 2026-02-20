# dorgu-operator

Kubernetes operator that validates Deployments against ApplicationPersona CRDs, manages ClusterPersona for cluster identity, and provides real-time integration with ArgoCD, Prometheus, and CLI tools.

## Description

The Dorgu Operator is the cluster-side component of the [Dorgu](https://github.com/dorgu-ai/dorgu) project. It watches `ApplicationPersona` and `ClusterPersona` Custom Resources, validates deployments, and provides the "Cluster Soul" foundation for AI-powered Kubernetes management.

**Key features:**
- **ApplicationPersona validation** — Checks resource limits, replica counts, health probes, and security context against persona constraints
- **ClusterPersona discovery** — Automatically discovers cluster state including nodes, add-ons (ArgoCD, Prometheus, cert-manager), and resource usage
- **ArgoCD integration** — Watches ArgoCD Applications and updates persona status with sync status and health
- **Prometheus baseline learning** — Queries Prometheus for resource usage metrics to establish baselines
- **WebSocket server** — Real-time communication with CLI for `dorgu watch` and `dorgu sync` commands
- **Status reporting** — Updates persona status with validation results, health information, and recommendations
- **Optional webhook** — Can run in advisory mode (warnings only) or enforcing mode (reject non-compliant deployments)
- **Non-invasive** — The operator reads and validates only; it does not modify workloads

**Integration with Dorgu CLI:**
```bash
# Generate and apply a persona from your application
dorgu persona apply ./my-app --namespace production

# Check persona status
dorgu persona status my-app -n production

# Initialize cluster persona
dorgu cluster init --name production-cluster --environment production

# Watch real-time updates
dorgu watch personas

# Sync with operator
dorgu sync status
```

## CRDs

### ApplicationPersona
Represents the identity and requirements of an application:
- Resource constraints (CPU, memory limits)
- Scaling parameters (min/max replicas)
- Health probe configuration
- Security policies
- Ownership and team information

### ClusterPersona
Represents the identity and state of a Kubernetes cluster:
- Cluster policies and conventions
- Node information and resource capacity
- Discovered add-ons (ArgoCD, Prometheus, etc.)
- Application count and namespace summary

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+
- kubectl version v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster

### Install with Helm (Recommended)

```bash
# Install the operator
helm install dorgu-operator oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
  --version 0.2.0 \
  --namespace dorgu-system \
  --create-namespace
```

**With optional features enabled:**

```bash
helm install dorgu-operator oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
  --version 0.2.0 \
  --namespace dorgu-system \
  --create-namespace \
  --set webhook.enabled=true \
  --set webhook.mode=advisory \
  --set prometheus.enabled=true \
  --set prometheus.url=http://prometheus-server.monitoring:9090 \
  --set websocket.enabled=true
```

### Public packages (for installs without login)

The release workflow runs on tag push and publishes the **container image** and **Helm chart** to GitHub Container Registry (GHCR). For users to install without authenticating, both packages must be **Public** in GitHub:

- **Container image:** `ghcr.io/dorgu-ai/dorgu-operator` — In GitHub: go to the package (from the repo or org), Package settings → Change visibility → **Public**.
- **Helm chart (OCI):** The chart is pushed to `ghcr.io/<org>/dorgu-operator-charts`. Ensure that package is also set to **Public** so `helm install oci://ghcr.io/...` works without `helm registry login`.

If your org uses GitHub Releases for chart or binary artifacts, keep those releases public as well.

### Configuration Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `webhook.enabled` | Enable deployment validation webhook | `false` |
| `webhook.mode` | Webhook mode: `advisory` or `enforcing` | `advisory` |
| `argocd.enabled` | Enable ArgoCD Application watching | `true` |
| `prometheus.enabled` | Enable Prometheus metrics integration | `false` |
| `prometheus.url` | Prometheus server URL | `""` |
| `websocket.enabled` | Enable WebSocket server for CLI | `false` |
| `websocket.port` | WebSocket server port | `9090` |

### Deploy from Source

**Build and push your image:**

```sh
make docker-build docker-push IMG=<some-registry>/dorgu-operator:tag
```

**Install the CRDs:**

```sh
make install
```

**Deploy the Manager:**

```sh
make deploy IMG=<some-registry>/dorgu-operator:tag
```

**Create sample resources:**

```sh
kubectl apply -k config/samples/
```

### Uninstall

```sh
# Delete sample resources
kubectl delete -k config/samples/

# Delete CRDs
make uninstall

# Undeploy controller
make undeploy

# Or with Helm
helm uninstall dorgu-operator -n dorgu-system
```

## Architecture

```
┌─────────────────┐     ┌──────────────────────────────────────┐
│   dorgu CLI     │     │           Kubernetes Cluster          │
│                 │     │                                        │
│  dorgu watch    │◀───▶│  ┌──────────────────────────────────┐ │
│  dorgu sync     │ WS  │  │       Dorgu Operator             │ │
│                 │     │  │                                  │ │
│  dorgu persona  │     │  │  - ApplicationPersona Controller │ │
│  dorgu cluster  │────▶│  │  - ClusterPersona Controller     │ │
│                 │     │  │  - ArgoCD Watcher                │ │
│                 │     │  │  - Prometheus Client             │ │
│                 │     │  │  - WebSocket Server              │ │
│                 │     │  └──────────────────────────────────┘ │
│                 │     │                 │                      │
│                 │     │    ┌────────────┼────────────┐        │
│                 │     │    ▼            ▼            ▼        │
│                 │     │  ArgoCD    Prometheus   Deployments   │
│                 │     │                                        │
└─────────────────┘     └────────────────────────────────────────┘
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to contribute. It references the [Dorgu CLI contributing guidelines](https://github.com/dorgu-ai/dorgu/blob/main/CONTRIBUTING.md) for general practices and adds operator-specific steps (fork, branch, `make manifests generate`, `make test`, PR).

**Security issues:** See [SECURITY.md](SECURITY.md).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for the full text.
