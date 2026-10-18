# OdooAddon reference

An `OdooAddon` points at a Git repository that holds Odoo modules. The operator syncs the repository into the addons volume of one `OdooInstance`, so the Odoo pods can load the modules. Each sync runs in a Job. The operator switches the modules to the new commit only after the Job succeeds.

## Contents

- [Minimal example](#minimal-example)
- [Spec reference](#spec-reference)
- [Validation](#validation)
- [Public and private repositories](#public-and-private-repositories)
- [Branches, tags and commit SHAs](#branches-tags-and-commit-shas)
- [Single-module and multi-module repositories](#single-module-and-multi-module-repositories)
- [Module removal, updates and conflicts](#module-removal-updates-and-conflicts)
- [Sync lifecycle and phases](#sync-lifecycle-and-phases)
- [Status](#status)
- [Resources and security of the sync Job](#resources-and-security-of-the-sync-job)
- [Examples](#examples)

## Minimal example

A public repository with several modules in folders at its root, synced from the `main` branch:

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: custom-addons
  namespace: odoo
spec:
  gitUrl: https://github.com/your-org/odoo-addons.git
  instanceRef:
    name: demo
```

`gitRef` defaults to `main`. The OdooAddon must be in the same namespace as its OdooInstance.

## Spec reference

| Field | Type | Default | Description |
|---|---|---|---|
| `gitUrl` | string | required | Repository URL. Allowed schemes: `https://`, `http://`, `ssh://`, `git@host:path`. Credentials must not be in the URL. |
| `gitRef` | string | `main` | Branch, tag, or a full 40-character lowercase commit SHA. Allowed characters: letters, digits, `.`, `_`, `-`, `/`. No `..`, no leading `-`. |
| `gitSecretRef.name` | string | none | Name of a Secret in the same namespace with the Git credentials. See [credentials](#public-and-private-repositories). |
| `addonPath` | string | repo root | A single folder name at the repo root whose subfolders are the modules (multi-module mode). Slashes are not allowed. |
| `singleAddon` | bool | `false` | `true` when the whole repository is one module. |
| `moduleName` | string | the OdooAddon name | Folder name of the module in the addons volume when `singleAddon: true`. Must match `^[a-zA-Z0-9._-]+$` and must not start with `.`. Set it to the Odoo technical name (for example `geox_theme`) when the Kubernetes name differs. |
| `instanceRef.name` | string | required | Name of the OdooInstance. |
| `instanceRef.namespace` | string | the addon's namespace | If set, must equal the OdooAddon namespace. Cross-namespace references are refused. |
| `readOnly` | bool | none | Stored only. **The operator does not use it.** The addons volume is always mounted read-only in the Odoo pods. |

`addonPath` and `moduleName` are used in a shell script inside the sync Job. The operator rejects any value that is not a plain name, so these fields cannot inject shell commands.

## Validation

Two layers check the spec.

**Admission webhook (only when the manager runs with `--enable-webhooks`).** The default manifests do not enable it. When enabled, it refuses:

- a `gitUrl` that does not start with `https://`, `http://`, `git@` or `ssh://`;
- a `gitRef` with `..`, a leading `-` or characters outside the allowed set;
- an `instanceRef.namespace` that differs from the OdooAddon namespace.

**Controller (always).** An invalid spec is not synced. The phase becomes `Failed` and `status.message` holds the reason. The controller also checks:

- `gitUrl` must not contain a password, and must not contain a user name except in `ssh://user@host` form;
- `gitSecretRef.name` must not be empty;
- `http://` with `gitSecretRef` is refused: `refusing to send git credentials over plain http; use https`;
- `addonPath`, `moduleName` and the OdooAddon name must be plain names.

## Public and private repositories

Public repositories need no secret. Private repositories need a Secret in the same namespace, referenced by `gitSecretRef.name`. Never put credentials in `gitUrl`: the controller refuses such URLs, and a URL in the spec is visible to everyone who can read the object.

### HTTPS with a token

| Secret key | Required | Use |
|---|---|---|
| `token` | yes | Personal access token or equivalent. Sent as an HTTP `Authorization: Basic` header, not in the clone URL. |
| `username` | no | Defaults to `x-access-token`. |

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: odoo-addons-git
  namespace: odoo
type: Opaque
stringData:
  token: <your-token>
  username: x-access-token
```

```yaml
spec:
  gitUrl: https://github.com/your-org/odoo-addons.git
  gitSecretRef:
    name: odoo-addons-git
  gitRef: main
  instanceRef:
    name: demo
```

### SSH

Use an `ssh://` or `git@host:path` URL. The Secret must contain both keys:

| Secret key | Required | Use |
|---|---|---|
| `ssh-privatekey` | yes | Private key with read access to the repository. |
| `known_hosts` | yes | Host keys of the Git server. The job uses `StrictHostKeyChecking=yes`. |

```bash
kubectl -n odoo create secret generic odoo-addons-ssh \
  --from-file=ssh-privatekey="$HOME/.ssh/id_ed25519_addons" \
  --from-file=known_hosts="$HOME/.ssh/known_hosts"
```

The secret is mounted read-only at `/etc/git-secret` with mode `0440`.

### Rotating credentials

The operator does not watch the Git Secret. A new token or key is used by the next sync Job. For a branch or tag, that is at most 5 minutes later (see [resync](#branches-tags-and-commit-shas)). An addon pinned to a commit SHA does not sync again, so a rotated credential has no effect on it until the `gitRef` changes.

### Missing keys

If the Secret or a required key is missing, the sync pod cannot start. The Job stays pending until its 180-second deadline, then fails. The addon ends in `Failed`, and the pod events (`kubectl describe pod`) show the cause. See [operations.md](operations.md#addon-stuck-in-syncing).

## Branches, tags and commit SHAs

- **Branch or tag** (the default): the Job runs `git clone --depth 1 --branch <gitRef>`. The addon re-syncs every 5 minutes, so a new commit on the branch is picked up within about 5 minutes. A re-sync that finds the same commit re-points the symlinks at the existing release.
- **Commit SHA** (40 lowercase hex characters): the Job fetches that exact object with `git fetch --depth 1 origin <sha>`. The server must allow fetching by SHA (GitHub does). While the addon is `Synced` at that SHA, periodic re-syncs are skipped.

For production, pin a commit SHA. Every deployment is then reproducible, and a rollback is a change of `gitRef`.

## Single-module and multi-module repositories

### Multi-module (default, `singleAddon: false`)

The operator looks at the immediate subfolders of the repository root, or of `addonPath` if set. Each subfolder that contains `__manifest__.py` becomes one module, and the module's folder name is the folder name in the repository.

```text
odoo-addons/              <- gitUrl
├── geox_theme/           <- module geox_theme (has __manifest__.py)
│   └── __manifest__.py
├── sale_custom/          <- module sale_custom
│   └── __manifest__.py
└── README.md
```

With `addonPath: addons`, the modules are the subfolders of `addons/`. `addonPath` is one folder name. `addons/custom` is rejected.

If no subfolder contains `__manifest__.py`, the sync fails with `no Odoo modules found in <dir>`. This check runs before any change to the addons volume, as does the conflict check.

### Single-module (`singleAddon: true`)

The whole repository is the module. Its root folder becomes `moduleName` in the addons volume. `addonPath` is ignored in this mode. The operator checks that `__manifest__.py` exists at the repository root, after the clone and before it changes the addons volume. If it is missing, the sync fails with `no __manifest__.py at the repository root of <moduleName>` and the volume is left as it was. The module name reported in `status.modules` is `moduleName`.

```text
geox-theme/              <- gitUrl (repository root is the module)
├── __manifest__.py
└── views/
```

Set `moduleName: geox_theme` so that the folder name matches the Odoo technical name.

## Sync lifecycle and phases

Each OdooAddon has one sync Job, named `<addon>-sync`, owned by the OdooAddon. The reconciler inspects the Job, takes one action and returns. It does not wait in a loop.

| Phase | Meaning |
|---|---|
| `Pending` | Waiting for the OdooInstance, or for the addons PVC to be `Bound`. Also used when `instanceRef.name` is empty. |
| `Syncing` | A sync Job was created or is running. |
| `Synced` | The last sync succeeded. `ready` is `true` and `clonedCommit` holds the commit. During a periodic re-sync the phase stays `Synced`. |
| `Failed` | The spec is invalid, the Job name is taken by an object this OdooAddon does not own, or the sync Job failed. `status.message` holds the reason. |

What the sync Job does:

1. Clones or fetches the repository into a scratch directory.
2. Prints the commit SHA as the last line of its log. The operator reads that line.
3. Copies each module into a release directory `/mnt/odoo/addons/.releases/<addon>/<sha>/`, if the release does not already contain it.
4. Points the module's symlink in `/mnt/odoo/addons/<module>` at the release. Each change is one rename, so Odoo never sees a half-copied module.
5. Keeps the two most recent release directories for the addon and deletes the older ones.

On success, the operator records the commit, sets `Synced` and deletes the Job. On failure, it records the reason and the log tail, sets `Failed` and deletes the Job.

Timing:

| Situation | Behaviour |
|---|---|
| Branch or tag, `Synced` | Re-sync every 5 minutes. |
| Commit SHA, `Synced` at that SHA | No re-sync. |
| `Failed` | A new Job is started every 30 seconds. A broken spec keeps failing until fixed. |
| Spec edited while the Job runs | The running Job was built from the old spec, so it is deleted. The next Job uses the new spec, once the timing rules above allow it. |

Because the operator deletes the Job after success or failure, use `status.message` rather than `kubectl logs` to read the error. Use `kubectl logs job/<addon>-sync` only while the Job still exists.

### Addons revision and the instance

The OdooInstance tracks the `clonedCommit` of every ready OdooAddon bound to it. When a commit changes, the instance rolls its pods. With `spec.upgrade` set, the rollout waits for an upgrade. See [upgrades.md](upgrades.md).

Adding or removing a ready OdooAddon for an instance also changes the revision and triggers the same rollout path.

Without `spec.upgrade`, the operator does not run `odoo -u`. The new code runs against the existing database. Module data and views are not updated until you run an upgrade yourself, see [upgrades.md](upgrades.md#manual-upgrade).

## Module removal, updates and conflicts

Each sync publishes modules as symlinks (`/mnt/odoo/addons/<module>` pointing into `/mnt/odoo/addons/.releases/<addon>/<sha>/<module>`) and records the list of modules in `.releases/<addon>/.modules`. This gives three guarantees:

- **Modules removed upstream disappear.** In a multi-module repository, a module that is no longer in the repository has its link removed on the next successful sync. Only links that point into this addon's own releases are removed.
- **Conflicts are detected before anything changes.** If another OdooAddon already provides a module with the same name, the sync fails with `module <name> is already provided by addon <other>` (see `status.message`). The check runs for all modules first, so a conflict leaves the volume untouched.
- **Deleting an OdooAddon cleans up.** The finalizer runs a short cleanup Job (`<addon>-cleanup`) that removes this addon's module links and its release directories. If the instance or its addons volume no longer exists, or the cleanup does not finish within about 3 minutes, the finalizer is removed anyway, so deletion never gets stuck.

Notes:

- Removing a module's files does not uninstall it from the Odoo database. Uninstall the module in Odoo first if you no longer want it; otherwise Odoo logs errors for an installed module that is missing on disk.
- The cleanup Job is started right after the sync Job is deleted in the background; in a narrow window a sync pod may still be finishing.
- The empty `.releases/` parent directory stays on the volume.
- Addons pinned to a commit that were synced by an older operator version are synced once more after the upgrade so that they report their modules.

## Status

| Field | Meaning |
|---|---|
| `status.phase` | `Pending`, `Syncing`, `Synced` or `Failed`. |
| `status.ready` | `true` only in `Synced`. |
| `status.clonedCommit` | Commit SHA of the last successful sync. |
| `status.modules` | Module names published by the last successful sync. The OdooInstance uses them for `status.addonPaths`. |
| `status.lastSyncTime` | When the last sync ended, successful or not. |
| `status.message` | Reason for `Failed`. For a failed Job it is the Job's failure reason, then `--- last log lines ---` and up to about 2000 bytes of the log. |
| `status.conditions` | `Ready`, `Progressing` and `Degraded`. See below. |

Conditions use the usual shape. The reasons:

| Type | Status | Reasons |
|---|---|---|
| `Ready` | `True` | `Synced`: the commit and the module count are in the message. |
| `Ready` | `False` | `InstanceNotFound` or `VolumeNotBound` (Pending), `Syncing`, `SyncFailed`, `InvalidSpec`, `JobConflict`, `ModuleConflict`. |
| `Progressing` | `True` | `Syncing`: the sync Job is running. |
| `Progressing` | `False` | `Synced`, `Pending`, or the failure reason. |
| `Degraded` | `True` | `SyncFailed`, `InvalidSpec`, `JobConflict`, `ModuleConflict`. |
| `Degraded` | `False` | `AsExpected`. |

`ModuleConflict` means the sync script found a module that another addon already provides. The log tail names both addons.

Printed columns: `Ready`, `Phase`, `Instance`, `Git URL`, `Age`.

## Resources and security of the sync Job

| Setting | Value |
|---|---|
| Image | `alpine/git:2.47.2`. Override for all sync Jobs with the `OPERATOR_SYNC_IMAGE` environment variable on the manager. It is read at start-up, so restart the manager after a change. |
| User | UID and GID `65532`, non-root. `fsGroup: 65532`. |
| Root filesystem | Read-only. `/tmp` is an emptyDir. |
| Capabilities | All dropped. `allowPrivilegeEscalation: false`. |
| Seccomp | `RuntimeDefault`. |
| Service account token | Not mounted. |
| Requests | `50m` CPU, `64Mi` memory. |
| Limits | `500m` CPU, `256Mi` memory. |
| Retries | `backoffLimit: 1`. |
| Deadline | `activeDeadlineSeconds: 180`. |
| Cleanup | The operator deletes the Job after success or failure. The Job TTL is 300 seconds as a fallback. |
| Addons volume | Mounted read-write, the only writer of `/mnt/odoo/addons`. |
| Node placement | If `addonsVolume.accessMode` is not `ReadWriteMany` and an Odoo pod is running, the Job is pinned to that pod's node. Otherwise the scheduler places it freely. |

## Examples

### Pinned commit, private HTTPS repository

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: custom-addons
  namespace: odoo
spec:
  gitUrl: https://github.com/your-org/odoo-addons.git
  gitRef: 3f2a9c1d0b7e4a5f6c8d9e0a1b2c3d4e5f607182   # full 40-character SHA
  gitSecretRef:
    name: odoo-addons-git
  instanceRef:
    name: shop
```

### Multi-module repository in a subfolder

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: shop-modules
  namespace: odoo
spec:
  gitUrl: https://github.com/your-org/shop.git
  gitRef: v2.3.0
  addonPath: addons          # modules are the subfolders of addons/
  instanceRef:
    name: shop
```

### Single module whose folder name differs from the addon name

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: geox-theme-addon
  namespace: odoo
spec:
  gitUrl: git@github.com:your-org/geox-theme.git
  gitRef: main
  singleAddon: true
  moduleName: geox_theme     # Odoo technical name
  gitSecretRef:
    name: odoo-addons-ssh    # ssh-privatekey and known_hosts
  instanceRef:
    name: shop
```

## See also

- [odooinstance.md](odooinstance.md): the instance that receives the addons.
- [upgrades.md](upgrades.md): applying module updates after a commit change.
- [operations.md](operations.md): what to do when a sync is stuck or failed.
