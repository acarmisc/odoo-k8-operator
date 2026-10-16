# Odoo Kubernetes Operator

A Kubernetes operator for managing Odoo instances and their custom addons.

## Overview

The Odoo Kubernetes Operator simplifies deploying and managing multiple Odoo instances along with their custom addons. It automates the creation of Odoo deployments, services, and manages addon synchronization from Git repositories.

## Features

- **OdooInstance Management**: Deploy Odoo containers with customizable configuration
- **Custom Addon Sync**: Clone and manage addons from Git repositories
- **Volume Management**: Addons volume mounted as read-only for Odoo pods, read-write for the operator
- **PostgreSQL Integration**: Configure external PostgreSQL connections
- **Custom Resources**: Full CRUD operations via Kubernetes CRDs

## Installation

### Prerequisites

- Kubernetes cluster v1.19+
- kubectl configured
- Docker (for building images)

### Quick Start

1. Install the CRDs:

```bash
make install
```

2. Build and deploy the operator:

```bash
make docker-build docker-push IMG=your-registry/odoo-k8-operator:latest
make deploy IMG=your-registry/odoo-k8-operator:latest
```

## Usage

### Create an Odoo Instance

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooInstance
metadata:
  name: my-odoo
  namespace: default
spec:
  image: odoo:17.0
  replicas: 1
  addonsVolume:
    size: 10Gi
    storageClass: standard
  postgres:
    host: postgres.default.svc.cluster.local
    port: 5432
    database: odoo
    user: odoo
    passwordSecret: odoo-db-secret
  config:
    limit_time_real: 120
    limit_time_cpu: 60
```

### Attach a Custom Addon

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: my-custom-addon
  namespace: default
spec:
  gitUrl: https://github.com/your-org/odoo-addon.git
  gitRef: main
  singleAddon: true
  instanceRef:
    name: my-odoo
    namespace: default
```

### Multi-Addons Repository

If your repository contains multiple addons:

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: my-addons-repo
spec:
  gitUrl: https://github.com/your-org/odoo-addons.git
  gitRef: 17.0
  addonPath: custom_addon_module
  singleAddon: false
  instanceRef:
    name: my-odoo
```

## API Reference

### OdooInstance

| Field | Type | Description |
|-------|------|-------------|
| `image` | string | Odoo Docker image (default: `odoo:17.0`) |
| `replicas` | int32 | Number of pods |
| `version` | string | Odoo version |
| `addonsVolume.size` | string | PVC size (default: `10Gi`) |
| `addonsVolume.storageClass` | string | Storage class name |
| `postgres` | object | PostgreSQL configuration |
| `config` | map[string]string | Odoo configuration |
| `resources` | object | CPU/memory requests/limits |

### OdooAddon

| Field | Type | Description |
|-------|------|-------------|
| `gitUrl` | string (required) | Git repository URL |
| `gitRef` | string | Branch/tag/commit (default: `main`) |
| `addonPath` | string | Path to addon in repository |
| `singleAddon` | bool | Repository contains single addon |
| `instanceRef.name` | string (required) | OdooInstance name |
| `instanceRef.namespace` | string | OdooInstance namespace |
| `readOnly` | bool | Mount as read-only (default: `true`) |

## Production notes

- **Data safety**: the filestore PVC (`<name>-data`) is *not* owned by the `OdooInstance` and survives its deletion
  (`dataVolume.retainOnDelete: true`, the default). Set it to `false` to garbage-collect it with the instance.
- **Replicas > 1** requires `ReadWriteMany` for both `addonsVolume.accessMode` and `dataVolume.accessMode`
  (enforced by CRD validation). With a single replica on RWO storage the Deployment uses the `Recreate` strategy.
- **Secrets**: `postgres.passwordSecret` and `adminPasswordSecret` (key `password`) are injected as env vars; the
  Odoo master password is never written to the ConfigMap. Changes to config, referenced Secrets or synced addon
  commits roll the pods automatically (pod-template hash annotations).
- **Defaults**: `proxy_mode = True` and `list_db = False` are set unless overridden in `spec.config`.
  Health probes use `/web/health`; port 8072 (`longpolling`) is exposed on the container and Service;
  a `PodDisruptionBudget` is created when `replicas > 1`.
- **Private repositories**: reference a Secret with `OdooAddon.spec.gitSecretRef` (never embed credentials in `gitUrl`).
  Pin `gitRef` to a commit SHA for reproducible deployments.
- **Atomic addon updates**: each sync stages modules in `.releases/<addon>/<sha>/` and switches symlinks with an atomic
  rename; Odoo never sees a half-copied module. The two latest releases are kept for rollback.
- The sync Job image defaults to `alpine/git:2.47.2`; override it with the `OPERATOR_SYNC_IMAGE` env var on the manager.

## Development

### Build

```bash
make build
```

### Run Tests

```bash
make test
```

### Generate CRDs

```bash
make manifests
```

## Architecture

- **OdooInstance Controller**: Manages Odoo deployments, services, PVCs, and ConfigMaps
- **OdooAddon Controller**: Handles Git repository cloning and addon path management

### Volume Strategy

The operator creates a persistent volume for addons:
- **Odoo Pods**: Mount the volume as READ-ONLY
- **Operator**: Has READ-WRITE access to sync Git repositories

## CI/CD

GitHub Actions workflows live in `.github/workflows/`:

| Workflow | Triggers | What it does |
|----------|----------|--------------|
| `lint.yml` | push to `main`, tags `v*`, all PRs | golangci-lint |
| `test.yml` | push to `main`, tags `v*`, all PRs | `make test`, verifies generated files and `go.mod`/`go.sum` are committed and tidy, uploads `cover.out`; `govulncheck ./...` in a separate job |
| `test-e2e.yml` | push to `main`, tags `v*`, PRs to `main` | Creates a pinned kind cluster and runs `make test-e2e`; uploads kind logs on failure |
| `build.yml` | push to `main`, tags `v*`, PRs to `main` | Builds a multi-arch (amd64, arm64) image. On non-PR events it scans the amd64 image with Trivy (fails on HIGH/CRITICAL fixable vulnerabilities), pushes to GHCR with SBOM and provenance, and signs the digest with cosign (keyless) |

Dependabot (`.github/dependabot.yml`) opens weekly updates for Go modules (Kubernetes modules grouped), GitHub Actions and the Docker base images.

Generated files (CRDs, RBAC, deepcopy) must be committed. CI fails if `make verify-generated` produces a diff. Run it locally before pushing:

```bash
make verify-generated
```

### Image tags

Images are published to `ghcr.io/acarmisc/odoo-k8-operator`:

| Event | Tags |
|-------|------|
| push to `main` | `main`, `sha-<short-sha>` |
| tag `vX.Y.Z` | `vX.Y.Z`, `X.Y`, `sha-<short-sha>`, `latest` |
| tag `vX.Y.Z-<pre>` | `vX.Y.Z-<pre>`, `X.Y`, `sha-<short-sha>` (no `latest`) |
| pull request | built, not pushed |

### Cutting a release

1. Make sure `main` is green and generated files are up to date.
2. Tag and push:

   ```bash
   git tag v0.1.0
   git push origin v0.1.0
   ```

3. The `build.yml` workflow publishes and signs the image. Tags are never cancelled by newer runs.

### Verifying the image signature

Images are signed keylessly with cosign using the GitHub Actions OIDC identity. To verify:

```bash
cosign verify ghcr.io/acarmisc/odoo-k8-operator:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/acarmisc/odoo-k8-operator/\.github/workflows/build\.yml@refs/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## License

Apache License 2.0 - See [LICENSE](LICENSE) for details.
