# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/) (pre-1.0: minor versions may change the API).

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
Odoo and PostgreSQL in CI.
