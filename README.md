# Odoo Kubernetes Operator

The Odoo Kubernetes Operator runs Odoo instances and your custom Odoo modules on Kubernetes. You describe an Odoo deployment with an `OdooInstance` and point an `OdooAddon` at a Git repository. The operator creates the Deployment, Service, ConfigMap and volumes, syncs the modules into the instance, and rolls the pods when the config, a Secret or the code changes. It can also run module upgrades with `odoo -u` before the new code goes live.

## Features

- **One resource per Odoo instance.** `OdooInstance` creates the Deployment, Service, ConfigMap, PVCs and, for more than one replica, a PodDisruptionBudget.
- **Git addons.** `OdooAddon` syncs HTTPS or SSH repositories, branches, tags or pinned commit SHAs. Single-module and multi-module repositories are both supported.
- **Atomic module updates.** Each sync publishes modules as symlinks into a release directory, so Odoo never sees a half-copied module.
- **Automatic rollouts.** Pods roll when `spec.config`, a referenced Secret or a synced commit changes. Passwords are never written to the ConfigMap.
- **Module upgrades.** `spec.upgrade` runs `odoo -u` after the code changes, and holds the pods on the old code until the upgrade succeeds. You can also run an `OdooUpgrade` by hand.
- **Hardened pods.** Non-root, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp, no service account token.
- **Non-blocking reconciles.** Syncs and upgrades run as Jobs. Many addons sync in parallel.

## Requirements

- **Kubernetes.** Tested in CI on kind node `v1.34.0` (end-to-end test) and with envtest for Kubernetes `1.37` (unit and controller tests). Other versions are not tested.
- **kubectl** with cluster-admin rights for the install.
- **A container registry** the cluster can pull the operator image from.
- **A StorageClass.** For more than one replica, the storage must support `ReadWriteMany`.
- **PostgreSQL** that accepts TLS. The operator always sets `db_sslmode = require`.
- **cert-manager is not required.** It is only needed if you enable the admission webhook, which the default manifests do not.
- **Go 1.26 and network access** only if you build the operator or use the `make` targets.

## Quick start

These steps take you from an empty namespace to a running Odoo on a cluster with a default StorageClass. They assume a PostgreSQL database `odoo` is reachable at `postgres.databases.svc.cluster.local` and already initialised. If it is empty, see the note after step 5.

**1. Install the CRDs.**

```bash
make install
```

**2. Deploy the operator.** Always pass `IMG`. The default image is `controller:latest`, which is not published.

```bash
make deploy IMG=ghcr.io/acarmisc/odoo-k8-operator:<tag>
kubectl -n odoo-k8-operator-system get pods
```

