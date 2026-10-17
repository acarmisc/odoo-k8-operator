# Operations runbooks

Day-2 tasks with `kubectl`: how to read the state, what each phase means, and what to do when an instance, an addon or an upgrade is stuck. The examples use the namespace `odoo`, an instance `shop` and an addon `shop-modules`. Replace them with your own names. The operator runs in `odoo-k8-operator-system` by default.

## Contents

- [Read the state](#read-the-state)
- [Find the logs](#find-the-logs)
- [Instance is not ready](#instance-is-not-ready)
- [Pods are not rolling](#pods-are-not-rolling)
- [PVC is pending](#pvc-is-pending)
- [Addon stuck in Pending](#addon-stuck-in-pending)
- [Addon stuck in Syncing](#addon-stuck-in-syncing)
- [Addon in Failed](#addon-in-failed)
- [Forcing a re-sync](#forcing-a-re-sync)
- [Upgrade failed](#upgrade-failed)
- [Initialisation failed](#initialisation-failed)
- [Instance stuck in Upgrading](#instance-stuck-in-upgrading)
- [Common error messages](#common-error-messages)

## Read the state

```bash
# Custom resources and their phases
kubectl -n odoo get odooinstance,odooaddon,odooupgrade

# Everything the operator created for one instance (names, see odooinstance.md)
kubectl -n odoo get deploy/shop svc/shop cm/shop-config pvc/shop-addons pvc/shop-data pdb/shop --ignore-not-found
kubectl -n odoo get pods -l app=odoo,instance=shop

# Full status and events of one object
kubectl -n odoo describe odooinstance shop
kubectl -n odoo get odooinstance shop -o jsonpath='{.status}{"\n"}'
kubectl -n odoo get odooaddon shop-modules -o jsonpath='{.status}{"\n"}'

# Conditions only: type, status, reason and message
kubectl -n odoo get odooinstance shop -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'

# Events of one object (the operator emits them on status changes only)
kubectl -n odoo get events --field-selector involvedObject.name=shop
```

`kubectl get` prints `Ready`, `Phase` and `Age` for each kind. Add `-o wide` for the other columns.

Each kind has `Ready`, `Progressing` and `Degraded` conditions, or `Succeeded` and `Progressing` for an upgrade. The reasons are listed in [odooinstance.md](odooinstance.md#status), [odooaddon.md](odooaddon.md#status) and [upgrades.md](upgrades.md). The operator also emits these Events:

| Object | Normal | Warning |
|---|---|---|
| OdooInstance | `RolledOut`, `UpgradeStarted`, `UpgradeSucceeded`, `Initialized` | `ReconcileFailed`, `UpgradeFailed`, `InitializationFailed`, `InitializationSkipped` |
| OdooAddon | `SyncStarted`, `SyncSucceeded` | `SyncFailed`, `ModuleConflict`, `CleanupFailed`, `CleanupTimeout` |
| OdooUpgrade | `Started`, `Succeeded` | `Failed` |

Only the pods carry the `app` and `instance` labels. The Service, the ConfigMap and the PVCs are found by name.

## Find the logs

| What | Command |
|---|---|
| Operator | `kubectl -n odoo-k8-operator-system logs deploy/odoo-k8-operator-controller-manager --tail=200` |
| Odoo pods | `kubectl -n odoo logs deploy/shop --tail=200` (use `-l app=odoo,instance=shop` for all pods) |
| Sync Job (only while it exists) | `kubectl -n odoo logs job/shop-modules-sync` |
| Sync pod (while the Job exists) | `kubectl -n odoo get pods -l odoo.operator.io/addon=shop-modules` |
| Upgrade Job | `kubectl -n odoo logs job/<upgrade-name>-run` (kept for 24 hours after it ends) |

The operator deletes sync Jobs after they succeed or fail. For a failed sync, the reason and the log tail are in `status.message`. Use that rather than the Job.

Filter the operator log for one object:

```bash
kubectl -n odoo-k8-operator-system logs deploy/odoo-k8-operator-controller-manager --tail=500 | grep 'shop'
```

## Instance is not ready

**Symptom:** `READY` is `false`, or `PHASE` is `Creating` for a long time.

1. Check the pods:

   ```bash
   kubectl -n odoo get pods -l app=odoo,instance=shop
   kubectl -n odoo describe pod -l app=odoo,instance=shop
   ```

   - `Pending` with `FailedScheduling`: not enough resources, or a volume cannot be scheduled. See [PVC is pending](#pvc-is-pending).
   - `CreateContainerConfigError`: a referenced Secret or key is missing. The event names it. Create the Secret, with key `password`.
   - `ImagePullBackOff`: the image name is wrong, or the namespace's `default` ServiceAccount has no pull secret.
   - Running but not ready: the probe on `/web/health` fails. Read the Odoo log.

2. Read the Odoo log:

   ```bash
   kubectl -n odoo logs deploy/shop --tail=100
   ```

   Typical causes: the database cannot be reached, TLS is not accepted (`db_sslmode = require`), the password is wrong, or the database does not exist or is not initialised.

3. If `Degraded` is `True`, or `PHASE` is `Failed`, the operator refused or could not reconcile the instance. The `Degraded` condition gives the reason and the message, and the `ReconcileFailed` Event names the same error:

   ```bash
   kubectl -n odoo get odooinstance shop -o jsonpath='{.status.conditions[?(@.type=="Degraded")]}{"\n"}'
   kubectl -n odoo describe odooinstance shop
   ```

   The messages are listed in [Common error messages](#common-error-messages).

## Pods are not rolling

**Symptom:** you changed `spec.config`, a Secret or an addon, but the pods still run the old version.

1. Check the phase first. `Upgrading` and `UpgradeFailed` hold the pods on the previous addons revision by design. See [Instance stuck in Upgrading](#instance-stuck-in-upgrading).
2. Compare the pod-template hashes with what you expect:

   ```bash
   kubectl -n odoo get deploy shop -o jsonpath='{.spec.template.metadata.annotations}{"\n"}'
   kubectl -n odoo get odooinstance shop -o jsonpath='{.status.appliedAddonsHash}{"\n"}'
   ```

   `odoo.operator.io/config-hash` changes with `spec.config` or `spec.postgres`. `odoo.operator.io/secrets-hash` changes with the content of the referenced Secrets. `odoo.operator.io/addons-hash` changes with the addon revisions.

3. With `replicas: 1` and a non-RWX filestore, the strategy is `Recreate`. The old pod stops first, so a rollout is a short outage. This is expected.
4. A new pod that does not become ready blocks the rollout with `maxUnavailable: 0`. Check its events and logs, as in [Instance is not ready](#instance-is-not-ready).

## PVC is pending

**Symptom:** a PVC stays `Pending`. Its pods stay `Pending` too, and an addon waits in `Pending`.

```bash
kubectl -n odoo get pvc
kubectl -n odoo describe pvc shop-addons
kubectl get storageclass -o custom-columns=NAME:.metadata.name,PROVISIONER:.provisioner,BINDING:.volumeBindingMode
```

- **No StorageClass, or a wrong name.** The events name the missing class. Set `storageClass` to an existing class, or fix the default class.
- **No provisioner running.** The events show waiting for a volume. Check the storage driver's pods.
- **`WaitForFirstConsumer`.** The PVC binds only when a pod using it is scheduled. A `Pending` PVC before the first pod is normal in this case. The addon also waits in `Pending` until the PVC is `Bound`. The Odoo pod must be scheduled first.
- **`ReadWriteMany` requested, class does not support it.** The provisioning events show the failure. Use an RWX class, or set `replicas: 1`.

Changing `size` on an existing PVC is not applied by the operator. Resize the PVC directly if your class allows expansion.

## Addon stuck in Pending

**Symptom:** `PHASE` is `Pending`.

Pending means one of three things:

- `spec.instanceRef.name` names no OdooInstance in the same namespace. Check the name and `instanceRef.namespace`.
- The instance's addons PVC `<instance>-addons` is not `Bound`. See [PVC is pending](#pvc-is-pending).
- `instanceRef.name` is empty.

```bash
kubectl -n odoo get odooinstance shop
kubectl -n odoo get pvc shop-addons
```

## Addon stuck in Syncing

**Symptom:** `PHASE` is `Syncing` for more than a few minutes.

```bash
kubectl -n odoo get job shop-modules-sync
kubectl -n odoo get pods -l odoo.operator.io/addon=shop-modules
kubectl -n odoo describe pod -l odoo.operator.io/addon=shop-modules
kubectl -n odoo logs job/shop-modules-sync
```

Common causes:

- **`CreateContainerConfigError`.** The Git Secret or a required key is missing (`token`, or `ssh-privatekey` and `known_hosts`). Create it. The next attempt starts by itself.
- **Pod pending with a `Multi-Attach` error.** The addons volume is `ReadWriteOnce` and is attached to another node. The Job is pinned to the node of a running Odoo pod, but if no Odoo pod is running, the scheduler may place it elsewhere. Start the instance, or use an RWX volume.
- **Image pull errors.** The sync image cannot be pulled. Check `OPERATOR_SYNC_IMAGE` and the registry access.
- **Slow clone.** The Job has a 180-second deadline. A large repository can hit it. The addon then fails with `DeadlineExceeded`. There is no setting for this deadline.

The Job is removed by the operator when it ends, so read `kubectl logs job/...` while it runs.

## Addon in Failed

**Symptom:** `PHASE` is `Failed`.

Read the message first. The `Degraded` condition holds the same message, with the reason (`SyncFailed`, `ModuleConflict`, `InvalidSpec` or `JobConflict`):

```bash
kubectl -n odoo get odooaddon shop-modules -o jsonpath='{.status.message}{"\n"}'
```

The message is the reason, and for a failed Job it is followed by `--- last log lines ---` and the end of the log. Match it against [Common error messages](#common-error-messages).

Things to know:

- **A failed addon retries every 30 seconds.** Each retry creates a new sync Job. This keeps the message up to date. Once the cause is fixed, the next attempt succeeds without any action.
- **Editing the spec** takes effect at the next attempt. For a failed addon that is within 30 seconds.
- **Git authentication errors** (`Authentication failed`, `Permission denied (publickey)`, `Repository not found`) show in the log tail. Check the Secret, the key or token scope, and that the URL is right.

## Forcing a re-sync

Use these in order. Each one is safe.

1. **Wait.** A branch or tag `Synced` addon re-syncs by itself every 5 minutes. A failed addon retries every 30 seconds.
2. **Edit the spec** only when you have a reason. A `Synced` addon waits for its cooldown (up to 5 minutes) before the next Job, so the change is not immediate.
3. **Delete and re-create the OdooAddon** when you need a sync now. The new object has no sync history, so it syncs at once. Wait until the old Job is gone first, or the new addon may report `job name conflict`. Keep only the name, namespace and spec in the saved file:

   ```bash
   kubectl -n odoo get odooaddon shop-modules -o yaml > shop-modules.yaml
   # edit shop-modules.yaml: keep apiVersion, kind, metadata.name, metadata.namespace and spec only
   kubectl -n odoo delete odooaddon shop-modules
   kubectl -n odoo wait --for=delete job/shop-modules-sync --timeout=120s
   kubectl -n odoo apply -f shop-modules.yaml
   ```

**Do not** delete the sync Job to force a sync. The cooldown still applies, so the Job is created later, not now.

## Upgrade failed

**Symptom:** the OdooInstance is `UpgradeFailed`, or an OdooUpgrade is `Failed`.

1. Read the message. The `Succeeded` condition has the same reason and message:

   ```bash
   kubectl -n odoo get odooupgrade
   kubectl -n odoo get odooupgrade shop-upgrade-<hash8> -o jsonpath='{.status.message}{"\n"}'
   ```

2. Read the Job log, which is kept for 24 hours:

   ```bash
   kubectl -n odoo logs job/shop-upgrade-<hash8>-run
   ```

   Odoo errors show up here. A module Odoo cannot find usually means the module name in `spec.upgrade.modules` does not match the folder name. Check `moduleName` on the OdooAddon.

3. Fix the cause. Common ones:
   - the database is not reachable, or the password is wrong;
   - the module name is wrong;
   - the upgrade needs more time: the Job hit `timeoutSeconds`. Raise it in `spec.upgrade.timeoutSeconds`, or in the spec of a manual OdooUpgrade;
   - the database name is missing: set `postgres.database` or `spec.database`.

4. Retry:

   - **Automatic upgrade** (from `spec.upgrade`): delete the failed OdooUpgrade. The instance re-creates it on the next reconcile, which is immediate.

     ```bash
     kubectl -n odoo delete odooupgrade shop-upgrade-<hash8>
     ```

   - **Manual upgrade:** the spec is immutable, so create a new OdooUpgrade with a new name. Delete the failed one too.

## Initialisation failed

**Symptom:** the OdooInstance phase is `InitializationFailed`, and its `Degraded` condition is `True` with reason `InitializationFailed`. The Deployment has 0 replicas.

1. Read the message:

   ```bash
   kubectl -n odoo get odooupgrade shop-init -o jsonpath='{.status.message}{"\n"}'
   ```

2. Read the Job log, which is kept for 24 hours:

   ```bash
   kubectl -n odoo logs job/shop-init-run
   ```

3. Fix the cause. Common ones:
   - the database is not reachable, or the database password is wrong;
   - the database does not exist, and the database user has no `CREATEDB`;
   - a module in `spec.initialize.modules` is not in the addons, because its OdooAddon is not Synced or the name is wrong;
   - the Secret `spec.initialize.adminUserPasswordSecret` is missing (the reason is `MissingSecret`, and no Job is created);
   - the run hit `timeoutSeconds`.

   If the failed run already created part of the database, restore an empty database before you retry. The operator never drops data.

4. Retry. Delete the failed OdooUpgrade and wait for its Job to be removed. `--cascade=foreground` waits for the Job, so the new run does not adopt the old one:

   ```bash
   kubectl -n odoo delete odooupgrade shop-init --cascade=foreground
   ```

   The operator re-creates `shop-init` on the next reconcile. The Deployment stays at 0 replicas until the new run succeeds.

If you only added `spec.initialize` to a Deployment that already runs pods, nothing is run. The instance is marked initialised and the `InitializationSkipped` Event explains it. Remove `spec.initialize` if it is not needed.

## Instance stuck in Upgrading

**Symptom:** `PHASE` is `Upgrading`. The pods run the previous addons revision. This is intended while the upgrade runs.

```bash
kubectl -n odoo get odooinstance shop -o jsonpath='{.status.activeUpgrade}{"\n"}'
kubectl -n odoo get odooupgrade <that-name> -o wide
```

- `Pending` with `waiting for upgrade "..." to finish`: an earlier upgrade for this instance is still running. Upgrades run one at a time. Wait for it, or look at it as above.
- `Pending` with `waiting for OdooInstance`: the instance does not exist. Re-create it or check the name.
- `Running`: wait. Check the Job log with `kubectl -n odoo logs job/<name>-run -f`.

Deleting a `Running` OdooUpgrade also deletes its Job. The operator then creates the upgrade again and runs it from the start. Do this only when the Job is stuck and you have checked that the database can safely be upgraded again.

## Common error messages

### Instance (`Degraded` condition message)

| Message | Meaning and fix |
|---|---|
| `secret "<name>" referenced by spec.postgres.passwordSecret does not exist in namespace "<ns>"` | Create the Secret in the instance's namespace, with key `password`. The same message exists for `spec.adminPasswordSecret`. |
| `config key "<key>" is not permitted in spec.config` | The key is `admin_passwd`, `db_password` or `dbfilter`. Use `adminPasswordSecret` for the master password. |
| `config key "<key>" contains illegal characters` | The key is empty, or contains `=`, `[`, `]` or a newline. |
| `config value for key "<key>" contains illegal newline characters` | Remove the line break from the value. |
| `invalid size "<size>" for PVC <name>` | Use a Kubernetes quantity such as `10Gi`. |
| `OdooUpgrade "<name>" belongs to instance "<x>", not "<y>"` | Two instances produced the same truncated upgrade name. Rename one instance. |

### Addon (`status.message`)

| Message | Meaning and fix |
|---|---|
| `gitUrl scheme not allowed` | The URL must start with `https://`, `http://`, `ssh://` or `git@`. |
| `gitUrl must not embed credentials; put them in gitSecretRef instead` | Remove the user name and password from the URL. Use `gitSecretRef`. |
| `refusing to send git credentials over plain http; use https` | `http://` with `gitSecretRef`. Use `https://`. |
| `invalid gitRef: "..."` or `gitRef contains disallowed characters` | The ref contains `..`, starts with `-`, or has a character outside letters, digits, `.`, `_`, `-`, `/`. |
| `invalid moduleName`, `invalid addonPath`, `invalid addon name` | Use a plain name without slashes. `moduleName` must not start with `.`. `addonPath` is one folder. |
| `gitSecretRef.name must not be empty` | Set the name, or remove the `gitSecretRef` block. |
| `cross-namespace instanceRef is not supported` | Put the OdooAddon in the instance's namespace. |
| `job name conflict` | A Job named `<addon>-sync` exists and this OdooAddon does not own it. Check its labels. Delete it if it is a leftover you created. |
| `no Odoo modules found in <dir>` | In multi-module mode, no direct subfolder has `__manifest__.py`. Check `addonPath` and the repository layout. |
| `DeadlineExceeded` | The Job ran for more than 180 seconds. See [Addon stuck in Syncing](#addon-stuck-in-syncing). |
| `BackoffLimitExceeded` | The Job failed twice. Read the log tail in the message. |
| `sync job <name> failed` | The Job failed without a reason. Read the log tail. |
| `sync job <name> produced no commit hash output` | The script ended without printing a SHA. Read the log tail. |
| `Authentication failed`, `Permission denied (publickey)`, `Repository not found` (in the log tail) | Git cannot authenticate, or the repository does not exist or is not visible to the credentials. |

### Initialisation (`Degraded` condition message)

| Message | Meaning and fix |
|---|---|
| `secret "<name>" referenced by spec.initialize.adminUserPasswordSecret does not exist` | Create the Secret with key `password`. Reason `MissingSecret`. |
| `OdooUpgrade "<name>-init" failed; pods stay at 0 replicas: ...` | Read the Job log, as in [Initialisation failed](#initialisation-failed). |

### Upgrade (`status.message`)

| Message | Meaning and fix |
|---|---|
| `waiting for OdooInstance` | Normal until the instance exists. |
| `waiting for upgrade "<name>" on the same instance to finish` | Normal queueing. |
| `no database configured` | Set `spec.database`, or `postgres.database` on the instance. |
| `modules: "all" must be the only entry when used` | Use `["all"]` alone, or list the modules. |
| `upgrade Job <name> no longer exists` | The Job was deleted before the upgrade finished. Retry. |
| `upgrade pod failed` | The pod failed without a reason. Read the Job log. |
| `DeadlineExceeded` | The run hit `timeoutSeconds`. Raise it, then retry. |
| `adminUserPasswordSecret requires installModules` | Set `installModules`, or remove `adminUserPasswordSecret`. |

## See also

- [odooaddon.md](odooaddon.md): the sync lifecycle these runbooks refer to.
- [upgrades.md](upgrades.md): the upgrade phases and the retry rules.
- [installation.md](installation.md#uninstalling): what happens to data on removal.
