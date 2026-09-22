# Spike: external-secrets as a black-box target under envtest

Date: 2026-09-22. It measures the fallback target `DESIGN.md` §12 names against the
contract §8 sets, the way the 2026-09-20 spike measured cert-manager.

## Question

Can an unmodified external-secrets controller run against a bare envtest API server, with
its `fake` provider and no webhook installed, and behave the way the harness expects:
converge, stay quiet, survive a crash, and clean up?

## Setup

| Component | Version | How obtained |
|---|---|---|
| external-secrets controller | v2.11.0 (`e8f12e1f1646e0ad47966458023ff10c9577f2b0`) | shallow `git clone --depth 1 --branch v2.11.0`, then `go build -tags fake` at the repository root |
| external-secrets CRDs | v2.11.0 | release asset `external-secrets.yaml` (2,007,547 bytes, sha256 `c427124642886d240af8f28d294714a168d920ef8e5d55db14600e00ed0cfb80`, 44 documents, 25 CRDs) |
| envtest control plane | Kubernetes 1.37.0 | `setup-envtest use 1.37.0 -p path` |
| Go | 1.26.6 | downloaded by `GOTOOLCHAIN=auto`; external-secrets' `go.mod` says `go 1.26.6` |

Controller flags: `--enable-leader-election=false --metrics-addr=127.0.0.1:0
--live-addr=127.0.0.1:0 --loglevel=debug`. There is no `--kubeconfig` flag; the controller
calls controller-runtime's `GetConfigOrDie`, which reads `$KUBECONFIG`. Leader election is
already off by default.

The driver program is `external-secrets-envtest/main.go` next to this file. It runs a
counting reverse proxy in front of the API server, hands the controller a kubeconfig that
points at it, and drives a `SecretStore` fixture using the `fake` provider plus an
`ExternalSecret` that reads from it: wait for Ready with `status.syncedResourceVersion`
naming the current generation; watch resourceVersions and proxy traffic for 30 s; change
the `remoteRef` key; rename the target Secret; SIGKILL and re-exec the controller, then
wait 15 s; delete the ExternalSecret.

## Results

Five runs. Each varies one field of the ExternalSecret from the spike's defaults, which
are `refreshPolicy: OnChange`, `refreshInterval: 1h0m0s` and the CRD's own
`creationPolicy: Owner` and `deletionPolicy: Retain`.

| Observation | defaults | `Periodic`, `10s` | `Periodic`, `0s` | `creationPolicy: Orphan` | `deletionPolicy: Delete` |
|---|---|---|---|---|---|
| envtest up | 4.4 s | 5.0 s | 4.4 s | 4.9 s | 4.1 s |
| ExternalSecret Ready after controller start | 2.29 s | 2.37 s | 2.37 s | 2.49 s | 2.36 s |
| resourceVersion changes over 30 s of a stable spec | 0 | 3 | 0 | 0 | 0 |
| writes the proxy saw in that window | 0 | 3 status `PUT`s | 0 | 0 | 0 |
| Ready again after a `remoteRef` change | 114 ms | 112 ms | never | 113 ms | 111 ms |
| Ready again after the target Secret was renamed | 218 ms | 213 ms | — | 114 ms | 114 ms |
| Secret ownerReferences | 1 | 1 | 1 | 0 | 1 |
| Every resourceVersion 15 s after SIGKILL + re-exec | unchanged | ExternalSecret moved | — | unchanged | unchanged |
| ExternalSecret gone after delete | 112 ms | 108 ms | — | 109 ms | 109 ms |
| Secrets left 10 s after the delete | 1 | 1 | — | 2 | 0 |

This spike runs no garbage collector, so the last row counts what the controller itself
left. Under `deletionPolicy: Retain` it leaves the target Secret, which still carries an
ownerReference to the deleted ExternalSecret, so botbox's collector (§5.8) removes it.
Under `Orphan` there is no ownerReference to resolve, and both the renamed Secret and the
one it replaced survive.