**3. Create a namespace for Odoo and let the operator manage it.** The operator watches only the namespaces it was granted: bind its role in your namespace **and** add the namespace to `WATCH_NAMESPACES` (see [installation.md](docs/installation.md#namespaces-and-rbac)).

```bash
kubectl create namespace odoo
kubectl -n odoo create rolebinding odoo-operator \
  --clusterrole=odoo-k8-operator-manager-role \
  --serviceaccount=odoo-k8-operator-system:odoo-k8-operator-controller-manager
kubectl -n odoo-k8-operator-system set env deployment/odoo-k8-operator-controller-manager \
  WATCH_NAMESPACES=odoo-k8-operator-system,odoo
```

**4. Create the Secrets.** Each one holds one key, `password`.

```bash
kubectl -n odoo create secret generic odoo-db    --from-literal=password='<db-password>'
kubectl -n odoo create secret generic odoo-admin --from-literal=password='<master-password>'
```

**5. Create an OdooInstance.**

```yaml
# demo-instance.yaml
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
    passwordSecret: odoo-db
  adminPasswordSecret: odoo-admin
```

```bash
kubectl apply -f demo-instance.yaml
```

If the database is empty, install the base module with an OdooUpgrade. The database must exist. See [upgrades.md](docs/upgrades.md#manual-upgrade). This path is not yet confirmed on a live cluster.

```yaml
apiVersion: odoo.operator.io/v1
kind: OdooUpgrade
metadata:
  name: demo-init
  namespace: odoo
spec:
  instanceRef:
    name: demo
  installModules: ["base"]
```

**6. Create an OdooAddon** for a repository of modules. This example uses a public repository and the `main` branch.

```yaml
# demo-addon.yaml
apiVersion: odoo.operator.io/v1
kind: OdooAddon
metadata:
  name: custom-addons
  namespace: odoo
spec:
  gitUrl: https://github.com/your-org/odoo-addons.git
  gitRef: main
  instanceRef:
    name: demo
```

```bash
kubectl apply -f demo-addon.yaml
```

**7. Check the status.**

```bash
kubectl -n odoo get odooinstance,odooaddon
kubectl -n odoo get pods -l app=odoo,instance=demo
```

The instance is ready when `READY` is `true` and `PHASE` is `Running`. The addon is ready when `PHASE` is `Synced`. If not, see [operations.md](docs/operations.md).

**8. Open Odoo.**

```bash
kubectl -n odoo port-forward svc/demo 8069:8069
```

Then open <http://localhost:8069>.

## Concepts

| Resource | Short name | Purpose |
|---|---|---|
| `OdooInstance` | none | One Odoo deployment: image, database, storage, `odoo.conf` settings, resources. Owns the Deployment, Service, ConfigMap, PVCs and PDB. |
| `OdooAddon` | none | A Git repository of modules, synced into one OdooInstance's addons volume. Has phases `Pending`, `Syncing`, `Synced`, `Failed`. |
| `OdooUpgrade` | none | A one-shot `odoo -u` or `-i` run against an instance's database. Created by hand or by `spec.upgrade`. Has phases `Pending`, `Running`, `Succeeded`, `Failed`. |

Use the full kind names with `kubectl`, for example `kubectl get odooinstance`.

## Production checklist

Before you run real users on an instance, check these. Each item links to the details.

- [ ] **Storage.** Use `ReadWriteMany` on both volumes for more than one replica, and confirm the class really supports it. ([production-guide](docs/production-guide.md#storage))
- [ ] **Secrets.** Keep the database and master passwords in Secrets. Never put credentials in `gitUrl`. ([production-guide](docs/production-guide.md#secrets))
- [ ] **Code.** Pin `gitRef` to a commit SHA, and use `gitSecretRef` for private repositories. ([odooaddon](docs/odooaddon.md#branches-tags-and-commit-shas))
- [ ] **Upgrades.** Decide whether `spec.upgrade` should gate rollouts, and test it on a copy first. ([upgrades](docs/upgrades.md))
- [ ] **Network.** Add a NetworkPolicy for each instance. The operator does not create one. ([production-guide](docs/production-guide.md#security))
- [ ] **Resources.** Set `resources` explicitly. The defaults are small for Odoo with several workers. ([odooinstance](docs/odooinstance.md#resources-default))
- [ ] **Monitoring.** Scrape the operator, and alert on instances that are not ready and addons in `Failed`. ([production-guide](docs/production-guide.md#monitoring))
- [ ] **Backups.** The operator does not back up the database or the filestore. Set up backups and test a restore. ([production-guide](docs/production-guide.md#backups))

## Documentation

| Page | Read it when |
|---|---|
| [docs/README.md](docs/README.md) | You want the index with a one-line summary of each page. |
| [docs/installation.md](docs/installation.md) | You install, upgrade or remove the operator, or you need the RBAC and manager flags. |
| [docs/odooinstance.md](docs/odooinstance.md) | You write an OdooInstance, or need a field's default, validation or the generated objects. |
| [docs/odooaddon.md](docs/odooaddon.md) | You add code from Git: credentials, pinning, single or multi-module repos. |
| [docs/upgrades.md](docs/upgrades.md) | You run module upgrades, automatically or by hand. |
| [docs/architecture.md](docs/architecture.md) | You want to know how the controllers, Jobs and volumes fit together. |
| [docs/production-guide.md](docs/production-guide.md) | You prepare an instance for production, or plan a version upgrade. |
| [docs/operations.md](docs/operations.md) | Something is stuck or failed, and you need a runbook. |
| [docs/development.md](docs/development.md) | You change the operator: build, test, CI, images, releases. |
| [docs/audit-2026-10.md](docs/audit-2026-10.md) | You want the historical review that drove recent changes. |

## Contributing

Read [docs/development.md](docs/development.md) first. The short version:

```bash
make build
make test
make lint
make manifests generate      # after changing api/v1 or RBAC markers
make verify-generated        # must pass before you push
```

Commit the generated files with the change that caused them.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
