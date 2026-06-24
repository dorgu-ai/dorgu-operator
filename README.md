# Dorgu Operator

Cluster-side component of [Dorgu](https://github.com/dorgu-ai/dorgu): validates Deployments against **ApplicationPersona** CRDs, manages **ClusterPersona** for cluster identity, **detects cluster health issues**, **diagnoses root causes**, and **tracks incidents** — all without modifying your workloads. Integrates with ArgoCD, Prometheus, metrics-server, and the CLI.

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

**Features:** Health detection (node failures, pod crashes, resource saturation, control plane) · Deterministic diagnosis with confidence scoring · Incident tracking via IncidentMemory CRDs · ApplicationPersona validation · ClusterPersona discovery · ArgoCD sync/health · Prometheus baseline learning · WebSocket server · Optional validating webhook

**CLI integration:**
```bash
# Cluster health at a glance
dorgu health

# View active incidents
dorgu incidents list
dorgu incidents describe im-default-api-oom-a3f2

# Generate and apply a persona
dorgu persona apply ./my-app --namespace production

# Watch real-time updates
dorgu watch personas
```

## CRDs

| CRD | Scope | Purpose |
|-----|-------|---------|
| **ApplicationPersona** | Namespaced | App identity and requirements: resources, scaling, health probes, security, ownership. Status includes validation, health, and active incident count. |
| **ClusterPersona** | Cluster | Cluster identity and state: nodes, add-ons, capacity, self-healing policy (mode, trust level, rollback config). |
| **IncidentMemory** | Namespaced | Detected incidents: signal, root cause, confidence score, affected resources, resolution tracking. Correlates to Personas. |
| **RemediationAction** | Namespaced | Remediation proposals: YAML diff, approval workflow, rollback spec, trust level requirements. Execution is implemented — approval → patch Persona → verify health → rollback on failure. |
| **DorguEvent** | Namespaced | Classified cluster events with severity, category, persona correlation, and TTL-based cleanup. |

## Getting Started

### Prerequisites

Go 1.21+ (for building), Helm 3.x, kubectl, and a Kubernetes cluster (1.11+).

### Install with Helm (Recommended)

```bash
# Install the operator
helm install dorgu-operator oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
  --version 0.6.0 \
  --namespace dorgu-system \
  --create-namespace
```

**With health detection enabled (recommended):**

```bash
helm install dorgu-operator oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
  --version 0.6.0 \
  --namespace dorgu-system \
  --create-namespace \
  --set healthCheck.enabled=true \
  --set websocket.enabled=true
```

**With all optional features:**

```bash
helm install dorgu-operator oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
  --version 0.6.0 \
  --namespace dorgu-system \
  --create-namespace \
  --set healthCheck.enabled=true \
  --set healthCheck.metricsServer=true \
  --set webhook.enabled=true \
  --set webhook.mode=advisory \
  --set prometheus.enabled=true \
  --set prometheus.url=http://prometheus-server.monitoring:9090 \
  --set websocket.enabled=true
```

**Public installs:** Image and Helm chart are published to GHCR on release. Set package visibility to **Public** in GitHub (package settings) so `helm install oci://...` works without login.

### AI configuration (Anthropic)

AI-enhanced diagnosis and AI-generated ordered remediation plans use Anthropic (Claude). The API key is injected as the `ANTHROPIC_API_KEY` env var from a Kubernetes Secret — it is **never** passed as an inline pod-spec arg (so it does not leak into `kubectl get pod -o yaml` or `helm get values`).

**Never commit a real key.** Copy the template to a gitignored `values-local.yaml` and fill it in:

```bash
cp charts/dorgu-operator/values-local.example.yaml values-local.yaml   # values-local.yaml is gitignored
```

**Preferred (production)** — create the Secret out-of-band and reference it; the key never touches Helm values:

```bash
kubectl create secret generic dorgu-llm \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... -n dorgu-system
```

```yaml
# values-local.yaml
llm:
  provider: claude
  existingSecret: dorgu-llm
  # existingSecretKey: ANTHROPIC_API_KEY  # default
aiRemediation:
  enabled: true
```

Then install/upgrade with the override file last so it wins:

```bash
helm upgrade --install dorgu-operator ./charts/dorgu-operator \
  -n dorgu-system --create-namespace \
  -f charts/dorgu-operator/values.yaml \
  -f values-local.yaml
```

**Dev alternative** — let the chart create the Secret from a raw key (set `llm.apiKey` + `llm.createSecret=true`). Note the raw key is then visible to `helm get values`; use `existingSecret` for anything real.

### Configuration Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `healthCheck.enabled` | Enable health detection, diagnosis, and incident tracking | `false` |
| `healthCheck.interval` | Health check reconciliation interval | `60s` |
| `healthCheck.metricsServer` | Enable metrics-server integration for usage-based detection | `true` |
| `webhook.enabled` | Enable deployment validation webhook | `false` |
| `webhook.mode` | Webhook mode: `advisory` or `enforcing` | `advisory` |
| `argocd.enabled` | Enable ArgoCD Application watching | `true` |
| `prometheus.enabled` | Enable Prometheus metrics integration | `false` |
| `prometheus.url` | Prometheus server URL | `""` |
| `websocket.enabled` | Enable WebSocket server for CLI | `false` |
| `websocket.port` | WebSocket server port | `9090` |
| `llm.provider` | AI provider; `claude` enables Anthropic AI, empty disables it | `""` |
| `llm.model` | Override the default model (empty = `claude-sonnet-4-6`) | `""` |
| `llm.existingSecret` | Name of a pre-created Secret holding the API key (**preferred**) | `""` |
| `llm.existingSecretKey` | Key within the Secret | `ANTHROPIC_API_KEY` |
| `llm.apiKey` | **Dev only:** raw key; requires `createSecret=true` to be stored in a Secret | `""` |
| `llm.createSecret` | When true + `apiKey` set, the chart creates a Secret for the key | `false` |
| `aiRemediation.enabled` | Enable AI-generated ordered remediation plans (needs `llm.provider=claude`) | `false` |

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

```mermaid
flowchart LR
  subgraph cli [dorgu CLI]
    health[dorgu health]
    incidents[dorgu incidents]
    watch[dorgu watch]
    persona[dorgu persona]
    clusterCmd[dorgu cluster]
  end

  subgraph k8s [Kubernetes Cluster]
    subgraph operator [Dorgu Operator]
      appCtrl[ApplicationPersona Controller]
      clusterCtrl[ClusterPersona Controller]
      healthRec[Health Check Reconciler]
      detEngine[Detection Engine]
      diagEngine[Diagnosis Engine]
      eventPipe[Event Pipeline]
      incidentCtrl[Incident Controller]
      argocdWatcher[ArgoCD Watcher]
      promClient[Prometheus Client]
      wsServer[WebSocket Server]
    end
    subgraph crds [CRDs]
      appPersona[ApplicationPersona]
      clusterPersona[ClusterPersona]
      incidentMem[IncidentMemory]
      dorguEvent[DorguEvent]
    end
    argocd[ArgoCD]
    metricsServer[metrics-server]
  end

  health --> incidentMem
  health --> clusterPersona
  incidents --> incidentMem
  incidents --> dorguEvent
  watch <-->|WebSocket| wsServer
  persona --> appCtrl
  clusterCmd --> clusterCtrl
  healthRec --> detEngine
  detEngine --> diagEngine
  diagEngine --> incidentCtrl
  eventPipe --> dorguEvent
  incidentCtrl --> incidentMem
  detEngine --> metricsServer
```

## Contributing & Security

[CONTRIBUTING.md](CONTRIBUTING.md) — operator-specific steps and link to [CLI contributing guidelines](https://github.com/dorgu-ai/dorgu/blob/main/CONTRIBUTING.md).  
[SECURITY.md](SECURITY.md) — how to report vulnerabilities.

**License:** Apache 2.0 — [LICENSE](LICENSE).
