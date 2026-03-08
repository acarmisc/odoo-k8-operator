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

## License

Apache License 2.0 - See [LICENSE](LICENSE) for details.
