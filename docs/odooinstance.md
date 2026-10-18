# OdooInstance reference

An `OdooInstance` describes one Odoo deployment: the image, the PostgreSQL database, the storage, the `odoo.conf` settings and the resources. The operator turns it into a Deployment, a Service, a ConfigMap, two PVCs, (for more than one replica) a PodDisruptionBudget and, with `spec.cron`, a separate cron Deployment. This page lists every field, the defaults the code applies, the status fields and the generated objects.

All fields are under `spec` unless noted. Field names are the JSON names used in the CRD.

## Contents

- [Minimal example](#minimal-example)
- [Spec reference](#spec-reference)
- [Validation rules](#validation-rules)
- [spec.config rules](#specconfig-rules)
- [Generated odoo.conf](#generated-odooconf)
- [Status](#status)
- [Dedicated cron pod](#dedicated-cron-pod)
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
| `initialize` | object | none | One-time database initialisation. See [Database initialisation](#database-initialisation). Can only be set when the instance is created. |
| `initialize.modules` | list of string | required | Modules for `odoo -i`. `["base"]` is the minimal value and creates an empty Odoo database. Min 1, max 100 items, each `^[a-zA-Z0-9_]+$`. |
| `initialize.adminUserPasswordSecret` | string | required | Secret in the same namespace, key `password`. Its value becomes the password of the Odoo user `admin`. |
| `initialize.withDemo` | bool | `false` | Load demo data. `false` passes `--without-demo=all`. |
| `initialize.timeoutSeconds` | int64 | `1800` | Deadline for the initialisation Job. Min 60, max 86400. |
| `cron` | object | none | Runs the scheduled actions in one dedicated pod. See [Dedicated cron pod](#dedicated-cron-pod). |
| `cron.enabled` | bool | `false` | Creates the Deployment `<name>-cron` and renders `max_cron_threads = 0` into the web pods' `odoo.conf`. |
| `cron.maxCronThreads` | int32 | `2` | Cron workers of the cron pod (`--max-cron-threads`). Min 1, max 16. |
| `cron.resources` | ResourceRequirements | `resources` | Resources of the cron pod. Unset means the web `resources`, or the defaults. |

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
- `initialize.modules` has 1 to 100 items, each matching `^[a-zA-Z0-9_]+$`.
- `initialize.adminUserPasswordSecret` is required and not empty when `initialize` is set.
- `spec.initialize` cannot be added to an existing OdooInstance (a transition rule). Adding it would re-initialise a live database. Removing it is allowed.

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

With `spec.cron.enabled: true` the operator also writes `max_cron_threads = 0`, which the web pods need to serve HTTP without cron workers. A `max_cron_threads` key in `spec.config` is rejected while cron is enabled (`InvalidConfig`).

The hash of this file is a pod-template annotation. Any change to it rolls the pods (see [architecture.md](architecture.md#rollouts)).

## Status

| Field | Type | Meaning |
|---|---|---|
| `status.phase` | string | `Creating`, `Running`, `Failed`, `Upgrading` or `UpgradeFailed`. See below. |
| `status.ready` | bool | `true` when the Deployment has at least `replicas` ready pods. |
| `status.addonPaths` | list of string | One entry per ready OdooAddon bound to this instance: `/mnt/odoo/addons/<moduleName or addon name>`. For a multi-module repo this is one entry for the OdooAddon, not one per module. |
| `status.appliedAddonsHash` | string | Addons revision the running pods were rolled out with. With `spec.upgrade` set, it only advances after the matching upgrade succeeded. |
| `status.activeUpgrade` | string | Name of the OdooUpgrade that currently holds the rollout, if any. |
| `status.initialized` | bool | `true` once `spec.initialize` succeeded, or was skipped because the Deployment already had ready pods. |
| `status.initUpgrade` | string | Name of the initialisation OdooUpgrade (`<name>-init`) while it runs or after it failed. |
| `status.observedGeneration` | int64 | Generation the operator last reconciled. |
| `status.conditions` | list | `Ready`, `Progressing` and `Degraded`, plus `CronReady` while `spec.cron.enabled` is set. See below. |
| `status.message` | string | Summary of the state: the `Degraded` message while degraded, otherwise the `Ready` message while not ready. |

Phases:

| Phase | Meaning |
|---|---|
| `Creating` | The operator is creating objects, or the Deployment has fewer ready pods than `replicas`. |
| `Running` | The Deployment has at least `replicas` ready pods. |
| `Failed` | The last reconcile returned an error (for example a missing Secret or an invalid `config`). The next successful reconcile clears it. |
| `Upgrading` | A new addons revision is ready, and the pods stay on the previous revision until the OdooUpgrade succeeds. |
| `UpgradeFailed` | The OdooUpgrade failed. The pods stay on the previous revision and `ready` is `false`. |
| `Initializing` | `spec.initialize` is running. The Deployment has 0 replicas. |
| `InitializationFailed` | The initialisation OdooUpgrade failed. The Deployment stays at 0 replicas. See [operations.md](operations.md#initialisation-failed). |

Conditions use the usual Kubernetes shape: `type`, `status`, `reason`, `message`, `lastTransitionTime` and `observedGeneration`. Read the `reason` for scripts and the `message` for people.

| Type | Status | Reasons |
|---|---|---|
| `Ready` | `True` | `AllReplicasReady`: the Deployment has `replicas` ready pods. |
| `Ready` | `False` | `DeploymentNotReady`, `UpgradeFailed`, `Initializing`, `InitializationFailed`, `MissingSecret`, `InvalidConfig`, `ReconcileError`. |
| `Progressing` | `True` | `Reconciling`: the Deployment is rolling out. `Upgrading`: an OdooUpgrade holds the rollout. `Initializing`: the database is being initialised. |
| `Progressing` | `False` | `Reconciled`, `UpgradeFailed`, `InitializationFailed`, `ReconcileError`. |
| `Degraded` | `False` | `AsExpected`. |
| `Degraded` | `True` | `MissingSecret`, `InvalidConfig`, `ReconcileError`, `UpgradeFailed`, `InitializationFailed`. |
| `CronReady` | `True` | `CronRunning`: the cron pod is ready. Present only while `spec.cron.enabled` is set. |
| `CronReady` | `False` | `CronPending`: no cron pod yet (not created, held during initialisation, or waiting for a web pod on the same node). `CronNotReady`: the cron pod exists but is not ready. |

`MissingSecret` names a Secret that does not exist. `InvalidConfig` is a bad `spec.config` key or value, or a bad size. The `ReconcileFailed` Event carries the same message. For the commands, see [operations.md](operations.md#read-the-state).

## Database initialisation

A new instance has an empty database, which Odoo cannot serve until its base modules are installed. The database manager is off (`list_db = False`), so there is no UI to do this. A new instance therefore needs a one-time initialisation. Set `spec.initialize` and the operator does it:

```yaml
spec:
  postgres:
    host: postgres.databases.svc.cluster.local
    port: 5432
    database: shop
    user: odoo
    passwordSecret: shop-db
  initialize:
    modules: ["base"]
    adminUserPasswordSecret: shop-init-admin   # Secret, key "password"
```

What the operator does:

1. It creates the OdooUpgrade `<name>-init`, owned by the instance. The OdooUpgrade runs `odoo -d <database> -i <modules> --stop-after-init`, with `--without-demo=all` unless `withDemo` is `true`. Then it sets the password of the Odoo user `admin` through `odoo shell`. The Job fails if either step fails.
2. Until the OdooUpgrade succeeds, the Deployment has **0 replicas** and the phase is `Initializing`. No Odoo pod runs against an empty database. The other objects (ConfigMap, Service, PVCs, PodDisruptionBudget) are reconciled as usual, because the Job needs them.
3. On success, `status.initialized` becomes `true`, `status.initUpgrade` is cleared, the Deployment is scaled to `replicas` and the `Initialized` Event is emitted.
4. On failure, the phase is `InitializationFailed`, `Degraded` is `True` with the failure message, the Deployment stays at 0 replicas and the reconcile is repeated every 60 seconds. Retry as described in [operations.md](operations.md#initialisation-failed).

Rules and limits:

- The default `admin` password (`admin`) is never left in place by this path. The password comes only from the Secret, through the environment of the Job. It never appears in the arguments or in the log.
- Modules other than `base` must already be Synced as OdooAddons. `-i` cannot find a module that is not in `addons_path`. For a new instance, initialise with `base`, then install more modules with a manual OdooUpgrade (see [upgrades.md](upgrades.md#manual-upgrade)).
- The database must exist, or the database user needs `CREATEDB`. See [production-guide.md](production-guide.md#first-installation).
- An instance whose Deployment already has ready pods and no `<name>-init` OdooUpgrade is treated as initialised. Nothing is run, and a `Warning` Event `InitializationSkipped` says so. This guards a live database against a second `-i`.
- The initialisation has not been run against a real Odoo in CI. The tests use envtest (a real API server, no Odoo). Check the first Job log on your own cluster.

## Dedicated cron pod

Odoo runs its scheduled actions (cron) in the worker threads of every pod with `max_cron_threads` above 0. With several web pods, each one would run the same jobs. With `spec.cron.enabled: true` the operator moves them to one pod:

- The web pods get `max_cron_threads = 0` in `odoo.conf`, so they serve HTTP only.
- The Deployment `<name>-cron` runs one pod with `--no-http --max-cron-threads=<n>`. It has no ports, no Service and no HTTP probes. Its strategy is `Recreate`, so two cron pods never run at once.
- The cron pod has the same image, environment, volumes, security context and rollout hashes as the web pods. It rolls with them, follows the addons revision the upgrade gate allows, and stays at zero replicas during `spec.initialize`.
- Its pod labels are `app: odoo-cron`, `instance: <name>` and `app.kubernetes.io/component: cron`. The web Service and PodDisruptionBudget select `app: odoo`, so they never select a cron pod. The Deployment selector cannot be changed after creation.
- **Shared node for `ReadWriteOnce` volumes.** When the data or addons volume is not `ReadWriteMany`, the web pod and the cron pod must run on the same node (`kubernetes.io/hostname`). Both pod templates carry the same required pod affinity on the label `instance: <name>`, so each one can only be scheduled onto a node that already runs a pod of the instance. The first pod of the instance is placed freely. Every later web or cron pod joins its node. A web rollout therefore stays on the node of the cron pod, and the cron pod stays on the node of a web pod, so the RWO volumes never attach to two nodes. The web Deployment selector is unchanged. Only the pod template changes, so the web pods roll once when you upgrade to a version with this rule. With `ReadWriteMany` volumes neither template has an affinity.
- Disable it with `enabled: false` or by removing `spec.cron`. The operator deletes `<name>-cron` and removes the override, so the web pods roll and run cron again.

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooInstance
metadata:
  name: shop
spec:
  replicas: 3
  addonsVolume:
    accessMode: ReadWriteMany
  dataVolume:
    accessMode: ReadWriteMany
  cron:
    enabled: true
    maxCronThreads: 4
```

`CronReady` (see [Status](#status)) reports whether the cron pod runs. It does not change `Ready`, which follows the web pods only.

## Generated objects

Names use the OdooInstance name `<name>`:

| Object | Name | Notes |
|---|---|---|
| Deployment | `<name>` | Owned by the instance. |
| Deployment | `<name>-cron` | Only while `spec.cron.enabled`. Owned by the instance. One replica, no Service. See [Dedicated cron pod](#dedicated-cron-pod). |
| Service | `<name>` | ClusterIP, ports `8069` (odoo) and `8072` (longpolling). |
| ConfigMap | `<name>-config` | Key `odoo.conf`. Owned by the instance. |
| PVC | `<name>-addons` | Owned by the instance, always. Deleted with the instance. |
| PVC | `<name>-data` | Owned only if `dataVolume.retainOnDelete: false`. |
| PodDisruptionBudget | `<name>` | Only while `replicas > 1`. `maxUnavailable: 1`. |
| OdooUpgrade | `<name>-upg-<8 hex chars>` | Created by the operator when `spec.upgrade` is set and the addons revision changes. Owned by the instance. |
| OdooUpgrade | `<name>-init` | Created by the operator when `spec.initialize` is set, until the database is initialised. Owned by the instance. |

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

The cron pods of `spec.cron` carry `app: odoo-cron` and `app.kubernetes.io/component: cron` instead of `app: odoo`.

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
