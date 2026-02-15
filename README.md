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

Contributions are welcome! Please see the [Dorgu CLI CONTRIBUTING.md](https://github.com/dorgu-ai/dorgu/blob/main/CONTRIBUTING.md) for general guidelines.

For operator-specific development:
1. Fork and clone the repository
2. Make changes to `api/` or `internal/controller/`
3. Run `make manifests generate` after modifying types
4. Run `make test` to ensure tests pass
5. Submit a pull request

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