The ExternalSecret carries the finalizer
`externalsecrets.external-secrets.io/externalsecret-cleanup`, so the delete does not
complete until the controller patches it off. The Secret carries the labels
`reconcile.external-secrets.io/managed: "true"` and
`reconcile.external-secrets.io/created-by: <hash>`, and the annotation
`reconcile.external-secrets.io/data-hash: <hash>`. Under `Orphan` the `created-by` label
is absent too.

Everything the controller left in the run namespace was the target Secret and three
Events: two on the SecretStore (`StoreUnmaintained`, `Valid`) and one on the Secret
(`Created`).

Over the restart the controller wrote one status `PATCH` to the SecretStore fixture and
two Events. The SecretStore's resourceVersion did not move, so that patch changed no
content. Under `Periodic` the ExternalSecret's own status moved as well, because a refresh
fell inside the window.

## What this means for the design

- **It works.** external-secrets needs no webhook, no cert controller and no leader
  election to sync a Secret from the `fake` provider under envtest. The webhook and the
  cert controller are separate subcommands of the same binary, so running
  `external-secrets` with no subcommand already excludes them.
- **The CRDs need no surgery.** No CRD in the release asset carries a `conversion` stanza,
  and `v1beta1` is `served: false`, so nothing asks the API server to convert. The asset is
  the chart rendered with its defaults, and the chart's `crds.conversion.enabled` is one of
  them: its README says "Conversion is disabled by default as we stopped supporting
  v1alpha1". The asset's 25 CRD specs equal the repository's `deploy/crds/bundle.yaml`.
- **The asset is a whole install manifest.** Besides the 25 CRDs `external-secrets.yaml`
  holds 3 Deployments, 3 ServiceAccounts, 5 ClusterRoles, 2 ValidatingWebhookConfigurations
  and 6 other objects. envtest reads the file and keeps only the
  CustomResourceDefinitions, so pointing `crds` at it installs the CRDs and nothing else.
- **Readiness needs CEL, and a prefix match.** An ExternalSecret carries no
  `observedGeneration`, at the top level or inside a condition. `status.syncedResourceVersion`
  is `"<metadata.generation>-<hash of labels and annotations>"`, so its prefix is what proves
  the controller observed the current generation. The predicate is
  `has(status.syncedResourceVersion) && status.syncedResourceVersion.startsWith(string(metadata.generation) + "-") && has(status.conditions) && status.conditions.exists(c, c.type == "Ready" && c.status == "True")`.
  It drove the runs below, and a throwaway test of `pkg/target`'s `compileReady` held it
  against a `metadata.generation` of either an `int64` or a `float64`.
- **Quiescence needs `refreshPolicy: OnChange`.** The default policy refreshes on
  `refreshInterval`, and each refresh writes `status.refreshTime` on the ExternalSecret.
  `refreshInterval: 0s` stops the refreshes but also stops convergence: the controller
  then skips a spec change outright unless the change renames the target Secret, so
  readiness never catches up to the new generation. `OnChange` refreshes exactly when the
  generation or the labels or the annotations change, and writes nothing otherwise. It
  costs coverage: under `OnChange` nothing botbox does reaches the periodic refresh path.
- **Runs may overlap.** Every port the controller binds moves under a flag. With
  `--metrics-addr` and `--live-addr` on ephemeral ports it bound two ephemeral sockets and
  nothing else. Port 9443, the controller-runtime webhook server's default, stayed free:
  the root command registers no webhook with the manager. Two spike runs at once both
  passed. external-secrets does not carry cert-manager's D28 constraint.
- **The negative control is real.** With `spec.target.creationPolicy: Orphan` the Secret
  carries no ownerReference and no `created-by` label. The controller does not delete it,
  the GC emulation of §5.8 has nothing to resolve, and the Secret survives the
  ExternalSecret. A rename under `Orphan` leaves the old Secret behind too, so that run
  ended with two.
