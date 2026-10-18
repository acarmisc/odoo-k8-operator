# Backups

An `OdooBackup` dumps the PostgreSQL database of an OdooInstance and, optionally, archives its filestore. It writes the files to a PersistentVolumeClaim or to an S3-compatible bucket. A backup is either one-shot (one Job) or scheduled (a CronJob). This page covers the fields, the destinations, retention, the restore runbook, and the limits. The restore steps are guidance. The operator does not restore anything.

## Contents

- [What is backed up](#what-is-backed-up)
- [Quick start](#quick-start)
- [Destinations](#destinations)
- [Scheduled backups](#scheduled-backups)
- [Retention](#retention)
- [Status, phases and conditions](#status-phases-and-conditions)
- [Restore runbook](#restore-runbook)
- [Requirements and limits](#requirements-and-limits)
- [Security notes](#security-notes)
- [Configuration](#configuration)
- [Field reference](#field-reference)

## What is backed up

| Item | How | Notes |
|---|---|---|
| PostgreSQL database | `pg_dump --format=custom --no-owner`, one file `<database>-<UTC timestamp>.dump` | Always. The database is `spec.database`, or the instance's `postgres.database`. |
| Filestore | `tar -czf` of `/var/lib/odoo/filestore/<database>`, one file `<database>-<timestamp>-filestore.tar.gz` | On by default (`includeFilestore: true`). If the directory does not exist yet, the backup logs a warning and writes the dump only. |

Not backed up: the addons (they come from Git, see [odooaddon.md](odooaddon.md)), the generated `odoo.conf`, the Secrets, and other databases on the server. The dump and the filestore archive are taken one after the other, not in one snapshot. Writes between the two can make them disagree. See [Requirements and limits](#requirements-and-limits).

## Quick start

A one-shot backup to a PersistentVolumeClaim named `backups`, kept for the last seven runs:

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooBackup
metadata:
  name: shop-manual-2026-10-17
  namespace: odoo
spec:
  instanceRef:
    name: shop
  includeFilestore: true
  destination:
    pvc:
      claimName: backups
      subPath: shop
  retention: 7
```

```bash
kubectl apply -f shop-backup.yaml
kubectl -n odoo get odoobackup shop-manual-2026-10-17 -w
kubectl -n odoo logs job/shop-manual-2026-10-17-run -c backup
```

A one-shot backup is immutable. To run another one, create a new `OdooBackup` with a new name.

## Destinations

Exactly one of `pvc` or `s3` must be set.

### PersistentVolumeClaim

```yaml
spec:
  destination:
    pvc:
      claimName: backups      # a PVC in the same namespace
      subPath: shop/daily     # optional directory inside the claim; created by the mount
```

The claim is mounted at `/backup`. The dump is written under a dot-prefixed temporary name and renamed when complete, so a half-written file never has a final name. The claim must be writable by group 100, which the pod gets through `fsGroup`.

### S3 and S3-compatible storage

```yaml
spec:
  destination:
    s3:
      bucket: odoo-backups
      prefix: shop                       # optional key prefix
      endpoint: https://minio.example.internal:9000   # optional, for non-AWS services
      region: us-east-1                  # optional, passed as AWS_DEFAULT_REGION
      credentialsSecret: backup-s3       # Secret with keys accessKeyID and secretAccessKey
      forcePathStyle: true               # usually needed for MinIO and similar services
```

Create the Secret in the same namespace as the OdooBackup:

```bash
kubectl -n odoo create secret generic backup-s3 \
  --from-literal=accessKeyID="$S3_ACCESS_KEY" \
  --from-literal=secretAccessKey="$S3_SECRET_KEY"
```

The S3 flow has two containers. An init container (the backup image) dumps into an `emptyDir` named scratch. The upload container (the AWS CLI image) copies the files to the bucket with `aws s3 cp` and then applies retention. The dump container holds the database password. The upload container holds the S3 credentials. Neither holds the other's secret.

The `emptyDir` is bounded by `scratchSize` (default `10Gi`). A dump plus its filestore archive must fit in it. If the scratch space is too small, the pod is evicted and the backup fails. Raise `scratchSize` for larger databases.

## Scheduled backups

Set `schedule` to a 5-field cron expression. The operator then keeps a CronJob in sync with the spec:

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

- A scheduled backup may change. The CronJob is updated when the spec changes.
- The CronJob uses `concurrencyPolicy: Forbid`, `startingDeadlineSeconds: 300`, and keeps 3 successful and 3 failed Jobs.
- `suspend: true` pauses the CronJob. Set it back to `false` to resume.
- A scheduled backup cannot become one-shot again. A one-shot backup cannot be changed.
- The schedule uses the time zone of the kube-controller-manager, which is UTC on most clusters.
- Scheduled runs do not fill `status.lastBackup`. Read the file names from the Job logs (`kubectl logs job/<name>`) or from the destination. `status.lastSuccessfulTime` is set from the CronJob.

## Retention

`retention: N` keeps the newest N backups of the database at the destination and deletes older ones. "Newest" is the UTC timestamp in the file name. Deleting a dump also deletes its filestore archive.

- Retention counts only files named `<database>-<timestamp>.dump`. Files of other databases, and files you put there yourself, are not touched.
- Pruning runs after the new files are written. A failed backup never deletes older ones.
- Without `retention`, every backup is kept, and the destination grows without limit.
- Retention is per database and per destination path. Two OdooBackups that write to the same prefix and the same database share the count.

## Status, phases and conditions

| Phase | Kind | Meaning |
|---|---|---|
| `Pending` | one-shot, or scheduled before the instance exists | Waiting for the OdooInstance, or for an older one-shot backup of the same instance. Requeued every 10 seconds. |
| `Running` | one-shot | The Job is running. `status.jobName` names it. |
| `Succeeded` | one-shot | The Job completed. `status.lastBackup` lists the files written. |
| `Failed` | one-shot, or scheduled when the CronJob was rejected | `status.message` has the reason and the tail of the pod log. |
| `Scheduled` | scheduled | The CronJob is in place. `status.cronJobName` names it. |

Conditions: `Succeeded`, `Progressing` and `Degraded`. The reasons are:

| Reason | Condition | Meaning |
|---|---|---|
| `Running` | Progressing, Succeeded | The Job is running. |
| `Complete` | Succeeded | The Job completed. |
| `JobFailed` | Succeeded, Degraded | The Job failed, or its Job disappeared. |
| `InvalidSpec` | Succeeded, Degraded | The spec failed a check. The message says which field. |
| `InstanceNotFound` | Succeeded | The OdooInstance does not exist (yet). |
| `Pending` | Succeeded | Another backup of the instance is queued ahead. |
| `Scheduled` | Progressing | The CronJob runs the backups. |
| `CronJobRejected` | Succeeded, Degraded | The API server rejected the CronJob. The message has the error. The operator retries on the next reconcile. |

Events are emitted on transitions: `Started`, `Succeeded` and `Failed`, all with action `Backup`.

Check the state with:

```bash
kubectl -n odoo get odoobackups
kubectl -n odoo get odoobackup shop-nightly -o jsonpath='{.status.conditions}'
```

## Restore runbook

This runbook is **guidance, not automated and not tested in this repository.** Restore into a copy first, and check the result before you touch the live database. The commands assume the default layout of an OdooInstance named `shop` in namespace `odoo`, with a database `shop` and the data PVC `shop-data`.

1. **Pick the backup.** List the destination, or read `status.lastBackup`. A backup is two files, `shop-<ts>.dump` and `shop-<ts>-filestore.tar.gz`. Restore both from the same timestamp.

2. **Stop the instance.** Odoo must not write during the restore.

   ```bash
   kubectl -n odoo scale deployment shop --replicas=0
   ```

3. **Get the files onto a volume a pod can reach.** For a PVC destination, mount the backup claim. For S3, download them first:

   ```bash
   aws s3 cp s3://odoo-backups/shop/shop-20261017T020000Z.dump ./ --endpoint-url https://minio.example.internal:9000
   ```

4. **Restore the database.** Restore into an empty database. `--clean --if-exists` drops and recreates the objects in the dump:

   ```bash
   PGPASSWORD="$DB_PASSWORD" pg_restore --clean --if-exists --no-owner \
     -h pg-rw.databases.svc.cluster.local -U odoo -d shop ./shop-20261017T020000Z.dump
   ```

5. **Restore the filestore.** Run a one-off pod that mounts the `shop-data` PVC at `/var/lib/odoo` and the backup volume, as uid 100 with `fsGroup: 100`, so the files get the same owner the Odoo pod uses. Then unpack the archive into the filestore root, which is the parent of the `shop` directory in the archive:

   ```bash
   tar -C /var/lib/odoo/filestore -xzf /backup/shop-20261017T020000Z-filestore.tar.gz
   ```

   The archive contains the `shop/` directory, so the extracted tree is `/var/lib/odoo/filestore/shop`.

6. **Start the instance and check.** Scale back up, then open the web UI and check a few records and attachments.

   ```bash
   kubectl -n odoo scale deployment shop --replicas=1
   ```

Keep the old database until the restored one is verified. A dump restored over a live database loses every change made since the dump.

## Requirements and limits

- **The data volume must be mountable.** The backup pod mounts `<instance>-data` read-only when the filestore is included. A ReadWriteOnce volume can be mounted from one node only, so the pod is pinned to the node of the running Odoo pod. For a scheduled backup, the pin is written into the CronJob. The operator refreshes it every 10 minutes, so after an Odoo pod moves, the next scheduled run may still fail until the refresh. Use ReadWriteMany on the data volume to avoid the pin.
- **The pod must run where the volume can be mounted.** If no Odoo pod runs, the pod is not pinned, and a ReadWriteOnce volume may fail to attach.
- **Backups run in the instance's namespace.** The destination claim, the credentials Secret and the database Secret must all be in the namespace of the OdooBackup.
- **The dump and the filestore are not one snapshot.** The filestore archive is taken after the dump. Odoo keeps writing in between. For a consistent pair, back up in a quiet window, or stop the instance first. For full consistency, combine this with a volume snapshot taken while the instance is stopped (see [production-guide.md](production-guide.md#backups)).
- **S3 scratch space.** The dump and the archive are staged in an `emptyDir` bounded by `scratchSize`. Size it for the largest database plus filestore you expect.
- **A PVC destination needs room for N+1 backups.** Retention prunes after the new files are written. The peak use is the retention count plus one.
- **Timeouts.** `timeoutSeconds` defaults to 3600 and is the activeDeadlineSeconds of the pod. A large database can need more. The maximum is 86400.
- **One-shot queue.** One-shot backups of an instance run one at a time, oldest first. Scheduled backups do not wait for them, and a scheduled run can overlap a one-shot run on the same instance.
- **Pod logs are best effort.** The result line (`status.lastBackup`) and the failure tail come from the pod logs. If the logs cannot be read, the status is still set, without them.
- **Not verified on real clusters.** The behaviour is covered by unit tests and envtest, which do not run pods. Run a backup and a restore on a test cluster before you rely on it.

## Security notes

- The backup pods run as uid 70 with `fsGroup` 100, with a read-only root filesystem, `allowPrivilegeEscalation: false`, all capabilities dropped, the RuntimeDefault seccomp profile, and no service account token. The pods run in the namespace of the OdooBackup.
- The database password reaches `pg_dump` through `PGPASSWORD` from the instance's Secret. It is never in the command line or the script text.
- The S3 credentials reach the AWS CLI through `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from the Secret named in `credentialsSecret`. The dump container does not get them.
- **Creating an OdooBackup is a privilege.** Anyone who may create an OdooBackup in a namespace can read that instance's database through the operator, and can make the operator use the Secrets named in the spec. Give the `odoobackups` permission only to the people who may read the database. Use a separate bucket and credentials per tenant when you run several.
- The file names contain the database name and a timestamp, not secrets.

## Configuration

Set on the manager Deployment and read when a backup pod is built:

| Variable | Default | Meaning |
|---|---|---|
| `OPERATOR_BACKUP_IMAGE` | `postgres:17-alpine` | Image that runs `pg_dump`, `tar` and the retention of PVC backups. Use an image with the same PostgreSQL client major version as your server. |
| `OPERATOR_S3_IMAGE` | `amazon/aws-cli:2.27.50` | Image that runs `aws s3 cp` and the S3 retention. |

The image must run as a non-root user, and the pod runs it as uid 70. The official `postgres` alpine image runs as uid 70, and the AWS CLI image works with a read-only root filesystem because the scripts use `/tmp`.

## Field reference

| Field | Type | Default | Rules |
|---|---|---|---|
| `spec.instanceRef.name` | string | required | OdooInstance in the same namespace. |
| `spec.database` | string | the instance's `postgres.database` | `^[a-zA-Z0-9_.-]+$`. |
| `spec.includeFilestore` | bool | `true` | Adds the filestore archive. |
| `spec.destination` | object | required | Exactly one of `pvc` or `s3`. |
| `spec.destination.pvc.claimName` | string | required | PVC in the same namespace. |
| `spec.destination.pvc.subPath` | string | root of the claim | `^[a-zA-Z0-9._/-]+$`, no `..`, no leading `/`. |
| `spec.destination.s3.bucket` | string | required | S3 bucket naming rules. |
| `spec.destination.s3.prefix` | string | none | Same rules as `subPath`. |
| `spec.destination.s3.endpoint` | string | AWS | Must start with `http://` or `https://`. |
| `spec.destination.s3.region` | string | none | Passed as `AWS_DEFAULT_REGION`. |
| `spec.destination.s3.credentialsSecret` | string | required | Secret with keys `accessKeyID` and `secretAccessKey`. |
| `spec.destination.s3.forcePathStyle` | bool | `false` | Path-style addressing. |
| `spec.retention` | int | unset (keep all) | At least 1. |
| `spec.scratchSize` | quantity | `10Gi` | S3 only. The emptyDir size limit. |
| `spec.timeoutSeconds` | int | `3600` | 60 to 86400. The pod's activeDeadlineSeconds. |
| `spec.schedule` | string | unset (one-shot) | Five fields. Makes the backup a CronJob. |
| `spec.suspend` | bool | `false` | Pauses a scheduled backup. |
| `status.phase` | string | | See [Status, phases and conditions](#status-phases-and-conditions). |
| `status.jobName`, `status.cronJobName` | string | | The Job or CronJob that runs the backup. |
| `status.lastBackup` | string | | File names of the last successful one-shot backup. |
| `status.lastScheduleTime`, `status.lastSuccessfulTime` | time | | Copied from the CronJob. |
