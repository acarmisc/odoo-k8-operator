# Documentation

This is the documentation for the Odoo Kubernetes Operator. Start with the [main README](../README.md) for an overview and a quick start. The pages below go deeper. Each page starts with a summary, and the long ones have a contents list.

## Pages

| Page | What it covers |
|---|---|
| [installation.md](installation.md) | Prerequisites, installing the CRDs and the operator, the namespace and RBAC model, manager flags, metrics, upgrading and uninstalling the operator. |
| [odooinstance.md](odooinstance.md) | Reference for `OdooInstance`: every field with its default and validation, the generated `odoo.conf`, status and phases, the generated objects, probes, volumes and security context. |
| [odooaddon.md](odooaddon.md) | Reference for `OdooAddon`: Git URLs and credentials, branch, tag or SHA pinning, single and multi-module repositories, the sync lifecycle and phases. |
| [upgrades.md](upgrades.md) | `OdooUpgrade` in detail, manual upgrades, automatic upgrades with `spec.upgrade` and the rollout gate, retries and limitations. |
| [backups.md](backups.md) | `OdooBackup`: database and filestore backups to a PVC or S3, scheduled backups, retention, the restore runbook and limits. |
| [architecture.md](architecture.md) | The controllers and the Jobs they create, the reconcile flows, the addons volume layout, how pods are rolled, and the upgrade gate, with diagrams. |
| [production-guide.md](production-guide.md) | Storage, secrets, database, scaling, resources, security, monitoring, backups, multi-instance layouts, a version-upgrade runbook and known limitations. |
| [operations.md](operations.md) | Day-2 runbooks with `kubectl`: reading state, logs, stuck or failed addons and upgrades, forcing a re-sync, and a table of common error messages. |
| [development.md](development.md) | Repository layout, tools and versions, `make` targets, generated files, running locally, the kind end-to-end test, CI, image tags, releases and conventions. |
| [audit-2026-10.md](audit-2026-10.md) | The historical review of the operator (in Italian), with the implementation status of its recommendations. |

## Suggested reading order

1. [installation.md](installation.md), to get the operator running.
2. [odooinstance.md](odooinstance.md) and [odooaddon.md](odooaddon.md), to describe your Odoo deployment.
3. [upgrades.md](upgrades.md), before you change code on a live instance.
4. [production-guide.md](production-guide.md), before go-live.
5. [operations.md](operations.md), to keep at hand.
