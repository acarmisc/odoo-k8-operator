# Production guide

This guide lists what to decide and check before you run Odoo on this operator for real users. It covers storage, secrets, scaling, resources, security, monitoring and backups, then the multi-instance layout, an upgrade runbook and the known limitations. The operator covers the Kubernetes objects. It runs backups with `OdooBackup`, but it does not cover your database server, the retention of your backup storage, or your network policy. Those are your job.

## Contents

- [Checklist](#checklist)
- [Storage](#storage)
- [Secrets](#secrets)
- [Database](#database)
- [Scaling and availability](#scaling-and-availability)
- [Resources](#resources)
- [Security](#security)
- [Monitoring](#monitoring)
- [Backups](#backups)
- [Multi-instance layout](#multi-instance-layout)
- [Odoo version upgrades](#odoo-version-upgrades)
- [Known limitations](#known-limitations)

## Checklist

- [ ] Storage: `ReadWriteMany` on both volumes if you run more than one replica. ([Storage](#storage))
- [ ] Secrets: database and master passwords in Secrets, with key `password`. ([Secrets](#secrets))
- [ ] Database: PostgreSQL accepts TLS, and the database is created and initialised. ([Database](#database), [First installation](#first-installation))
- [ ] Git: pin `gitRef` to a commit SHA. Use a `gitSecretRef` for private repositories. ([odooaddon.md](odooaddon.md#branches-tags-and-commit-shas))
- [ ] Upgrades: decide whether `spec.upgrade` gates rollouts, and test it. ([upgrades.md](upgrades.md))
- [ ] Network: add a NetworkPolicy per instance. The operator does not create one. ([Security](#security))
- [ ] Monitoring: scrape the operator, alert on not-ready instances and failed addons. ([Monitoring](#monitoring))
- [ ] Backups: create an `OdooBackup` for the database and the filestore, and test a restore. ([Backups](#backups))

## Storage

Each instance has two PVCs:

| PVC | Holds | Default size | Default access mode | Owned by the instance |
|---|---|---|---|---|
| `<name>-addons` | Synced modules (read-only in the Odoo pods) | `10Gi` | `ReadWriteOnce` | Always. Deleted with the instance. |
| `<name>-data` | Filestore and sessions (`/var/lib/odoo`) | `5Gi` | `ReadWriteOnce` | Only with `retainOnDelete: false`. Default is `true`, so it is kept. |

**ReadWriteOnce (RWO)** is enough for one replica. The Deployment then uses the `Recreate` strategy, so the old pod stops before the new one starts. Two things follow from RWO:

- The sync Job and the upgrade Job are pinned to the node of the running Odoo pod. If no Odoo pod is running yet, they are placed freely, and a Job can fail to attach the volume. See [known limitations](#known-limitations).
- An RWO volume cannot be shared across nodes, so you cannot scale out.

**ReadWriteMany (RWX)** is required for more than one replica. The CRD rejects `replicas > 1` unless both `addonsVolume.accessMode` and `dataVolume.accessMode` are `ReadWriteMany`. Use a class that really supports RWX (for example NFS, CephFS, EFS or Azure Files). Check it before you go live:

```bash
kubectl get storageclass
```

Other storage points:

- **Size changes after creation are not applied by the operator.** The PVC is created once. If your StorageClass has `allowVolumeExpansion: true`, resize the PVC yourself, for example `kubectl -n odoo patch pvc demo-data -p '{"spec":{"resources":{"requests":{"storage":"20Gi"}}}}'`.
- **Retention.** The filestore survives deleting the OdooInstance, by default. Delete it by hand when you are sure. See [installation.md](installation.md#what-happens-to-the-data).
- **Reclaim policy.** Check that your StorageClass uses `reclaimPolicy: Retain` for the filestore if you want the volume to survive a PVC deletion. The default `Delete` removes the volume.

## Secrets

| Use | Field | Secret key |
|---|---|---|
| PostgreSQL password | `spec.postgres.passwordSecret` | `password` |
| Odoo master password | `spec.adminPasswordSecret` | `password` |
| Git token or SSH key | `OdooAddon.spec.gitSecretRef` | `token`, or `ssh-privatekey` and `known_hosts` |

- The key is fixed. A Secret for the database must have a key `password`.
- The master password is injected as an environment variable and passed to `--admin-passwd`. It never goes into the ConfigMap. `admin_passwd` is a blocked key in `spec.config`.
- **Rotation of the database or master password** rolls the pods automatically. The operator watches the referenced Secrets and includes their data in the pod template hash.
- **Rotation of a Git token** takes effect at the next sync run. The operator does not watch the Git Secret. See [odooaddon.md](odooaddon.md#rotating-credentials).
- Do not put passwords in `spec.config`, `gitUrl` or any ConfigMap.

## Database

- The operator connects with `db_host`, `db_port`, `db_name`, `db_user` and the password from the Secret.
- It always sets `db_sslmode = require`. Your database must accept TLS. There is no field for a custom CA, so a self-signed database certificate is not supported by the generated config.
- The operator does **not** create the database by itself. The database must exist, or the database user must be able to create it. To initialise a new database, set `spec.initialize`. See [First installation](#first-installation).
- Put a connection pooler in front of the database only after you have tested it with Odoo.
- Set `postgres.port`. When `postgres.database` is set and the port is unset, the generated `db_port` is `0`.

## First installation

A runbook for a new instance with an empty database. Change nothing else in the spec.

1. Create the Secrets. Each one has the key `password`:

   ```bash
   kubectl -n odoo create secret generic shop-db --from-literal=password='<database password>'
   kubectl -n odoo create secret generic shop-admin --from-literal=password='<master password>'
   kubectl -n odoo create secret generic shop-init-admin --from-literal=password='<password of user admin>'
   ```

2. Make sure the database can be created by the operator's run. Either:
   - the database user has `CREATEDB`, and Odoo creates the database during the install; or
   - create an empty database owned by the user, for example `CREATE DATABASE shop OWNER odoo;`. Odoo then only installs into it.

   Odoo's default `admin` password is never used, because `spec.initialize` sets it.

3. Apply the OdooInstance with `postgres` set and `initialize` added:

   ```yaml
   apiVersion: odoo.operator.io/v1
   kind: OdooInstance
   metadata:
     name: shop
     namespace: odoo
   spec:
     image: odoo:19.0
     adminPasswordSecret: shop-admin
     postgres:
       host: pg-rw.databases.svc.cluster.local
       port: 5432
       database: shop
       user: odoo
       passwordSecret: shop-db
     initialize:
       modules: ["base"]
       adminUserPasswordSecret: shop-init-admin
   ```

4. Watch the initialisation. The phase is `Initializing` while the Deployment has 0 replicas:

   ```bash
   kubectl -n odoo get odooinstance shop -w
   kubectl -n odoo get odooupgrade shop-init
   kubectl -n odoo logs job/shop-init-run
   ```

5. When the instance is `Running`, the first install is done. Install other modules with a manual OdooUpgrade, after their OdooAddons are Synced.

If the phase becomes `InitializationFailed`, see [operations.md](operations.md#initialisation-failed).

The install path has not been run against a live Odoo in CI. The tests use envtest only.

## Scaling and availability

- `replicas` accepts 1 to 100. There is no autoscaler. Change `replicas` yourself, or with an external tool that patches the spec.
- With `replicas > 1` the operator creates a PodDisruptionBudget with `maxUnavailable: 1`. A node drain then evicts at most one Odoo pod at a time.
- Rolling updates use `maxSurge: 1` and `maxUnavailable: 0`, so the old pods keep serving until the new ones are ready.
- The operator has no fields for node selectors, affinity, tolerations or topology spread. Use cluster-level policies if you need them.
- **Cron jobs.** Without `spec.cron`, every pod runs Odoo's scheduled actions in its cron workers, so each web replica multiplies them. Set `spec.cron.enabled: true` before you scale out. The web pods then run no cron workers, and one `<name>-cron` pod runs the scheduled actions. See [odooinstance.md](odooinstance.md#dedicated-cron-pod).
- **Recommended layout for more than one web pod:** `replicas` with `ReadWriteMany` volumes, `cron.enabled: true`, and `cron.maxCronThreads` sized for your scheduled actions. Point the Ingress at the web Service. The cron pod has no Service.

## Resources

Defaults when `resources` is not set: requests `250m` CPU and `512Mi` memory, limits `1` CPU and `1Gi` memory. Odoo with several workers needs more memory than this. Set `resources` explicitly, based on the number of workers and your load.

```yaml
resources:
  requests:
    cpu: 500m
    memory: 1Gi
  limits:
    cpu: "2"
    memory: 2Gi
```

The sync Job and the operator have fixed resources. Those are listed in [odooaddon.md](odooaddon.md#resources-and-security-of-the-sync-job) and [installation.md](installation.md#manager-flags-and-environment).

## Security

### What the operator sets

Odoo pods:

- `runAsNonRoot: true`, UID and GID `100` (the Odoo image user), `fsGroup: 100`;
- `seccompProfile: RuntimeDefault`;
- `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, all capabilities dropped;
- `automountServiceAccountToken: false`;
- no privileged mode, no host networking, no host paths.

Sync and upgrade Jobs use the same pattern. The sync Job runs as UID `65532`.

These fields match what the Kubernetes `restricted` Pod Security Standard requires. Check this on your cluster. A dry run reports the pods that would violate a policy:

```bash
kubectl label --dry-run=server --overwrite ns odoo pod-security.kubernetes.io/enforce=restricted
```

The operator's own namespace is labelled `restricted` in the end-to-end test.

### What you should add

- **NetworkPolicy per instance.** The operator creates none. A typical policy allows ingress to ports `8069` and `8072` only from your ingress controller's namespace, and egress only to the database and DNS.
- **Image by digest.** `spec.image` accepts any image reference, including `odoo@sha256:...`. Use a digest for production.
- **Pull secrets.** The pods have no `imagePullSecrets` field. They use the namespace's `default` ServiceAccount, so add pull secrets there:

  ```bash
  kubectl -n odoo patch serviceaccount default -p '{"imagePullSecrets":[{"name":"regcred"}]}'
  ```

- **Sync image.** The default `alpine/git:2.47.2` is a tag, not a digest. Set `OPERATOR_SYNC_IMAGE` to a digest in your manager configuration.
- **Operator image.** Verify the cosign signature before you deploy. See [development.md](development.md#image-tags-and-signature-verification).
- **RBAC.** Read [installation.md](installation.md#namespaces-and-rbac) before you grant cluster-wide access.

## Monitoring

**Operator metrics.** The manager serves metrics on `:8443` over HTTPS with authentication and authorization. Give your Prometheus a ServiceAccount that is bound to the `metrics-reader` ClusterRole. `config/prometheus/monitor.yaml` is a `ServiceMonitor` for that endpoint. It needs the Prometheus Operator CRDs, and it is not part of the default deploy. It sets `insecureSkipVerify: true`, which the file's own TODO flags for production.

**Odoo pods.** The operator exposes no Odoo metrics. Use what the cluster already gives you: Deployment readiness, container restarts, and the OdooInstance `status.ready` and `status.phase` fields.

**Alerts to consider.** Write them with your own Prometheus rules or with a tool that reads the Kubernetes objects:

- an OdooInstance `status.ready` is `false` for longer than your deploy time;
- an OdooInstance `status.phase` is `UpgradeFailed`;
- an OdooAddon `status.phase` is `Failed` for more than a few minutes;
- an OdooUpgrade `status.phase` is `Failed`;
- the manager pod restarts.

## Backups

The operator backs up the database and the filestore with the `OdooBackup` resource. It runs `pg_dump` and archives `/var/lib/odoo/filestore/<database>`, and writes the files to a PersistentVolumeClaim or to an S3-compatible bucket. One-shot and scheduled (cron) backups are both supported, with retention. The full reference, including the restore runbook and the limits, is in [backups.md](backups.md).

A minimal scheduled backup to S3-compatible storage:

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooBackup
metadata:
  name: shop-nightly
  namespace: odoo
spec:
  instanceRef:
    name: shop
  schedule: "0 2 * * *"
  destination:
    s3:
      bucket: odoo-backups
      prefix: shop
      endpoint: https://minio.example.internal:9000
      forcePathStyle: true
      credentialsSecret: backup-s3
  retention: 14
```

The addons are not backed up. They come from Git, so they can be restored from the commit SHA.

Points to decide before go-live:

- **Consistency.** The dump and the filestore archive are taken one after the other. Writes in between can make them disagree. Schedule backups in a quiet window, or stop the instance for a consistent pair. See [backups.md](backups.md#requirements-and-limits).
- **Volume snapshots** are a complement, not a replacement. A VolumeSnapshot of `<name>-data` is fast and covers the whole filestore. Take it with a snapshot class that supports your StorageClass, and combine it with a database dump taken in the same window:

  ```yaml
  apiVersion: snapshot.storage.k8s.io/v1
  kind: VolumeSnapshot
  metadata:
    name: shop-data-2026-10-17
    namespace: odoo
  spec:
    volumeSnapshotClassName: <your-snapshot-class>
    source:
      persistentVolumeClaimName: shop-data
  ```

- **Credentials and access.** Creating an OdooBackup lets its creator read the instance's database through the operator. Give that permission only where database access is intended.
- **Test restores.** An untested backup is not a backup. Restore to a second namespace on a schedule. The restore steps are in [backups.md](backups.md#restore-runbook).

## Multi-instance layout

Two layouts work with the operator:

| Layout | Use when | Notes |
|---|---|---|
| One namespace per instance | Tenants must be isolated, or each instance has its own quota and network policy | One RoleBinding per namespace. Clear ownership and clean deletion. |
| Several instances in one namespace | Small number of instances, shared team | One RoleBinding. Names must be unique in the namespace, because the Deployment, Service and PVCs are named after the instance. |

Rules that apply to both:

- An OdooAddon must be in the same namespace as the OdooInstance it references. Cross-namespace references are refused.
- The instance name is used for the Deployment, the Service, the PDB and the PVCs. Choose names that are valid DNS labels with room for the suffixes (`-addons`, `-config`, `-data`).
- Use a ResourceQuota per namespace to keep one tenant from consuming the cluster.

## Odoo version upgrades

This runbook is **guidance, not tested in this repository**. It uses the features described in [upgrades.md](upgrades.md). Check the Odoo migration notes for your version jump before you start. Odoo migrations usually go one major version at a time.

Why a copy: the operator changes the image immediately. An image change is not gated by `spec.upgrade`, so changing `spec.image` on the live instance would start the new code against the old database. Do the upgrade on a copy first.

1. **Back up** the database and the filestore. See [Backups](#backups).
2. **Copy the database** into a new database, for example `shop_next`. Stop writes first or use a consistent copy. Check your database tooling for the exact commands.
3. **Create a second instance** with the new image and the copy:

   ```yaml
   apiVersion: odoo.operator.io/v1
   kind: OdooInstance
   metadata:
     name: shop-next
     namespace: odoo
   spec:
     image: odoo:<new-version>
     postgres:
       host: pg-rw.databases.svc.cluster.local
       port: 5432
       database: shop_next
       user: odoo
       passwordSecret: shop-db
     adminPasswordSecret: shop-admin
     addonsVolume:
       size: 10Gi
   ```

4. **Point an addon at the new instance.** Each OdooAddon has one `instanceRef`, so create a separate OdooAddon for `shop-next`. Its `gitRef` must point at code that works with the new version.
5. **Upgrade the copy.** Create an OdooUpgrade for `shop-next` with `modules: ["all"]`, or the modules you need. Wait for `Succeeded`, then test.
6. **Cut over.** Point your ingress at `shop-next`. The old instance and its database are untouched, so rolling back is pointing the ingress at `shop` again.
7. **Clean up** the old instance only after you are satisfied.

## Known limitations

These come from reading the code and from [audit-2026-10.md](audit-2026-10.md), the review of commit `45583eb`. Some items in that review are now fixed. Check the current code before you plan around one.

**Database and upgrades**

- The operator does not create or initialise the database. Only an OdooUpgrade runs `odoo -u` or `-i`.
- Image changes and config changes roll the pods at once. Only addon revisions can be gated by `spec.upgrade`.
- Addon files on the shared volume switch before an upgrade finishes.
- An OdooUpgrade always uses the instance's image.

**Storage and scheduling**

- Sync and upgrade pods with RWO volumes are pinned to a running Odoo pod's node. Before any Odoo pod runs, the scheduler places them freely.
- PVC size changes are not applied.
- No fields for node selectors, affinity, tolerations or topology spread.

**Networking and workloads**

- No Ingress, Gateway API or Route is created. Expose the Service yourself.
- No HorizontalPodAutoscaler.
- Odoo's `workers` and similar settings are not set by the operator. Set them in `spec.config`.
- `db_sslmode` is always `require`. There is no custom CA field.

**Status and visibility**

- Fields that the API accepts but the operator does not use: `spec.version`, `spec.edition` (OdooInstance) and `spec.readOnly` (OdooAddon).

**Git**

- A branch or tag is synced every 5 minutes, and each sync runs a Job.
- A rotated Git credential is picked up at the next sync, not when the Secret changes.

**Validation and admission**

- The OdooAddon webhook is off by default and not included in the default manifests.
- The OdooInstance has no webhook. Its only admission checks are the CEL rules on `replicas` and the volume access modes.

**Operator**

- The manager watches all namespaces and caches every Secret it can read. Memory use grows with the number of Secrets. See [installation.md](installation.md#namespaces-and-rbac) for the RBAC issue.
- Metrics are served only if you deploy the manifests that enable them.

## See also

- [installation.md](installation.md): install, RBAC and flags.
- [operations.md](operations.md): what to do when something is wrong.
- [audit-2026-10.md](audit-2026-10.md): the full review that this guide draws on.
