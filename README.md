# botbox

A black-box property-based and fault-injection harness for Kubernetes controllers.

**Status:** pre-alpha, milestone M0 (scaffold). The commands below are the intended interface and do not run yet. See [DESIGN.md §10](DESIGN.md#10-milestones) for the milestone plan.

## What botbox does

botbox tests an unmodified Kubernetes controller. It generates random but valid sequences of operations on a custom resource. It observes the controller only through the Kubernetes API, and it injects faults at the API boundary and via process restarts.
It checks generic invariants that need no per-controller configuration, plus optional per-controller properties. It shrinks any failing sequence to a minimal reproducer ([DESIGN.md §1](DESIGN.md#1-thesis)).

## Install

Planned; not usable yet.

```sh
go install github.com/rosenhouse/botbox/cmd/botbox@latest
```

botbox also needs an envtest control plane: install `setup-envtest` and use it to fetch `kube-apiserver` and `etcd` for the pinned Kubernetes version ([DESIGN.md §5.8](DESIGN.md#58-test-cluster)).

## Quickstart: cert-manager

This section will hold the exact script CI runs, embedded from `examples/cert-manager/quickstart.sh` behind an HTML comment of the form `<!-- embed: examples/cert-manager/quickstart.sh -->`. A unit test checks the embedded block against that file byte-for-byte. That keeps the quickstart from ever drifting from what CI runs (DESIGN.md §11). It arrives in M4.

cert-manager publishes no installable binary. The M4 example instead clones the pinned tag and runs `go build` ([DESIGN.md §8.1](DESIGN.md#81-targetyaml), [§10 M4](DESIGN.md#10-milestones)). The intended shape:

```sh
botbox run --target examples/cert-manager/target.yaml
```

## Your own controller

A target is declared in one YAML file. This is the cert-manager target from [DESIGN.md §8.1](DESIGN.md#81-targetyaml):

```yaml
# examples/cert-manager/target.yaml
name: cert-manager
version: v1.21.2                              # free text; printed in reports
crds:
  - crds/cert-manager.crds.yaml               # files or directories of CRD YAML
primary: cert-manager.io/v1/Certificate       # the resource CR ops act on
sample: certificate.yaml                      # a valid primary CR; generation mutates copies of it
fixtures:
  - issuer.yaml                               # applied to the run namespace before op 0
manages:
  - v1/Secret
  - cert-manager.io/v1/CertificateRequest
ready: >-                                     # CEL over metadata, spec, status; must yield bool
  status.conditions.exists(c, c.type == "Ready" && c.status == "True"
    && c.observedGeneration == metadata.generation)
generate:
  mutate:                                     # allowlist of paths; absent means every schema path
    - spec.dnsNames
    - spec.duration
    - spec.privateKey.algorithm
    - spec.privateKey.rotationPolicy
  overlay:                                    # per-path schema tightening
    spec.dnsNames: {minItems: 1, maxItems: 3}
    spec.duration: {enum: ["1h", "24h", "2160h"]}
launch:
  binary: bin/cert-manager-controller
  args:
    - --kubeconfig=$KUBECONFIG
    - --leader-elect=false
    - --enable-certificate-owner-ref=true
timeouts:                                     # optional; defaults in §6
  settle: 30s
  stable: 10s
  delete: 60s
```

`primary` is the one CRD your sequences act on. `fixtures` are objects botbox applies once, before op 0, that generation never mutates, such as the Issuer a Certificate needs.
`manages` lists the other kinds your controller owns, which drives attribution for the invariants. `ready` is a CEL expression over `metadata`, `spec` and `status` that must evaluate to a boolean.
`launch` says how to exec your controller binary, with `$KUBECONFIG` substituted for the proxy's address.

## Reading a report

Arrives in M6. A failing run writes `report.md` and `report.json` containing: the minimized failing sequence, the violated invariant or property with its evidence (a request-log excerpt and an object version timeline), the target and its version, the seed, and a one-line replay command.
The run directory also keeps `target.log`, `requests.jsonl` (the proxy log) and `objects.jsonl` (the Observer history), so a report can be re-examined without re-running ([DESIGN.md §5.7](DESIGN.md#57-report)).

## Running in CI

Planned. The shape below is the intended recipe for adopters once M4 and M6 land.

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- run: go install github.com/rosenhouse/botbox/cmd/botbox@latest
- run: setup-envtest use "$ENVTEST_K8S_VERSION" -p path  # pinned in the Makefile
- run: <build your controller into a binary>
- run: botbox run --target target.yaml
```

Every tier below `kind` needs only `proxy.golang.org`, `sum.golang.org`, `github.com` and GitHub's raw and release-asset hosts. No tier assumes a container registry ([DESIGN.md §11](DESIGN.md#11-repo-conventions)).

## Invariants

Six generic invariants apply to every target. See [DESIGN.md §6](DESIGN.md#6-generic-invariants) for exact statements, defaults and attribution rules.

| ID | Name | Checks |
|---|---|---|
| G1 | Bounded reconciliation | The API request rate reaches zero and stays there while the spec is unchanged |
| G2 | No churn | Managed objects and their resourceVersions stop changing once converged |
| G3 | Clean deletion | Deleting the CR removes everything it manages and clears its finalizers |
| G4 | Convergence | The target's `Ready` predicate holds and `observedGeneration` matches `generation` |
| G5 | Restart-stable | Restarting the target does not change converged state |
| G6 | No error loop | The target does not retry the same failing request more than a bounded number of times |

## Development

- `make setup` — install the Go module cache, `setup-envtest`, and the envtest control-plane binaries.
- `make test` — unit tests; no API server.
- `make test-envtest` — tests against a local envtest control plane; under 5 minutes in CI.
- `make fmt` / `make vet` — `gofmt` and `go vet`.

A Claude Code web session runs `make setup` automatically, through the `SessionStart` hook in `.claude/`, so envtest is ready without manual steps.

## Design and internals

- [DESIGN.md](DESIGN.md) — the governing design; code and docs must not contradict it.
- [docs/journal.md](docs/journal.md) — one entry per milestone: what the agents got right, what they got wrong, and what changed as a result.
- [docs/spikes/](docs/spikes/) — write-ups of the experiments behind design decisions.
- `docs/bug-matrix.md` — which invariant catches each seeded bug in the toy target; arrives in M3.

## License

Apache-2.0. See [LICENSE](LICENSE).
