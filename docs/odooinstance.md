# OdooInstance reference

An `OdooInstance` describes one Odoo deployment: the image, the PostgreSQL database, the storage, the `odoo.conf` settings and the resources. The operator turns it into a Deployment, a Service, a ConfigMap, two PVCs and (for more than one replica) a PodDisruptionBudget. This page lists every field, the defaults the code applies, the status fields and the generated objects.

All fields are under `spec` unless noted. Field names are the JSON names used in the CRD.

## Contents

- [Minimal example](#minimal-example)
- [Spec reference](#spec-reference)
- [Validation rules](#validation-rules)
- [spec.config rules](#specconfig-rules)
- [Generated odoo.conf](#generated-odooconf)
- [Status](#status)
- [Generated objects](#generated-objects)
- [Pod details](#pod-details)
- [Production example](#production-example)

## Minimal example

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooInstance
metadata:
  name: demo
  namespace: odoo
spec:
  image: odoo:19.0
  postgres:
    host: postgres.databases.svc.cluster.local
    port: 5432
    database: odoo
    user: odoo
    passwordSecret: odoo-db          # Secret, key "password"
  adminPasswordSecret: odoo-admin    # Secret, key "password"
```

## Spec reference

| Field | Type | Default | Description |
|---|---|---|---|
| `image` | string | `odoo:19.0` | Odoo image for the web pods and for upgrade Jobs. |
| `replicas` | int32 | `1` | Number of Odoo pods. Min 1, max 100. More than 1 requires `ReadWriteMany` on both volumes. |
| `version` | string | none | Stored in the object and shown as a column. **The operator does not use it.** |
| `edition` | string | none | Stored only. **The operator does not use it.** |
| `addonsVolume.size` | quantity | `10Gi` | Size of the addons PVC. Used only when the PVC is created. |
| `addonsVolume.storageClass` | string | cluster default | StorageClass of the addons PVC. Unset means the cluster default. |
| `addonsVolume.accessMode` | `ReadWriteOnce` or `ReadWriteMany` | `ReadWriteOnce` | Access mode of the addons PVC. |
| `dataVolume.size` | quantity | `5Gi` | Size of the filestore PVC (`/var/lib/odoo`). Used only when the PVC is created. |
| `dataVolume.storageClass` | string | cluster default | StorageClass of the filestore PVC. |
| `dataVolume.accessMode` | `ReadWriteOnce` or `ReadWriteMany` | `ReadWriteOnce` | Access mode of the filestore PVC. |
| `dataVolume.retainOnDelete` | bool | `true` | Keep the filestore PVC when the OdooInstance is deleted. See [installation.md](installation.md#uninstalling). |
| `postgres.host` | string | none | PostgreSQL host. Rendered as `db_host` when `postgres.database` is set. |
| `postgres.port` | int32 | `5432` (rendered when unset) | PostgreSQL port, 1 to 65535. |
| `postgres.database` | string | none | Database name. When set, the operator renders the `db_*` options and sets `db_sslmode = require`. |
| `postgres.user` | string | none | Database user, rendered as `db_user`. |
| `postgres.passwordSecret` | string | none | Name of a Secret in the same namespace. Key `password` is injected as `PGPASSWORD` and `DB_PASSWORD`. |
| `config` | map of string to string | none | Extra `odoo.conf` options. See [spec.config rules](#specconfig-rules). |
| `resources` | ResourceRequirements | see below | Resources of the Odoo container and of the upgrade Job. |
| `adminPasswordSecret` | string | none | Name of a Secret with key `password` for Odoo's master password. Injected as `ODOO_ADMIN_PASSWD` and passed as `--admin-passwd`. Never written to the ConfigMap. |
| `upgrade` | object | none | Automatic module upgrades. See [upgrades.md](upgrades.md#automatic-upgrades-specupgrade). |
| `upgrade.modules` | list of string | required | Modules for `odoo -u`. Use `["all"]` for every module. Min 1, max 100 items. Each item matches `^[a-zA-Z0-9_]+$`. |
| `upgrade.timeoutSeconds` | int64 | `1800` | Deadline for each generated OdooUpgrade Job. Min 60, max 86400. |

### Resources default

If both `resources.requests` and `resources.limits` are nil, the operator uses:

| | CPU | Memory |
|---|---|---|
| requests | `250m` | `512Mi` |
| limits | `1` | `1Gi` |

If you set either block, your block is used as is. The two blocks are not merged with the defaults. The same resources apply to the upgrade Job. The addon sync Job has its own fixed resources (see [odooaddon.md](odooaddon.md#resources-and-security-of-the-sync-job)).

## Validation rules

Rules enforced by the CRD (CEL) at admission time:

- `replicas > 1` requires `dataVolume.accessMode: ReadWriteMany`.
- `replicas > 1` requires `addonsVolume.accessMode: ReadWriteMany`.
- `replicas` is between 1 and 100.
- `postgres.port` is between 1 and 65535.
- `upgrade.modules` has 1 to 100 items, each matching `^[a-zA-Z0-9_]+$`.
- `upgrade.timeoutSeconds` is between 60 and 86400.

Rules checked by the controller (not at admission). A violation sets `status.phase: Failed` and the reason is only in the operator log:

- `config` keys and values must pass the [spec.config rules](#specconfig-rules).
- `adminPasswordSecret` and `postgres.passwordSecret` must name existing Secrets. The error is `secret "<name>" referenced by <field> does not exist in namespace "<ns>"`.
- Volume `size` must parse as a Kubernetes quantity.

There is no admission webhook for OdooInstance.

## spec.config rules

`spec.config` is rendered into `odoo.conf` (see below). The operator rejects a spec when:

- a key is one of the blocked keys: `admin_passwd`, `db_password`, `dbfilter`. Use `adminPasswordSecret` for the master password.
- a key is empty or contains `=`, `[`, `]`, a newline or a carriage return.
- a value contains a newline or a carriage return.

Defaults the operator sets, which `spec.config` may override:

| Key | Default | Note |
|---|---|---|
| `addons_path` | `/mnt/odoo/addons` | Overriding it replaces the operator's path. Keep `/mnt/odoo/addons` in your value or your addons will not be found. |
| `proxy_mode` | `True` | Odoo trusts the proxy headers. Expose the Service through a reverse proxy or ingress that sets them. |
| `list_db` | `False` | Disables the database manager listing. |

When `postgres.database` is set, the operator overwrites these keys even if you set them in `spec.config`: `db_host`, `db_port`, `db_name`, `db_user`, `db_sslmode`. Set database connection values in `postgres`, not in `config`.

All values are strings in YAML. Quote numbers and booleans:

```yaml
config:
  workers: "4"
  limit_time_real: "120"
```

## Generated odoo.conf

The operator renders keys in sorted order and stores the file in the ConfigMap `<name>-config` under the key `odoo.conf`. The sample instance above with `workers` and `limit_time_real` set renders:

```ini
[options]
addons_path = /mnt/odoo/addons
db_host = postgres.databases.svc.cluster.local
db_name = odoo
db_port = 5432
db_sslmode = require
db_user = odoo
limit_time_real = 120
list_db = False
proxy_mode = True
workers = 4
```

The hash of this file is a pod-template annotation. Any change to it rolls the pods (see [architecture.md](architecture.md#rollouts)).

## Status

| Field | Type | Meaning |
|---|---|---|
| `status.phase` | string | `Creating`, `Running`, `Failed`, `Upgrading` or `UpgradeFailed`. See below. |
| `status.ready` | bool | `true` when the Deployment has at least `replicas` ready pods. |
| `status.addonPaths` | list of string | One entry per ready OdooAddon bound to this instance: `/mnt/odoo/addons/<moduleName or addon name>`. For a multi-module repo this is one entry for the OdooAddon, not one per module. |
| `status.appliedAddonsHash` | string | Addons revision the running pods were rolled out with. With `spec.upgrade` set, it only advances after the matching upgrade succeeded. |
| `status.activeUpgrade` | string | Name of the OdooUpgrade that currently holds the rollout, if any. |
| `status.observedGeneration` | int64 | Generation the operator last reconciled. |

Phases:

| Phase | Meaning |
|---|---|
| `Creating` | The operator is creating objects, or the Deployment has fewer ready pods than `replicas`. |
| `Running` | The Deployment has at least `replicas` ready pods. |
| `Failed` | The last reconcile returned an error (for example a missing Secret or an invalid `config`). The next successful reconcile clears it. |
| `Upgrading` | A new addons revision is ready, and the pods stay on the previous revision until the OdooUpgrade succeeds. |
| `UpgradeFailed` | The OdooUpgrade failed. The pods stay on the previous revision and `ready` is `false`. |

`status.ready` and `status.phase` are the only summary fields. There is no `status.message`. For the reason behind a `Failed` phase, read the operator log (see [operations.md](operations.md#instance-is-not-ready)).

## Generated objects

Names use the OdooInstance name `<name>`:

| Object | Name | Notes |
|---|---|---|
| Deployment | `<name>` | Owned by the instance. |
| Service | `<name>` | ClusterIP, ports `8069` (odoo) and `8072` (longpolling). |
| ConfigMap | `<name>-config` | Key `odoo.conf`. Owned by the instance. |
| PVC | `<name>-addons` | Owned by the instance, always. Deleted with the instance. |
| PVC | `<name>-data` | Owned only if `dataVolume.retainOnDelete: false`. |
| PodDisruptionBudget | `<name>` | Only while `replicas > 1`. `maxUnavailable: 1`. |
| OdooUpgrade | `<name>-upg-<8 hex chars>` | Created by the operator when `spec.upgrade` is set and the addons revision changes. Owned by the instance. |

Both PVCs are created on the first reconcile, even when the instance has one replica. PVC specs are not changed after creation: a new `size` in the spec does not resize an existing PVC.

Pod labels:

| Label | Value |
|---|---|
| `app` | `odoo` (selector) |
| `instance` | `<name>` (selector) |
| `app.kubernetes.io/name` | `odoo` |
| `app.kubernetes.io/instance` | `<name>` |
| `app.kubernetes.io/managed-by` | `odoo-k8-operator` |

The Deployment selector uses only `app` and `instance`. Do not rely on other labels for selection.

## Pod details

### Volumes

| Mount path | Source | Access | Purpose |
|---|---|---|---|
| `/etc/odoo` | ConfigMap `<name>-config` | read-only | `odoo.conf`. |
| `/mnt/odoo/addons` | PVC `<name>-addons` | read-only | Synced addon modules. Written only by the sync Job. |
| `/var/lib/odoo` | PVC `<name>-data` | read-write | Filestore and sessions. |
| `/tmp` | emptyDir | read-write | Writable temp directory. The root filesystem is read-only. |

### Environment variables

| Variable | Set when | Source |
|---|---|---|
| `PGPASSWORD` | `postgres.passwordSecret` is set | Secret key `password` |
| `DB_PASSWORD` | `postgres.passwordSecret` is set | Secret key `password` |
| `PGSSLMODE` | `postgres.database` is set | Literal `require` |
| `ODOO_ADMIN_PASSWD` | `adminPasswordSecret` is set | Secret key `password` |

The container runs `odoo --config /etc/odoo/odoo.conf`, plus `--admin-passwd=$(ODOO_ADMIN_PASSWD)` when `adminPasswordSecret` is set. The kubelet expands the variable, so the password is not in the pod spec.

### Probes

All probes use HTTP `GET /web/health` on port `8069` with a 1-second timeout.

| Probe | Period | Failure threshold | Effect |
|---|---|---|---|
| startup | 10 s | 30 | Gives Odoo up to about 5 minutes to start. |
| readiness | 10 s | 3 | Removes the pod from the Service when it fails. |
| liveness | 20 s | 3 | Restarts the container when it fails. |

### Rollout strategy

- `replicas: 1` and `dataVolume.accessMode` not `ReadWriteMany`: `Recreate`. The old pod stops before the new one starts, because a `ReadWriteOnce` volume cannot attach to two nodes.
- Otherwise: `RollingUpdate` with `maxSurge: 1` and `maxUnavailable: 0`.

The check looks only at `dataVolume.accessMode`, not at `addonsVolume.accessMode`.

### PodDisruptionBudget

Created when `replicas > 1`, with `maxUnavailable: 1`. It is deleted again when `replicas` drops to 1.

### Security context

Pod level:

- `runAsNonRoot: true`, `runAsUser: 100`, `fsGroup: 100` (the Odoo image user).
- `seccompProfile: RuntimeDefault`.
- `automountServiceAccountToken: false`.
- `terminationGracePeriodSeconds: 60`.

Container level:

- `allowPrivilegeEscalation: false`.
- `readOnlyRootFilesystem: true`.
- `capabilities.drop: [ALL]`.

## Production example

Two replicas on ReadWriteMany storage, with automatic module upgrades. Replace the StorageClass with one that supports ReadWriteMany in your cluster.

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooInstance
metadata:
  name: shop
  namespace: odoo
spec:
  image: odoo:19.0
  replicas: 2
  postgres:
    host: pg-rw.databases.svc.cluster.local
    port: 5432
    database: shop
    user: odoo
    passwordSecret: shop-db
  adminPasswordSecret: shop-admin
  addonsVolume:
    storageClass: nfs-rwx            # placeholder: your ReadWriteMany class
    size: 10Gi
    accessMode: ReadWriteMany
  dataVolume:
    storageClass: nfs-rwx            # placeholder: your ReadWriteMany class
    size: 50Gi
    accessMode: ReadWriteMany
    retainOnDelete: true
  config:
    workers: "4"
    limit_time_real: "120"
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: "2"
      memory: 2Gi
  upgrade:
    modules: ["all"]
    timeoutSeconds: 3600
```

Read [production-guide.md](production-guide.md) before using this in production, in particular the storage and backup sections.

## See also

- [odooaddon.md](odooaddon.md): adding code to the instance.
- [upgrades.md](upgrades.md): what `spec.upgrade` does and what it does not gate.
- [architecture.md](architecture.md): how the controllers create these objects.
