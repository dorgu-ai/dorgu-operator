# dorgu-operator

Kubernetes operator that validates Deployments against ApplicationPersona CRDs, ensuring applications conform to their declared resource, scaling, health, and security requirements.

## Description

The Dorgu Operator is the cluster-side component of the [Dorgu](https://github.com/dorgu-ai/dorgu) project. It watches `ApplicationPersona` Custom Resources and validates that corresponding Deployments adhere to the constraints defined in the persona spec.

**Key features:**
- **Deployment validation** — Checks resource limits, replica counts, health probes, and security context against persona constraints
- **Status reporting** — Updates persona status with validation results, health information, and recommendations
- **Optional webhook** — Can run in advisory mode (warnings only) or enforcing mode (reject non-compliant deployments)
- **Non-invasive** — The operator reads and validates only; it does not modify workloads. ArgoCD, Flux, or kubectl remain responsible for deployments.

**Integration with Dorgu CLI:**
```bash
# Generate and apply a persona from your application
dorgu persona apply ./my-app --namespace production

# Check persona status
dorgu persona status my-app -n production
```

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/dorgu-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/dorgu-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/dorgu-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/dorgu-operator/<tag or branch>/dist/install.yaml
```

### Install with Helm (OCI)

The operator is published as a Helm chart to GitHub Container Registry (OCI). After a release tag (e.g. `v0.1.0`) is pushed, the chart is available at:

```bash
# Install the operator (replace <org> with your GitHub org, e.g. dorgu-ai)
helm install dorgu-operator oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
  --version 0.1.0 \
  --namespace dorgu-system \
  --create-namespace
```

If the chart is **private**, log in first:

```bash
echo $GITHUB_TOKEN | helm registry login ghcr.io -u YOUR_GITHUB_USER --password-stdin
```

To enable the optional Deployment validating webhook:

```bash
helm install dorgu-operator oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
  --version 0.1.0 \
  --namespace dorgu-system \
  --create-namespace \
  --set webhook.enabled=true \
  --set webhook.mode=advisory
```

**Uninstall:**

```bash
helm uninstall dorgu-operator -n dorgu-system
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

