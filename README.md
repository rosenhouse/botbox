# botbox

A black-box property-based and fault-injection harness for Kubernetes controllers.

botbox exercises an unmodified controller and judges it only through the Kubernetes API. It draws
sequences of operations on one custom resource from that resource's own CRD schema, applies them,
restarts the controller where a sequence says to, and records every request the controller makes.
It then checks six generic invariants that need no per-controller configuration, plus properties a
target declares. If your controller talks to an API server, botbox can test it.

**Status:** M0–M5 are merged. `botbox run` draws sequences, runs them, and minimizes the first
failure; `botbox replay` re-executes one. Faults and `report.md` arrive in M6 ([DESIGN.md §10](DESIGN.md#10-milestones)).

## Install

An invocation starts an envtest control plane, so botbox needs `kube-apiserver` and `etcd` too. `setup-envtest` fetches them, and the pinned `--index` decides which build ([DESIGN.md §15, D18](DESIGN.md#15-decision-log)).

```sh
go install github.com/rosenhouse/botbox/cmd/botbox@latest
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1
index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 --index $index -p path)"
```

`botbox run --kubeconfig` uses an existing cluster instead ([DESIGN.md §5.8](DESIGN.md#58-test-cluster)).

## Quickstart: cert-manager

`examples/cert-manager/` drives [cert-manager](https://github.com/cert-manager/cert-manager)
v1.21.2, built from its own source at that tag and run unmodified. Nothing is patched into
it: the CRDs are its release asset, pinned by sha256, and the flags are its own. From a
clean checkout, this script is the whole run.

<!-- embed: examples/cert-manager/quickstart.sh -->
```sh
#!/bin/sh
# Exercise cert-manager against the generic invariants of DESIGN.md §6. It
# builds what it needs, so a clean checkout is enough. Arguments go to botbox:
# --seed picks the sequences it draws, and a later --runs wins over the one here.
set -eu
cd "$(dirname "$0")/../.."

# cert-manager's healthz port is fixed (DESIGN.md §15, D28), so runs collide.
if ! command -v lsof >/dev/null; then
  echo "lsof is missing, so nothing checked whether port 9403 is free." >&2
elif lsof -nP -iTCP:9403 -sTCP:LISTEN >/dev/null; then
  echo "port 9403 is bound. cert-manager listens there, so its runs cannot overlap." >&2
  exit 1
fi

go build -o bin/botbox ./cmd/botbox
make --no-print-directory cert-manager

KUBEBUILDER_ASSETS="$(make --no-print-directory assets-path)" ./bin/botbox run \
  --target examples/cert-manager/target.yaml --runs 5 "$@"
```

The first invocation installs `setup-envtest`, downloads the control plane and builds
cert-manager, a few minutes in all; the five runs then take about three minutes together. Each
applies the Issuer fixture to a fresh namespace, launches the controller behind the proxy, and
executes one drawn sequence. With no `--seed` botbox draws one and prints it. Fixing it draws
the same five sequences every time:

```sh
examples/cert-manager/quickstart.sh --seed 23
```

```
run 1: seed 23, generated
run 2: seed 24, generated
run 3: seed 25, generated
run 4: seed 26, generated
run 5: seed 27, generated
every run passed.
```

### The negative control

A passing example proves little by itself, so the example also ships a configuration that must
fail. With `--enable-certificate-owner-ref=false`, which `--launch-arg` appends to `launch.args`,
cert-manager leaves the issued Secret behind, as upstream documents. The target declares
`v1/Secret` as managed, so G3 has to report it:

```sh
examples/cert-manager/quickstart.sh --seed 23 --runs 1 --deadline 5m --launch-arg --enable-certificate-owner-ref=false
```

```
run 1: seed 23, generated
run 1: G3 the v1/Secret example-tls was still there 1m0s after the CR was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-21T05:59:08.980624165Z; 1 versions, the first v1/Secret example-tls
  the evidence is in botbox-out/20260921T055744Z-23/run-1
  the sequence is 1 op, in botbox-out/20260921T055744Z-23/run-1/sequence.json
```

Seed 23 draws a single op, so there is nothing to minimize. A longer sequence is cut to the ops
the failure needs before it is reported, which costs a replay each: give `--deadline` room for
that. `make test-example` runs this same control, and fails unless the default configuration
passes and the control fails on G3 naming that Secret. A nightly workflow draws its own seeds.

## Your own controller

A target is one YAML file, here `examples/cert-manager/target.yaml` trimmed. Four keys are
required: `name`, `primary`, `sample` and `launch.binary`. Everything else is optional. A target
that declares no `ready` is judged by `has(status.observedGeneration) && status.observedGeneration
== metadata.generation`, so declare one if your CR does not carry `observedGeneration`
([DESIGN.md §8.1](DESIGN.md#81-targetyaml)).

```yaml
name: cert-manager
crds:
  - crds/cert-manager.crds.yaml               # files or directories of CRD YAML
primary: cert-manager.io/v1/Certificate       # the resource CR ops act on
sample: certificate.yaml                      # a valid primary CR; generation mutates copies of it
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
  binary: bin/cert-manager-controller       # relative to the working directory, not to this file
  args:
    - --kubeconfig=$KUBECONFIG                # replaced with a kubeconfig for the proxy
    - --enable-certificate-owner-ref=true
```

Every sequence starts by creating your `sample`, then draws from `update`, `delete`, `recreate`,
`settle`, `restart` and `deleteManaged`, which deletes one managed object behind the
controller's back. Field values come from the CRD's own schema: its numeric ranges, enums,
patterns and list lengths. A schema that says only `type: string` yields a random word, so the
schema is not a safety net. Where it allows more than your controller does, `generate.mutate`
lists the only paths a sequence changes and `generate.overlay` tightens one path's schema, as
`examples/cert-manager/target.yaml` does. Naming a path botbox cannot draw from is a
configuration error, not a silent skip ([DESIGN.md §8.3](DESIGN.md#83-generation-constraints-and-admission-webhooks)).

A sequence file runs as written and is never minimized. This is
`examples/cert-manager/sequences/issue.json`, reflowed ([DESIGN.md §7](DESIGN.md#7-sequence-format)):

```json
{"seed": 20260920, "target": "cert-manager", "ops": [
  {"i": 0, "t": "create", "obj": {"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
    "metadata": {"name": "example"}, "spec": {"secretName": "example-tls",
    "commonName": "example.test", "dnsNames": ["example.test"],
    "issuerRef": {"kind": "Issuer", "name": "selfsigned"}}}},
  {"i": 1, "t": "delete"}]}
```

`botbox replay --target target.yaml sequence.json` re-executes one, which is how you re-examine
a failure, and `make test-example` runs both pinned sequences so they cannot rot.

## Reading a report

A run that violates an invariant prints the ID, what it saw and where the evidence is, then
exits 1. A configuration or harness error exits 2, so your CI can tell a find from a broken
target. The evidence is in `botbox-out/<timestamp>-<seed>/run-<n>/`:

- `sequence.json` — the sequence the rest of the directory is evidence of.
- `sequence.shrunk.json` — a smaller sequence the deadline left unrun. Present only then.
- `requests.jsonl` — every request the target made, as the proxy saw it.
- `objects.jsonl` — every version of every object the Observer saw.
- `target.log` — the target's own output.

Passing runs are not kept. `report.md` and `report.json` arrive in M6 ([DESIGN.md §5.7](DESIGN.md#57-report)).

## Running in CI

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- run: go install github.com/rosenhouse/botbox/cmd/botbox@latest
- run: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1
- run: |
    index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
    echo "KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 --index $index -p path)" >>"$GITHUB_ENV"
- run: go build -o bin/controller ./cmd/controller   # whatever launch.binary names
- run: botbox run --target target.yaml --seed 23 --runs 5 --deadline 10m
```

`$GITHUB_ENV` is what carries `KUBEBUILDER_ASSETS` between steps; an `export` does not. Give
`--deadline` room for your controller, because a run that overruns it exits 2 rather than
reporting a find. Cache the control plane and the target as
[.github/workflows/ci.yml](.github/workflows/ci.yml) does. Fix the seed on pull requests, so that
a failure is the change under review and not a new draw, and draw fresh seeds on a schedule, as
[nightly.yml](.github/workflows/nightly.yml) does. The job needs no cluster and no registry.

## Invariants

Six generic invariants apply to every target. [DESIGN.md §6](DESIGN.md#6-generic-invariants) states them exactly, with their windows, thresholds and attribution rules.

| ID | Checks |
|---|---|
| G1 | Bounded reconciliation. The target's request rate falls to zero under an unchanged spec. |
| G2 | No churn. Once converged, the managed objects and their resourceVersions stop changing. |
| G3 | Clean deletion. Deleting the CR removes everything it manages and clears its finalizers. |
| G4 | Convergence. `ready` holds within `T_settle` of every spec change. |
| G5 | Restart-stable. Restarting the target does not change converged state. |
| G6 | No error loop. The target does not repeat one failing request more than `N_errloop` times. |

[docs/bug-matrix.md](docs/bug-matrix.md) shows which check catches each bug seeded into the toy controller of [DESIGN.md §9](DESIGN.md#9-toy-target-widget), and CI regenerates it from real runs.

## Development and internals

- `make setup` installs the envtest control plane, and `make help` lists every target.
- `make test`, `make test-envtest` and `make test-example` are the three tiers CI runs on every PR.
- A block after `<!-- embed: path -->` holds that file byte for byte, and `make test` enforces it.
- [DESIGN.md](DESIGN.md) is the governing design. Code and docs must not contradict it.
- [docs/journal.md](docs/journal.md) and [docs/spikes/](docs/spikes/) hold the milestone journal and the experiments behind DESIGN.md §15.

## License

Apache-2.0. See [LICENSE](LICENSE).
