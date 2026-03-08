# Odoo Kubernetes Operator - Development Plan

## Project Overview

**Project Name:** odoo-k8-operator  
**Type:** Kubernetes Operator (Custom Controller)  
**Language:** Python (using Operator SDK)  
**Goal:** Simplify deployment of multiple Odoo instances and their custom addons

---

## Architecture

### Custom Resource Definitions (CRDs)

#### 1. OdooInstance
Represents a single Odoo deployment with its configuration.

**Spec Fields:**
- `image` (string): Odoo Docker image (default: `odoo:17.0`)
- `replicas` (int): Number of Odoo pods (default: 1)
- `edition` (string): Odoo edition - "community" or "enterprise"
- `version` (string): Odoo version (e.g., "17.0")
- `addonsVolume` (AddonVolumeSpec): Configuration for addons volume
  - `storageClass` (string): Storage class for PVC
  - `size` (string): Volume size (default: "10Gi")
- `postgres` (PostgresSpec): PostgreSQL configuration
  - `host` (string): External PostgreSQL host
  - `port` (int): PostgreSQL port (default: 5432)
  - `database` (string): Database name
  - `user` (string): Database user
  - `passwordSecret` (string): Secret name containing DB password
- `config` (map[string]string): Odoo configuration parameters
- `resources` (ResourceRequirements): CPU/memory requests/limits

**Status Fields:**
- `ready` (bool): Instance ready status
- `phase` (string): Current phase (Creating, Running, Failed)
- `addonPaths` ([]string): Paths to mounted addons
- `observedGeneration` (int64): Last observed generation

#### 2. OdooAddon
Represents a custom addon from a Git repository.

**Spec Fields:**
- `gitUrl` (string): Git repository URL (required)
- `gitRef` (string): Git branch/tag/commit (default: "main")
- `addonPath` (string): Path to addon within repository (for monorepo)
- `singleAddon` (bool): If true, repository contains single addon
- `instanceRef` (ObjectReference): Reference to OdooInstance
- `readOnly` (bool): Mount as readonly for Odoo pod (default: true)

**Status Fields:**
- `ready` (bool): Addon synced status
- `phase` (string): Current phase (Pending, Cloning, Synced, Failed)
- `clonedCommit` (string): Current git commit hash
- `lastSyncTime` (Time): Last successful sync timestamp

---

## Implementation Details

### Volume Management Strategy

1. **Addons Volume (PVC)**
   - Created by OdooInstance controller
   - Mounted in Odoo pod as ReadWriteOnce
   - **Odoo Pod**: Mounted as READ-ONLY
   - **Operator**: Mounts with READ-WRITE for git operations

2. **Implementation**
   - Init container clones addons to volume (RW by operator)
   - Main Odoo container mounts volume as READ-ONLY
   - OdooAddon controller updates volume content via git operations

### Controller Logic

#### OdooInstance Controller
1. Create/manage addons PVC
2. Create Odoo Deployment
3. Create Odoo Service
4. Handle configmap for odoo.conf
5. Manage secret references
6. Update status with addon paths

#### OdooAddon Controller
1. Watch OdooAddon resources
2. Validate instance reference exists
3. Clone/update git repository to addons volume
4. Update addon path in OdooInstance status
5. Trigger Odoo pod restart on addon change

---

## Directory Structure

```
odoo-k8-operator/
├── PROJECT                      # operator-sdk project config
├── Makefile                     # Build automation
├── config/
│   ├── default/
│   │   └── kustomization.yaml
│   ├── manager/
│   │   └── manager.yaml
│   ├── rbac/
│   │   ├── role.yaml
│   │   └── role_binding.yaml
│   └── crd/
│       ├── bases/
│       │   ├── odooinstances.odoo.io_odooinstances.yaml
│       │   └── odooinstances.odoo_odooaddons.yaml
│       └── patches/
├── controllers/
│   ├── odooinstance_controller.py
│   ├── odooaddon_controller.py
│   └── suite_test.py
├── api/
│   ├── v1/
│   │   ├── odooinstance_types.py
│   │   ├── odooaddon_types.py
│   │   └── groupversion_info.py
│   └── v1alpha1/                # Future: older API versions
├── operator/
│   └── operator.py             # Main entry point
├── requirements.txt
├── docker-operator/
│   ├── Dockerfile
│   └── Makefile
└── tests/
    └── e2e/
```

---

## Acceptance Criteria

1. ✅ OdooInstance CRD can be created with custom configuration
2. ✅ OdooAddon CRD can reference Git repository and attach to instance
3. ✅ Addons volume mounted readonly in Odoo pod
4. ✅ Operator can write to addons volume for git operations
5. ✅ Odoo pods restart when addons are updated
6. ✅ Status fields reflect current state
7. ✅ Operator handles reconcile loops correctly
8. ✅ Basic unit tests pass

---

## Future Enhancements (Out of Scope)

- OdooCluster CRD for multi-pod Odoo deployments
- Automatic PostgreSQL provisioning
- Odoo backup/restore functionality
- Metrics and monitoring
- Webhook validation
