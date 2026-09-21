# botbox

A black-box property-based and fault-injection harness for Kubernetes controllers.

botbox exercises an unmodified controller and judges it only through the Kubernetes API.
It applies a sequence of operations to one custom resource, restarts the controller where
the sequence says to, and records every request the controller makes. It then checks six
generic invariants that need no per-controller configuration, plus properties a target
declares. If your controller talks to an API server, botbox can test it.

**Status:** M0–M4 are merged. `botbox run` and `botbox replay` execute hand-written sequences and report
the first violation. Generation and shrinking arrive in M5, faults and `report.md` in M6 ([DESIGN.md §10](DESIGN.md#10-milestones)).

## Install

Each invocation starts an envtest control plane, so botbox needs `kube-apiserver` and `etcd`
too. `setup-envtest` fetches them, and the pinned `--index` decides which build ([DESIGN.md §15, D18](DESIGN.md#15-decision-log)).

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
# builds what it needs, so a clean checkout is enough. Arguments go to botbox.
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
  --target examples/cert-manager/target.yaml "$@" \
  examples/cert-manager/sequences/*.json
```

The first invocation installs `setup-envtest`, downloads the control plane and builds cert-manager,
a few minutes in all; the two runs then take about 80 seconds together. Per sequence, botbox applies
the Issuer fixture to a fresh namespace and launches the controller behind the proxy. `issue.json`
creates a Certificate and deletes it, and `reissue.json` adds a `spec.dnsNames` entry and restarts the controller:

```
run 1: seed 20260920, sequence examples/cert-manager/sequences/issue.json
run 2: seed 20260920, sequence examples/cert-manager/sequences/reissue.json
every run passed.
```

### The negative control

A passing example proves little by itself, so the example also ships a configuration that
must fail. With `--enable-certificate-owner-ref=false`, which `--launch-arg` appends to
`launch.args`, cert-manager leaves the issued Secret behind, as upstream documents. The
target declares `v1/Secret` as managed, so G3 has to report it:

```
run 1: seed 20260920, sequence examples/cert-manager/sequences/issue.json
run 1: G3 the v1/Secret example-tls was still there 1m0s after the CR was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-20T23:16:05.423491732Z; 1 versions, the first v1/Secret example-tls
  the evidence is in botbox-out/20260920T231452Z-20260920/run-1
```

`make test-example` runs both and fails unless the first passes and the second fails on G3 naming that Secret.

## Your own controller

A target is one YAML file, here `examples/cert-manager/target.yaml` trimmed to the keys every target needs.

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

`properties`, `generate`, `timeouts` and `thresholds` are optional ([DESIGN.md §8.1](DESIGN.md#81-targetyaml)).

### Your own sequence

M5 generates sequences, so until then you write one by hand. Here is the whole `ops` array of
`examples/cert-manager/sequences/issue.json`, which the file wraps in a `seed` and a target name:

```json
"ops": [
  {"i": 0, "t": "create", "obj": {"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
    "metadata": {"name": "example"},
    "spec": {"secretName": "example-tls", "commonName": "example.test",
             "dnsNames": ["example.test"], "issuerRef": {"kind": "Issuer", "name": "selfsigned"}}}},
  {"i": 1, "t": "delete"}
]
```

The op types are `create`, `update`, `delete`, `recreate`, `settle`, `restart` and `deleteManaged`,
and a settle wait follows every op that changes the CR or a managed object ([DESIGN.md §7](DESIGN.md#7-sequence-format)).
`botbox run --target target.yaml sequences/*.json` then exercises your controller.

## Reading a report

A run that violates an invariant prints the ID, what it saw and where the evidence is,
then exits 1. The evidence is in `botbox-out/<timestamp>-<seed>/run-<n>/`:

- `sequence.json` — what was executed. `botbox replay` re-executes it.
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
- run: botbox run --target target.yaml sequences/*.json
```

The job needs no cluster and no container registry. Cache the control plane and the target,
as [.github/workflows/ci.yml](.github/workflows/ci.yml) does.

## Invariants

Six generic invariants apply to every target. [DESIGN.md §6](DESIGN.md#6-generic-invariants) states
them exactly, with their windows, thresholds and attribution rules.

| ID | Checks |
|---|---|
| G1 | Bounded reconciliation. The target's request rate falls to zero under an unchanged spec. |
| G2 | No churn. Once converged, the managed objects and their resourceVersions stop changing. |
| G3 | Clean deletion. Deleting the CR removes everything it manages and clears its finalizers. |
| G4 | Convergence. `ready` holds within `T_settle` of every spec change. |
| G5 | Restart-stable. Restarting the target does not change converged state. |
| G6 | No error loop. The target does not repeat one failing request more than `N_errloop` times. |

[docs/bug-matrix.md](docs/bug-matrix.md) shows which check catches each bug seeded into the toy
controller of [DESIGN.md §9](DESIGN.md#9-toy-target-widget), and CI regenerates it from real runs.

## Development

- `make setup` — install the envtest control plane. `make help` lists every target.
- `make test`, `make test-envtest`, `make test-example` — the three tiers CI runs on every PR.

A block after `<!-- embed: path -->` holds that file byte for byte, and `make test` enforces it.

## Design and internals

- [DESIGN.md](DESIGN.md) — the governing design. Code and docs must not contradict it.
- [docs/bug-matrix.md](docs/bug-matrix.md) — which check catches each seeded bug.
- [docs/journal.md](docs/journal.md) and [docs/spikes/](docs/spikes/) — the milestone journal, and
  the experiments behind the decisions in DESIGN.md §15.

## License

Apache-2.0. See [LICENSE](LICENSE).
