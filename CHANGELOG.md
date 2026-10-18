# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/) (pre-1.0: minor versions may change the API).

## [0.1.1] - 2026-10-18

**Upgrade from 0.1.0 as soon as possible if you set `spec.adminPasswordSecret`:** in 0.1.0 the web pods crashed
on Odoo 19 (see "Fixed"). Web pods roll once on this upgrade when `adminPasswordSecret` is set and, with
`ReadWriteOnce` volumes, because the pod affinity changed.

This release is the first one verified end to end with real Odoo 19 and PostgreSQL 17 on kind: database
initialisation, admin login, dedicated cron pod, addon sync and install, upgrade gate, backup to a PVC and
the cleanup paths all run in CI now (`test/e2e/e2e_stack_test.go`).

### Added
- End-to-end test of the full stack on kind: PostgreSQL with TLS, database initialisation, login checks, dedicated cron pod, addon sync and install, upgrade gate, PVC backup and cleanup paths (`test/e2e/e2e_stack_test.go`). Diagnostics are dumped on failure.

### Fixed
- Rollouts with `ReadWriteOnce` volumes could stall on a Multi-Attach error. The cron pod was pinned to a web
  pod's node, but the web pods had no affinity to the cron pod, so a web rollout could land on another node
  while the cron pod still held the volume. The web and cron pod templates now share one required pod affinity
  on `instance: <name>`, so they always run on the same node. The web pods roll once when you upgrade. The
  web Deployment selector is unchanged.
- OdooAddon with `singleAddon: true` fails with `no __manifest__.py at the repository root of <moduleName>` when
  the repository has no `__manifest__.py` at its root. Before, the sync published the repository as a module that
  Odoo could not load. The check runs before any change to the addons volume.
- OdooInstance with `spec.adminPasswordSecret` crashed on Odoo 19 with `no such option: --admin-passwd`. Odoo has no
  command line flag for the master password, only the `admin_passwd` config key. An init container (`render-config`)
  now writes the key into an in-memory config (`medium: Memory`, 1Mi) that the web container reads. The password is
  never in the ConfigMap or on the command line, and only the init container receives it. The web pods roll once
  when you upgrade.
- Install Jobs passed `--without-demo=all`, which Odoo 19 logs as an invalid boolean. They now pass
  `--without-demo=True`, which means no demo data on Odoo 16 to 19.
- An update conflict (HTTP 409, "the object has been modified") on an OdooInstance was reported as a failure: a
  `ReconcileFailed` event, the `Degraded` condition and an error log. A conflict is normal and self-heals, so the
  reconcile now requeues after one second without an error, condition or event.

## [0.1.0] - 2026-10-17

First release after a production-readiness review (see [docs/audit-2026-10.md](docs/audit-2026-10.md)).
**Upgrading from 0.0.x:** re-apply the CRDs (`make install` / `kubectl apply -k config/crd`), then the operator.
The operator now watches only the namespaces in `WATCH_NAMESPACES` (default: its own); see
[docs/installation.md](docs/installation.md#namespaces-and-rbac). Addons pinned to a commit and synced by
0.0.x are synced once more so they report their modules.

### Added
- `OdooUpgrade` CRD: one-shot `odoo -u/-i --stop-after-init` Jobs, serialised per instance.
- `OdooInstance.spec.upgrade`: addon revisions roll out only after a successful `OdooUpgrade`.
- `OdooInstance.spec.initialize`: first-time database initialisation (modules, no demo data by default,
  admin user password from a Secret) before any Odoo pod starts.
- `OdooBackup` CRD: PostgreSQL (`pg_dump`) and filestore backups to a PVC or S3-compatible storage,
  one-shot or scheduled (CronJob), with retention.
- `OdooInstance.spec.cron`: dedicated cron Deployment, web pods run without cron workers.
- Status `conditions` and Kubernetes Events on all CRDs; `status.message` on `OdooInstance`.
- Private Git repositories (`OdooAddon.spec.gitSecretRef`: HTTPS token or SSH key) and commit-SHA pinning.
- `OdooInstance.spec.adminPasswordSecret`, `dataVolume.retainOnDelete`, `accessMode` for both volumes.
- Pod-template hashes: config, referenced Secrets and addon revisions roll the pods automatically.
- Probes (`/web/health`), port 8072, PodDisruptionBudget, rollout strategy matching the storage, hardened pod spec.
- Documentation: README plus `docs/` (installation, architecture, API references, upgrades, backups,
  production guide, operations, development).
- CI: lint, tests with envtest, `govulncheck`, e2e on kind, signed multi-arch image build with Trivy and SBOM.
- `LICENSE` (Apache 2.0).

### Changed
- OdooAddon sync is a non-blocking state machine over a hardened Job; several addons sync in parallel
  (`--addon-max-concurrent-reconciles`, default 4).
- Addon updates are atomic (release directories + symlink swap); modules removed upstream are cleaned up,
  module-name conflicts between addons are detected, and deleting an OdooAddon cleans its files.
- `status.addonPaths` lists the real module paths of multi-module repositories.
- The data PVC is no longer garbage-collected with the `OdooInstance` (see `retainOnDelete`).
- Dependencies: Go 1.26, controller-runtime 0.25, Kubernetes libraries 0.37; distroless runtime image.
- `postgres.port` defaults to 5432; `proxy_mode = True` and `list_db = False` are rendered by default.

### Fixed
- The committed OdooAddon CRD was missing `spec.moduleName`.
- Default RBAC (RoleBinding in the operator namespace) did not match the cluster-wide cache.
- Sample manifests used the wrong `apiVersion` and an empty spec.

### Known limitations
See the "Known limitations" section of [docs/production-guide.md](docs/production-guide.md).
Initialisation, backup and cron flows are covered by envtest only; they have not been run against a real
Odoo and PostgreSQL in CI. With `ReadWriteOnce` volumes the cron pod is pinned to a web pod's node at
scheduling time; a later web rollout onto another node can wait on a Multi-Attach error (use
`ReadWriteMany`). There is no automated restore, Ingress or HPA.