- **`deletionPolicy` is not a negative control.** `Retain` is the CRD's own default, and
  under it the controller leaves the Secret alone. The Secret still carries an
  ownerReference, so the GC emulation removes it and G3 passes.

## Gotchas for the implementer

- **The `fake` provider is behind a build tag.** `pkg/register/fake.go` is
  `//go:build fake || all_providers`. Without a tag the SecretStore never goes Ready:
  `failed to find registered store backend for type: fake`. Upstream's own Makefile builds
  with `all_providers`; `-tags fake` costs one extra package over the untagged build,
  where `all_providers` costs 690.
- **The main package is the repository root**, `github.com/external-secrets/external-secrets`,
  with 61 `replace` directives onto in-tree modules. `go install ...@v2.11.0` cannot reach
  it. Clone and build, as cert-manager needs (D10). Measured here: 2.1 s to clone, 147 s
  to build with an empty module and build cache, 9 s warm. The build fills a 2.2 GB module
  cache and a 1.6 GB build cache, and the binary is 133,310,064 bytes.
- **There is no `--kubeconfig` flag.** The controller reads `$KUBECONFIG`, which the Binary
  launcher already exports (§5.1). A `--kubeconfig=$KUBECONFIG` in `launch.args` is an
  unknown flag and the process exits.
- `spec.target` defaults to `creationPolicy: Owner` and `deletionPolicy: Retain`
  field by field, so a sample that names only `spec.target.name` gets both.
- The controller posts Events into the run namespace and leaves them there. `manages` must
  not name `v1/Event`, or G3 fails on every run.
- The SecretStore fixture is re-validated on every controller start and every
  `--store-requeue-interval` (default 5 m), which writes a no-op status patch and two
  Events. G1 counts those requests, so the interval belongs in `launch.args` set longer
  than a run.
- envtest takes 4.1 s to 5.0 s to install the 25 CRDs, against 3.2 s for cert-manager's
  six in the 2026-09-20 spike.

## Against botbox

A draft `target.yaml` ran under `botbox` itself, outside the repository, with the release
asset as its `crds` and the binary as `bin/external-secrets`. It declares `v1/Secret` as
its only managed kind, the predicate above as `ready`, the SecretStore as its one fixture,
and mutates `spec.target.name` and `spec.refreshInterval`.

| Invocation | Result |
|---|---|
| `botbox run --runs 5 --seed 23`, the seeds the cert-manager example fixes | every run passed, 184 s |
| `botbox run` on a create-and-delete and on an update-and-restart sequence | both passed, 82 s |
| `botbox run` on a create-and-delete sequence whose ExternalSecret sets `creationPolicy: Orphan` | exit 1 in 100 s: `G3 the v1/Secret example-secret was still there 1m0s after the CR was deleted, orphaned: it carries no ownerReference to the CR` |

The negative control is a sequence rather than a `--launch-arg`, because no flag makes the
controller orphan its Secret.

The generated runs are not vacuous. Re-running them with the `spec.target.name` overlay
widened to `^EXAMPLE-[A-Z]{3}$`, which the CRD's own pattern rejects, failed on seed 24:
`spec.target.name: Invalid value: "EXAMPLE-MZS"`. Seed 23 alone draws one op and does not
reach that path.

## Rerun

```sh
git clone --depth 1 --branch v2.11.0 https://github.com/external-secrets/external-secrets.git /tmp/eso
(cd /tmp/eso && GOTOOLCHAIN=auto go build -tags fake -o /tmp/external-secrets .)
curl -fsSL -o /tmp/external-secrets.yaml \
  https://github.com/external-secrets/external-secrets/releases/download/v2.11.0/external-secrets.yaml
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 -p path)"
cd docs/spikes/external-secrets-envtest
go run . /tmp/external-secrets.yaml /tmp/external-secrets
```

The bare command is the defaults column. The others are `-refresh-policy=Periodic
-refresh-interval=10s`, `-refresh-policy=Periodic -refresh-interval=0s`,
`-creation-policy=Orphan` and `-deletion-policy=Delete`.
