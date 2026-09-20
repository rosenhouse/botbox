# botbox

A black-box property-based and fault-injection harness for Kubernetes controllers.

botbox exercises an unmodified controller and judges it only through the Kubernetes API.
It applies a sequence of operations to one custom resource, restarts the controller where
the sequence says to, and records every request the controller makes. It then checks six
generic invariants that need no per-controller configuration, plus properties a target
declares. Nothing is linked into the controller: if it talks to an API server, botbox can
test it.

**Status:** M0–M3 are merged, and M4, the cert-manager example below, is this change.
`botbox run` and `botbox replay` execute hand-written sequences and report the first
violation. Generated sequences and shrinking arrive in M5; fault injection and
`report.md` arrive in M6 ([DESIGN.md §10](DESIGN.md#10-milestones)).

## Install

```sh
go install github.com/rosenhouse/botbox/cmd/botbox@latest
```

Each invocation starts an envtest control plane, so botbox also needs `kube-apiserver`
and `etcd`. `setup-envtest` fetches them, and `KUBEBUILDER_ASSETS` says where they landed:

```sh
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 -p path)"
```

`botbox run --kubeconfig` uses an existing cluster instead ([DESIGN.md §5.8](DESIGN.md#58-test-cluster)).

## Quickstart: cert-manager

`examples/cert-manager/` drives [cert-manager](https://github.com/cert-manager/cert-manager)
v1.21.2. botbox builds it from its own source at that tag, runs the binary unmodified
behind a recording proxy, and judges it only through the API. Nothing is patched into
cert-manager: the CRDs are its release asset and the flags are its own.

From a clean checkout, this script is the whole run.

<!-- embed: examples/cert-manager/quickstart.sh -->
```sh
#!/bin/sh
# Exercise cert-manager against the generic invariants of DESIGN.md §6. It
# builds what it needs, so a clean checkout is enough. Arguments go to botbox.
set -eu
cd "$(dirname "$0")/../.."

go build -o bin/botbox ./cmd/botbox
make --no-print-directory cert-manager

KUBEBUILDER_ASSETS="$(make --no-print-directory assets-path)" ./bin/botbox run \
  --target examples/cert-manager/target.yaml "$@" \
  examples/cert-manager/sequences/*.json
```

It clones the tag and builds `cmd/controller`, which costs about 90 seconds once. botbox
then starts one envtest control plane holding the CRDs, and for each sequence it applies
the Issuer fixture to a fresh namespace, launches the controller behind the proxy, and
executes the ops. `issue.json` creates a Certificate and deletes it. `reissue.json` adds a
name to `spec.dnsNames` and restarts the controller. After the build output, botbox prints:

```
run 1: seed 20260920, sequence examples/cert-manager/sequences/issue.json
run 2: seed 20260920, sequence examples/cert-manager/sequences/reissue.json
every run passed.
```

### The negative control

A passing example proves little by itself, so the example also ships a configuration that
must fail. With `--enable-certificate-owner-ref=false`, cert-manager leaves the issued
Secret behind when the Certificate is deleted, which upstream documents as intended. The
target declares `v1/Secret` as managed, so G3 has to report it. Passing
`--launch-arg --enable-certificate-owner-ref=false` to the script appends that flag to
`launch.args`, and the run ends:

```
run 1: seed 20260920, sequence examples/cert-manager/sequences/issue.json
run 1: G3 the v1/Secret example-tls was still there 1m0s after the CR was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-20T23:16:05.423491732Z; 1 versions, the first v1/Secret example-tls
  the evidence is in botbox-out/20260920T231452Z-20260920/run-1
```

`make test-example` runs both configurations and fails unless the first passes and the
second fails on G3 naming that Secret. CI runs it on every pull request.

## Your own controller

A target is one YAML file. Below is `examples/cert-manager/target.yaml`, the file the
quickstart runs, trimmed to the keys every target needs.

```yaml
name: cert-manager
version: v1.21.2
crds:
  - crds/cert-manager.crds.yaml               # files or directories of CRD YAML
primary: cert-manager.io/v1/Certificate       # the resource CR ops act on
sample: certificate.yaml                      # a valid primary CR; M5 generates from it
fixtures:
  - issuer.yaml                               # applied to the run namespace before op 0
manages:                                      # group/version/Kind, or v1/Kind for the core group
  - v1/Secret
  - cert-manager.io/v1/CertificateRequest
ready: >-                                     # CEL over metadata, spec, status; must yield bool
  has(status.conditions) && status.conditions.exists(c,
    c.type == "Ready" && c.status == "True"
    && has(c.observedGeneration) && c.observedGeneration == metadata.generation)
launch:
  binary: bin/cert-manager-controller
  args:
    - --kubeconfig=$KUBECONFIG                # replaced with a kubeconfig for the proxy
    - --leader-elect=false
    - --enable-certificate-owner-ref=true
    - --metrics-listen-address=127.0.0.1:0
```

You supply the CRDs your controller serves, one valid sample CR, any fixture that CR
depends on as a Certificate depends on an Issuer, the kinds your controller creates so
that botbox can tell your objects from its own, an expression that says when
reconciliation has finished, and a command line that launches your binary. `properties`,
`generate`, `timeouts` and `thresholds` are optional ([DESIGN.md §8.1](DESIGN.md#81-targetyaml)).
`botbox run --target target.yaml sequences/*.json` then exercises it.

## Reading a report

A run that violates an invariant prints the ID, what it saw and where the evidence is,
then exits 1. The evidence is in `botbox-out/<timestamp>-<seed>/run-<n>/`:

- `sequence.json` — what was executed. `botbox replay` re-executes it.
- `requests.jsonl` — every request the target made, as the proxy saw it.
- `objects.jsonl` — every version of every object the Observer saw.
- `target.log` — the target's own output.

Passing runs are not kept. `report.md` and `report.json`, with the minimized sequence and
its evidence inline, arrive in M6 ([DESIGN.md §5.7](DESIGN.md#57-report)).

## Running in CI

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- run: go install github.com/rosenhouse/botbox/cmd/botbox@latest
- run: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1
- run: echo "KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path)" >>"$GITHUB_ENV"
- run: go build -o bin/controller ./cmd/controller   # whatever launch.binary names
- run: botbox run --target target.yaml sequences/*.json
```

The job needs no cluster and no container registry: envtest is a local API server and
etcd, and the target is a binary you built. Cache both, as
[.github/workflows/ci.yml](.github/workflows/ci.yml) does.

## Invariants

Six generic invariants apply to every target. [DESIGN.md §6](DESIGN.md#6-generic-invariants)
states them exactly, with their windows, thresholds and attribution rules.

| ID | Checks |
|---|---|
| G1 | Bounded reconciliation. The target's request rate falls to zero under an unchanged spec. |
| G2 | No churn. Once converged, the managed objects and their resourceVersions stop changing. |
| G3 | Clean deletion. Deleting the CR removes everything it manages and clears its finalizers. |
| G4 | Convergence. `ready` holds within `T_settle` of every spec change. |
| G5 | Restart-stable. Restarting the target does not change converged state. |
| G6 | No error loop. The target does not repeat one failing request more than `N_errloop` times. |

[docs/bug-matrix.md](docs/bug-matrix.md) shows which check catches each bug seeded into the
toy controller of [DESIGN.md §9](DESIGN.md#9-toy-target-widget). CI regenerates it from
real runs on every pull request and fails if the committed copy is stale. Its B0 row is
the control, the toy with no bug, and is empty on purpose.

## Development

- `make setup` — download modules and install the envtest control plane.
- `make test` — the unit tier. No API server.
- `make test-envtest` — the envtest tier.
- `make test-example` — the cert-manager example and its negative control.
- `make bug-matrix` — regenerate `docs/bug-matrix.md`.
- `make fmt` and `make vet` — `gofmt` and `go vet`.

A Claude Code web session runs `make setup` through the `SessionStart` hook in `.claude/`,
so envtest is ready without manual steps.

A `<!-- embed: path -->` comment before a fenced block means the block holds that file
byte for byte, and `make test` enforces it ([DESIGN.md §11](DESIGN.md#11-repo-conventions)).

## Design and internals

- [DESIGN.md](DESIGN.md) — the governing design. Code and docs must not contradict it.
- [docs/bug-matrix.md](docs/bug-matrix.md) — which check catches each seeded bug.
- [docs/journal.md](docs/journal.md) — what the agents got right and wrong, per milestone.
- [docs/spikes/](docs/spikes/) — the experiments behind the decisions in DESIGN.md §15.

## License

Apache-2.0. See [LICENSE](LICENSE).
