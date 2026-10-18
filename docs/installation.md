# Installation

This page covers what you need before you install, how to install the CRDs and the operator, how the RBAC is laid out, which manager flags exist, and how to upgrade and remove the operator. The default manifests need no cert-manager.

## Contents

- [Prerequisites](#prerequisites)
- [Install the CRDs](#install-the-crds)
- [Deploy the operator](#deploy-the-operator)
- [Namespaces and RBAC](#namespaces-and-rbac)
- [Manager flags and environment](#manager-flags-and-environment)
- [Metrics and health probes](#metrics-and-health-probes)
- [Admission webhook](#admission-webhook)
- [Upgrading the operator](#upgrading-the-operator)
- [Uninstalling](#uninstalling)

## Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes | Tested in CI on kind node `v1.34.0` (end-to-end tests) and against envtest for Kubernetes `1.37` (unit and controller tests). Other versions are not tested. |
| `kubectl` | Configured for the target cluster. Installing CRDs and RBAC needs cluster-admin rights. |
| Container registry | The manager image must be pullable by the cluster. The default image is `ghcr.io/acarmisc/odoo-k8-operator`. |
| StorageClass | A default class, or set `storageClass` on each volume. |
| ReadWriteMany storage | Only for `replicas > 1`. See [production-guide.md](production-guide.md#storage). |
| PostgreSQL | Reachable from the pods, and it must accept TLS. The operator sets `db_sslmode = require`. |
| Go 1.26 and network access | Only for the `make` targets. They download kustomize `v5.8.2` and controller-gen `v0.23.0` into `bin/`. |
| cert-manager | Not required. Only needed if you enable the admission webhook (see [below](#admission-webhook)). |

## Install the CRDs

```bash
make install
```

This runs `make manifests` and applies `config/crd` (four CRDs: `odooinstances`, `odooaddons`, `odooupgrades`, `odoobackups`, group `odoo.operator.io`, version `v1`). Without `make`:

```bash
kubectl apply -k config/crd
```

Check:

```bash
kubectl get crd | grep odoo.operator.io
```

## Deploy the operator

Always pass `IMG`. The Makefile default is `controller:latest`, which is not a published image.

```bash
make deploy IMG=ghcr.io/acarmisc/odoo-k8-operator:<tag>
```

`<tag>` is one of the tags published by the build pipeline (see [development.md](development.md#image-tags-and-signature-verification)).

`make deploy` runs `kustomize edit set image` in `config/manager`, which rewrites `config/manager/kustomization.yaml` in your checkout. Do not commit that change unless you mean to.

The deploy applies `config/default`. It creates:

- the namespace `odoo-k8-operator-system`;
- the ServiceAccount `odoo-k8-operator-controller-manager`;
- the ClusterRole `odoo-k8-operator-manager-role` and its RoleBinding in the operator namespace;
- the leader-election and metrics RBAC;
- the Deployment `odoo-k8-operator-controller-manager` with one replica;
- the metrics Service `odoo-k8-operator-controller-manager-metrics-service`.

Check:

```bash
kubectl -n odoo-k8-operator-system get deploy,pods
kubectl -n odoo-k8-operator-system logs deploy/odoo-k8-operator-controller-manager
```

### Alternative: a single install file

```bash
make build-installer IMG=ghcr.io/acarmisc/odoo-k8-operator:<tag>
kubectl apply -f dist/install.yaml
```

`build-installer` writes the CRDs and the manager manifests into `dist/install.yaml`.

### Kustomize overlay

To change the namespace, the image or the labels, put an overlay next to a checkout of this repository:

```yaml
# overlays/prod/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../odoo-k8-operator/config/default
images:
  - name: controller
    newName: ghcr.io/acarmisc/odoo-k8-operator
    newTag: <tag>
```

```bash
kubectl apply -k overlays/prod
```

The `config/default` kustomization sets the namespace `odoo-k8-operator-system`. Override it in the overlay if you need another one.

## Namespaces and RBAC

### The model

The RBAC is one ClusterRole, `manager-role`, used through namespace-scoped RoleBindings. The source file is `config/rbac/role.yaml`. Its comment says the role is for per-namespace RoleBindings and that a ClusterRoleBinding should not be used in production.

The default RoleBinding is in the operator namespace only. To let the operator manage OdooInstances in another namespace, create a RoleBinding there:

```bash
kubectl create namespace odoo
kubectl -n odoo create rolebinding odoo-operator \
  --clusterrole=odoo-k8-operator-manager-role \
  --serviceaccount=odoo-k8-operator-system:odoo-k8-operator-controller-manager
```

Check the ClusterRole name first with `kubectl get clusterrole | grep manager-role`. The name is prefixed by the kustomize `namePrefix`.

### Which namespaces the operator watches

The manager only watches the namespaces listed in `--watch-namespaces` (comma separated), whose default is the environment variable `WATCH_NAMESPACES`. The default manifest sets that variable to the operator's own namespace (from the downward API), which matches the default RoleBinding: the operator can read and write only where it has been granted access, and its cache never asks for cluster-wide permissions.

To manage OdooInstances in another namespace you need **both**:

1. a RoleBinding in that namespace (the `kubectl create rolebinding` command above), and
2. the namespace in `WATCH_NAMESPACES`, for example:

   ```bash
   kubectl -n odoo-k8-operator-system set env deployment/odoo-k8-operator-controller-manager \
     WATCH_NAMESPACES=odoo-k8-operator-system,odoo
   ```

   In a kustomize overlay, patch the `manager` container `env` instead (a later `make deploy` resets a manual change).

If you prefer cluster-wide operation, bind `manager-role` with a ClusterRoleBinding and remove the flag (empty means all namespaces). This gives the operator read access to Secrets in every namespace, which is the trade-off the role comment warns about.

If a namespace is missing from either place the objects there are ignored (no RoleBinding: the log shows `forbidden`; not in `WATCH_NAMESPACES`: nothing happens). Check the manager log after changing it.

### Permissions and why

| API group | Resources | Verbs | Why |
|---|---|---|---|
| core | `pods` | get, list, watch, create, update, patch, delete | The code reads pods to find the node of a running Odoo pod (node pinning) and to find the pod that holds a Job's logs. No write verbs on pods are used. |
| core | `pods/log` | get | Reads the log tail of sync and upgrade pods for `status.message`. |
| core | `services`, `configmaps`, `persistentvolumeclaims` | get, list, watch, create, update, patch, delete | Create and update the Service, the `odoo.conf` ConfigMap and the PVCs. Owned objects are removed by Kubernetes garbage collection, not by the operator. |
| core | `secrets` | get, list, watch | Hash the Secrets an instance references, and re-reconcile the instance when one changes. The list and watch are needed by the cached client. |
| apps | `deployments` | full | Odoo Deployments. |
| policy | `poddisruptionbudgets` | full | PDB when `replicas > 1`. |
| batch | `jobs` | full | Sync Jobs, upgrade Jobs and backup Jobs. The operator deletes sync Jobs when they finish. |
| batch | `cronjobs` | full | CronJobs of scheduled OdooBackups. Created, updated when the spec changes, and deleted with the backup. |
| events.k8s.io | `events` | create, patch | Emit Events on transitions (sync, upgrade, rollout, errors). |
| odoo.operator.io | the three CRDs, their `/status` and `/finalizers` | full | Reconcile and status writes, and finalizers. |

Leader election uses a namespaced Role in the operator namespace (`configmaps` and `coordination.k8s.io/leases`).

The metrics endpoint uses two more ClusterRoles, `metrics-auth-role` (TokenReview and SubjectAccessReview create) and `metrics-reader` (GET `/metrics`).

## Manager flags and environment

The flags are defined in `cmd/main.go`. The default manifests set `--leader-elect`, `--health-probe-bind-address=:8081`, and (through a patch) `--metrics-bind-address=:8443`.

| Flag | Default in binary | Default manifests | Meaning |
|---|---|---|---|
| `--leader-elect` | `false` | `true` | Only one manager is active. Lease ID `686a6249.odoo.io`. |
| `--health-probe-bind-address` | `:8081` | `:8081` | `/healthz` and `/readyz`. |
| `--metrics-bind-address` | `0` (disabled) | `:8443` | Metrics endpoint. `0` turns it off. |
| `--metrics-secure` | `true` | unset (`true`) | Serve metrics over HTTPS with authentication and authorization. |
| `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key` | `""`, `tls.crt`, `tls.key` | unset | Use a certificate from a directory instead of the generated self-signed one. |
| `--enable-webhooks` | `false` | unset | Register the OdooAddon admission webhook. |
| `--webhook-cert-path`, `--webhook-cert-name`, `--webhook-cert-key` | `""`, `tls.crt`, `tls.key` | unset | Webhook certificate directory and file names. |
| `--enable-http2` | `false` | unset | HTTP/2 is off by default because of known HTTP/2 CVEs. |
| `--watch-namespaces` | `$WATCH_NAMESPACES`, empty = all namespaces | unset (the env var below is used) | Namespaces to watch and manage. Must match the RoleBindings you created. |
| `--addon-max-concurrent-reconciles` | `4` | unset (`4`) | How many OdooAddons reconcile at once. Values below 1 mean 1. |
| `--zap-*` | controller-runtime defaults | unset | Log format and level. |

Environment variables read by the manager:

| Variable | Default | Meaning |
|---|---|---|
| `WATCH_NAMESPACES` | operator namespace in the default manifest | Default of `--watch-namespaces`. Comma-separated list; empty means all namespaces (needs a ClusterRoleBinding). |
| `OPERATOR_SYNC_IMAGE` | `alpine/git:2.47.2` | Image for every sync Job. Read once at start-up. Restart the manager after changing it. |
| `OPERATOR_BACKUP_IMAGE` | `postgres:17-alpine` | Image for the `pg_dump` step of every backup. See [backups.md](backups.md#configuration). |
| `OPERATOR_S3_IMAGE` | `amazon/aws-cli:2.27.50` | Image for the S3 upload step of backups to S3. See [backups.md](backups.md#configuration). |

Example: raise the addon concurrency:

```yaml
# In config/manager/manager.yaml, or patch the Deployment:
args:
  - --leader-elect
  - --health-probe-bind-address=:8081
  - --addon-max-concurrent-reconciles=8
```

## Metrics and health probes

- Probes: `/healthz` and `/readyz` on port `8081`.
- Metrics: HTTPS on port `8443` in the default manifests. Requests need a token that the `metrics-reader` ClusterRole allows.
- `config/prometheus` contains a `ServiceMonitor` for the metrics Service. It is not part of the default deploy. Its TLS setting uses `insecureSkipVerify: true` (see the TODO in that file). See [production-guide.md](production-guide.md#monitoring).
- `config/network-policy` contains a NetworkPolicy that allows metrics traffic from namespaces labelled `metrics: enabled`. It is not part of the default deploy.

## Admission webhook

The OdooAddon validating webhook exists in `api/v1/odooaddon_webhook.go`. It is off by default, and the default manifests do not include `config/webhook`. To use it:

1. Start the manager with `--enable-webhooks`.
2. Uncomment the `[WEBHOOK]` sections in `config/default/kustomization.yaml` and `config/crd/kustomization.yaml`, and add `../webhook`.
3. Provide a serving certificate. The manifests comment on cert-manager for this. Without it, mount your own certificate and pass `--webhook-cert-path`.

The controller validates the same rules when the webhook is off, so the webhook is an early check, not a requirement. See [odooaddon.md](odooaddon.md#validation).

## Upgrading the operator

There is one API version (`v1`), so there is no conversion webhook.

1. Read the release notes for the new version.
2. Apply the CRDs first, so the new fields exist before the new manager starts:

   ```bash
   make install
   ```

3. Deploy the new image:

   ```bash
   make deploy IMG=ghcr.io/acarmisc/odoo-k8-operator:<new-tag>
   ```

4. Check the rollout and the manager log:

   ```bash
   kubectl -n odoo-k8-operator-system rollout status deploy/odoo-k8-operator-controller-manager
   kubectl get odooinstances,odooaddons,odooupgrades,odoobackups -A
   ```

The controllers keep no state of their own. Restarting the manager does not lose progress. Jobs and status are the state. The rollout of the manager does not restart Odoo pods.

## Uninstalling

Order matters. Each OdooInstance, OdooAddon and OdooUpgrade has a finalizer, and only the running manager removes it.

1. Delete the custom resources while the manager runs:

   ```bash
   kubectl delete odooupgrades,odooaddons,odooinstances --all -A
   ```

   Deleting an OdooInstance removes its Deployment, Service, ConfigMap, PDB, addons PVC and generated upgrades.

2. Remove the manager:

   ```bash
   make undeploy
   ```

3. Remove the CRDs:

   ```bash
   make uninstall
   ```

`make uninstall` deletes the CRDs, and with them every remaining custom resource. If you remove the manager first, the finalizers stay on the objects. They then stay in `Terminating` and block the CRD deletion. As a last resort, after the manager is gone, remove a stuck finalizer by hand:

```bash
kubectl -n <ns> patch odooinstance <name> --type merge -p '{"metadata":{"finalizers":null}}'
```

### What happens to the data

| Volume | Default behaviour | Result |
|---|---|---|
| `<name>-addons` | Always owned by the instance. | Deleted with the instance. The modules live in Git, so this is usually fine. |
| `<name>-data` (filestore) | `dataVolume.retainOnDelete: true`. Not owned. | Kept after the instance is deleted, with its data. Delete it by hand when you no longer need it. |
| `<name>-data` with `retainOnDelete: false` | Owned by the instance. | Deleted with the instance. |

Whether the underlying volume survives a PVC delete depends on the StorageClass `reclaimPolicy`. A `Delete` policy removes the volume. Your PostgreSQL database is never touched by the operator. See [production-guide.md](production-guide.md#backups).

Delete a retained filestore only after you have confirmed you do not need it:

```bash
kubectl -n odoo delete pvc demo-data
```

## See also

- [odooinstance.md](odooinstance.md): what the instance creates.
- [production-guide.md](production-guide.md): storage, security and monitoring before go-live.
- [operations.md](operations.md): day-2 runbooks.
