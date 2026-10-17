# Development

This page is for people who change the operator. It covers the repository layout, the tools and their versions, the `make` targets, running the operator from your checkout, the end-to-end test, the CI workflows, how images are tagged and signed, the release procedure, and how to add a field to the API.

## Contents

- [Repository layout](#repository-layout)
- [Tools and versions](#tools-and-versions)
- [Make targets](#make-targets)
- [Generated files](#generated-files)
- [Running the operator from your checkout](#running-the-operator-from-your-checkout)
- [End-to-end tests on kind](#end-to-end-tests-on-kind)
- [CI/CD workflows](#cicd-workflows)
- [Image tags and signature verification](#image-tags-and-signature-verification)
- [Cutting a release](#cutting-a-release)
- [Adding a field to the API](#adding-a-field-to-the-api)
- [Code conventions](#code-conventions)

## Repository layout

```text
api/v1/                    CRD types, webhook (odooaddon_webhook.go), generated deepcopy
cmd/main.go                manager entry point, flags, controller setup
internal/controller/      the three reconcilers and their pure helpers
  odooinstance_*.go         OdooInstance: workload, storage, config, hashes, upgrade gate
  odooaddon_*.go            OdooAddon: controller, state machine, sync script and Job
  odooupgrade_*.go          OdooUpgrade: controller, Job builder, queueing
  *_test.go                 unit, table-driven and envtest tests
config/                    kustomize sources
  crd/                       generated CRDs (config/crd/bases)
  rbac/                      generated manager-role and hand-written helper roles
  manager/                   manager Deployment and namespace
  default/                   the default deploy (CRDs, RBAC, manager, metrics)
  samples/                   example CRs
  webhook/                   optional admission webhook manifest, see installation.md
  prometheus/, network-policy/ optional monitoring and metrics NetworkPolicy
  scorecard/                 operator-sdk scorecard configuration
test/e2e/, test/utils/     end-to-end test on kind
hack/                      boilerplate header for generated code
docs/                      this documentation
.github/workflows/         CI (see CI/CD workflows)
Makefile, Dockerfile       build and image
.golangci.yml              lint configuration
PROJECT                    Kubebuilder project metadata
```

The Go module path is `github.com/odoo-k8s/odoo-k8-operator`. The GitHub repository and the published image are under `acarmisc`. Use the module path in imports.

## Tools and versions

| Tool | Version | Where it is set |
|---|---|---|
| Go | 1.26 (`go 1.26.0` in `go.mod`, `1.26.x` in CI) | `go.mod`, `.github/workflows/*.yml` |
| controller-runtime | v0.25.2 | `go.mod` |
| k8s.io/api, apimachinery, client-go | v0.37.1 | `go.mod` |
| envtest Kubernetes version | `1.37` (derived from `k8s.io/api`) | `Makefile` (`ENVTEST_K8S_VERSION`) |
| setup-envtest | release matching controller-runtime | `Makefile` (`ENVTEST_VERSION`) |
| controller-gen | v0.23.0 | `Makefile` (`CONTROLLER_TOOLS_VERSION`) |
| kustomize | v5.8.2 | `Makefile` (`KUSTOMIZE_VERSION`) |
| golangci-lint | v2.15.0 | `Makefile` (`GOLANGCI_LINT_VERSION`), `lint.yml` |
| operator-sdk | v1.42.0 | `Makefile` (`OPERATOR_SDK_VERSION`), used by `make bundle` only |
| kind | v0.29.0 | `test-e2e.yml` |
| kind node image | `kindest/node:v1.34.0` | `test-e2e.yml` |
| govulncheck | `@latest` | `test.yml` |
| Docker | any with buildx for multi-arch | `Makefile` (`CONTAINER_TOOL`) |

Tools that the Makefile needs (`controller-gen`, `kustomize`, `setup-envtest`, `golangci-lint`) are installed into `bin/` on first use. `kind`, `kubectl` and Docker are not installed by the Makefile.

The devcontainer image is `golang:1.24`, which is older than `go.mod`. Use a local Go 1.26 toolchain, or update the devcontainer before you rely on it.

## Make targets

Run `make help` for the full list. The targets you will use most:

| Target | What it does |
|---|---|
| `make build` | `manifests`, `generate`, `fmt`, `vet`, then builds `bin/manager`. |
| `make test` | `manifests`, `generate`, `fmt`, `vet`, then runs `go test` on all packages except `test/e2e` with envtest assets. Writes `cover.out`. |
| `make lint` | Runs golangci-lint. `make lint-fix` applies the automatic fixes. |
| `make manifests` | Regenerates CRDs and RBAC (`config/crd/bases`, `config/rbac/role.yaml`) and the webhook manifest. |
| `make generate` | Regenerates `zz_generated.deepcopy.go`. |
| `make verify-generated` | Runs `manifests` and `generate`, then fails if `git diff` or untracked files appear. Used by CI. |
| `make fmt`, `make vet` | `go fmt ./...`, `go vet ./...`. |
| `make run` | Runs the manager on your machine against the cluster in your kubeconfig. |
| `make install` | Applies `config/crd` to the cluster. |
| `make uninstall` | Deletes the CRDs. This deletes all custom resources. |
| `make deploy IMG=...` | Deploys `config/default` with the given image. |
| `make undeploy` | Removes the default deploy. |
| `make build-installer IMG=...` | Writes `dist/install.yaml` with CRDs and manager. |
| `make docker-build IMG=...`, `make docker-push IMG=...` | Build and push the manager image. |
| `make docker-buildx IMG=...` | Multi-platform build and push (linux/arm64, amd64, s390x, ppc64le). |
| `make setup-test-e2e` | Creates the kind cluster `odoo-k8-operator-test-e2e` if it does not exist. |
| `make test-e2e` | Runs the end-to-end test, then deletes the kind cluster. |
| `make cleanup-test-e2e` | Deletes the kind cluster. |
| `make bundle` | Generates an OLM bundle with operator-sdk. Not run in CI. |

`make test` downloads the envtest binaries for Kubernetes `1.37` on the first run, so the first run needs network access.

## Generated files

These files are generated. Commit the generated output with the change that caused it:

- `config/crd/bases/*.yaml` and `config/rbac/role.yaml` (from `make manifests`);
- `config/webhook/manifests.yaml` (from `make manifests`);
- `api/v1/zz_generated.deepcopy.go` (from `make generate`).

The rule is enforced by `make verify-generated`, which CI runs. The check fails if the generated output differs from what is committed. After any change to `api/v1` or to RBAC markers, run:

```bash
make manifests generate
make verify-generated
```

CI also checks `go.mod` and `go.sum`. Run `go mod tidy` and commit the result.

## Running the operator from your checkout

The operator runs on your machine and talks to the cluster in your kubeconfig. Your user needs cluster-admin rights, or equivalent, to create the CRDs and read Secrets.

```bash
make install                 # CRDs
make run                     # the manager, in the foreground
```

In another terminal, create resources as usual. The `make run` process uses the default flags: leader election is off, metrics are disabled and the health probe is on `:8081`. Stop it with Ctrl-C.

To test a different sync image without rebuilding, set the variable before `make run`:

```bash
OPERATOR_SYNC_IMAGE=alpine/git:2.47.2 make run
```

A sync Job created by the local process still runs in the cluster, so the cluster must be able to pull the image and the Job pods must be able to reach the Git server.

## End-to-end tests on kind

The end-to-end suite is in `test/e2e`. It has one spec: the manager pod runs. It does not create an OdooInstance, so it is a deployment smoke test, not a reconcile test.

Prerequisites: Docker, `kind` and `kubectl` on your `PATH`.

```bash
make test-e2e
```

What happens:

1. `setup-test-e2e` creates the kind cluster `odoo-k8-operator-test-e2e` if it does not exist.
2. The suite builds the image `example.com/odoo-k8-operator:v0.0.1` with `make docker-build` and loads it into kind.
3. It creates the namespace `odoo-k8-operator-system`, labels it with the `restricted` Pod Security level, and installs the CRDs.
4. It deploys the manager with that image and checks that the pod is running.
5. It undeploys, removes the CRDs and the namespace, and `cleanup-test-e2e` deletes the kind cluster.

To use an existing kind cluster, set `KIND_CLUSTER` to its name. `test/utils` reads that variable. To keep the cluster after a run, skip the `make test-e2e` target and run its steps by hand: `make setup-test-e2e`, then `KIND_CLUSTER=<name> go test ./test/e2e/ -v -ginkgo.v`, then inspect the cluster.

## CI/CD workflows

All workflows are in `.github/workflows/`. Each one has `workflow_dispatch`, so it can be started by hand.

| Workflow | Runs on | Jobs and steps |
|---|---|---|
| `lint.yml` | push to `main`, tags `v*`, all PRs | golangci-lint `v2.15.0`. |
| `test.yml` | push to `main`, tags `v*`, all PRs | `unit and envtest`: `make test`, `go mod tidy` check, `make verify-generated`, coverage upload (`cover.out`, 14 days). `govulncheck`: `govulncheck ./...` in a separate job. |
| `test-e2e.yml` | push to `main`, tags `v*`, PRs to `main` | kind `v0.29.0` with node `kindest/node:v1.34.0`, then `make test-e2e`. Uploads kind logs on failure. |
| `build.yml` | push to `main`, tags `v*`, PRs to `main` | Builds the multi-arch image (amd64 and arm64). On non-PR events only: scans the amd64 image with Trivy (fails on HIGH and CRITICAL fixable vulnerabilities), pushes to GHCR with SBOM and provenance, and signs the digest with cosign (keyless). |

Concurrency: runs on the same branch cancel each other, except on tags, which always finish.

Pull requests never log in to the registry, never push, never sign.

## Image tags and signature verification

Images are published to `ghcr.io/acarmisc/odoo-k8-operator`:

| Event | Tags |
|---|---|
| push to `main` | `main`, `sha-<short-sha>` |
| tag `vX.Y.Z` | `vX.Y.Z`, `X.Y`, `sha-<short-sha>`, `latest` |
| tag `vX.Y.Z-<pre>` | `vX.Y.Z-<pre>`, `X.Y`, `sha-<short-sha>` (no `latest`) |
| pull request | built, not pushed |

Images are signed keylessly with cosign using the GitHub Actions OIDC identity. The signature is over the digest, so every tag of that image is covered. To verify:

```bash
cosign verify ghcr.io/acarmisc/odoo-k8-operator:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/acarmisc/odoo-k8-operator/\.github/workflows/build\.yml@refs/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

For production, deploy by digest once you have verified it.

## Cutting a release

1. Make sure `main` is green, and that the generated files are up to date (`make verify-generated`).
2. Tag and push:

   ```bash
   git tag v0.1.0
   git push origin v0.1.0
   ```

3. The `build.yml` workflow publishes and signs the image. Tags are never cancelled by newer runs.
4. Verify the signature (see above), then deploy with `make deploy IMG=ghcr.io/acarmisc/odoo-k8-operator:v0.1.0`.

Pre-release tags such as `v0.2.0-rc1` are published without `latest`, so a default `make deploy` never picks them up.

## Adding a field to the API

1. Add the field to the type in `api/v1/*_types.go`, with its doc comment and kubebuilder markers, for example `// +kubebuilder:validation:Minimum=1`. The doc comment becomes the CRD description, so write it for users.
2. Regenerate: `make manifests generate`.
3. Use the field in the controller. Put pure logic in a helper that takes plain values, and cover it with a table-driven test.
4. Add or update the envtest or unit tests for the new behaviour.
5. Run `make test`, then `make lint`.
6. Update the docs: `docs/odooinstance.md` or `docs/odooaddon.md` for the field reference, and `docs/installation.md` if the RBAC changes.
7. Commit the generated files together with the code. `make verify-generated` must pass.

Validation belongs in the CRD when CEL can express it (for example `replicas` and the access modes). Use the webhook only for checks that CEL cannot express. Also check the same rule in the controller, because the webhook is off by default.

## Code conventions

These are the patterns the code already follows. Keep to them.

- **Pure helpers.** Decision logic is a plain function over plain inputs. Examples: `decideAddonSync` in `odooaddon_state.go`, `gateDecide` in `odooinstance_gate.go`, `upgradeDecideStart` in `odooupgrade_queue.go`, `buildSyncScript` and `buildSyncJob` in `odooaddon_sync.go`. Test them with table-driven tests.
- **Non-blocking reconcilers.** A reconcile inspects state once, acts, and returns with `RequeueAfter` or waits for a watch event. It never sleeps or polls inside `Reconcile`. Jobs are watched with `Owns`.
- **Idempotent creates.** Ignore `AlreadyExists` on `Create`. Use `CreateOrUpdate` for objects the operator fully owns.
- **Write status only on change.** `writeAddonStatus` and `upgradeWriteStatus` compare before writing. Writing an unchanged status wakes the reconciler again through the watch.
- **Owner references.** Use `SetControllerReference` for objects that must go with their parent. Use the finalizer only for work that cannot be done by garbage collection.
- **Validate before shell.** Values that reach the sync script go through `validatePathSegment`, `validateModuleName` or `validateGitRef` first. Quote the rest with `shQuote`.
- **Constants.** Names, labels, paths and default values are constants in the `*_constants.go` or `constants.go` files. Repeated strings are flagged by `goconst`.
- **Lint.** `.golangci.yml` enables `copyloopvar`, `dupl`, `errcheck`, `ginkgolinter`, `goconst`, `gocyclo`, `govet`, `ineffassign`, `lll`, `misspell`, `nakedret`, `prealloc`, `revive` (`comment-spacings`, `import-shadowing`), `staticcheck`, `unconvert`, `unparam` and `unused`, with `gofmt` and `goimports`. `lll` is off for `api/*`. `dupl` and `lll` are off for `internal/*`.
- **Errors in logs, not status.** Instance reconcile errors are logged and set `Failed`. Addon and upgrade messages go to `status.message`, because users read them with `kubectl`.

## See also

- [architecture.md](architecture.md): the reconcile flows the code implements.
- [installation.md](installation.md): the deploy the CI tests against.
- [audit-2026-10.md](audit-2026-10.md): the historical review that set many of these conventions.
