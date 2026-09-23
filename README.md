# botbox

A black-box property-based and fault-injection harness for Kubernetes controllers.

botbox exercises an unmodified controller and judges it only through the Kubernetes API. It draws
sequences of operations on one custom resource from that resource's own CRD schema, applies them,
restarts the controller where a sequence says to, and records every request the controller makes.
It then checks six generic invariants that need no per-controller configuration, plus properties a
target declares. If your controller talks to an API server, botbox can test it.

**Status:** M0–M7 are merged. `botbox run` draws sequences, runs them, and minimizes the first
failure; `botbox replay` re-executes one. A failing run writes a report naming what broke and
how to see it again ([DESIGN.md §5.7](DESIGN.md#57-report)).

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
  at 2026-09-21T05:59:08.980624165Z; 1 version, the first v1/Secret example-tls
  the evidence is in botbox-out/20260921T055744Z-23/run-1
  the sequence is 1 op, in botbox-out/20260921T055744Z-23/run-1/sequence.json
```

Seed 23 draws a single op, so there is nothing to minimize. A longer sequence is cut to the ops
the failure needs before it is reported, which costs a replay each: give `--deadline` room for
that. `make test-example` runs this same control, and fails unless the default configuration
passes and the control fails on G3 naming that Secret. A nightly workflow draws its own seeds.

## A second example: external-secrets

`examples/external-secrets/` drives [external-secrets](https://github.com/external-secrets/external-secrets)
v2.11.0, pinned and built from its own source the same way.

```sh
examples/external-secrets/quickstart.sh --seed 23
```

It shows three things cert-manager does not.

- Every port this controller binds is ephemeral, so two runs may overlap and the quickstart
  needs no port guard.
- An ExternalSecret carries no `observedGeneration`, so `ready` proves the controller saw this
  generation from a version string: `status.syncedResourceVersion` is `"<generation>-<hash>"`,
  and the predicate matches its prefix.
- The negative control is a sequence rather than a flag. No flag makes the controller orphan
  the Secret it manages. `spec.target.creationPolicy: Orphan` in the CR does.

`make test-example-external-secrets` runs the drawn sequences, then the pinned ones, then
that control, and fails unless the control reports G3:

```
run 1: seed 20260922, sequence examples/external-secrets/sequences/orphan.json
run 1: G3 the v1/Secret example-secret was still there 1m0s after the CR was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-22T16:43:05.836050746Z; 1 version, the first v1/Secret example-secret
  the evidence is in botbox-out/20260922T164150Z-20260922/run-1
```

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
timeouts:                                     # optional; 30s, 10s and 60s by default
  settle: 30s                                 # the whole budget for one spec change
  stable: 10s                                 # the quiet it has to end in, carved out of settle
  delete: 60s                                 # how long a deletion has to come clean
```

A slow controller needs a wider `settle`, and a chatty one a narrower `stable`. The quiet
window sits inside the settle budget, so the controller has `settle - stable` to stop
writing. A `stable` at least as wide as `settle` leaves it none, so botbox refuses to load
that target rather than reporting G4 against your controller.

envtest runs no garbage collector, so botbox runs its own over the kinds your target
declares. It deletes an object once every owner the object names is gone. It finds an owner
by group, kind and name, at any version the API server serves, and then compares the UID.
It counts as live an owner of a kind your target does not declare, or one named at a version
the API server does not serve, so it never deletes an object that names one. The run prints
a note for each such object and owner, and the report carries it. A real garbage collector
cannot resolve an unserved version either, so fix that reference in your controller. If your
controller creates an owner of an undeclared kind, add the kind to `manages`. Otherwise,
point `botbox run --kubeconfig` at a cluster such as kind, whose garbage collector resolves
every kind.

Every sequence starts by creating your `sample`, then draws from `update`, `delete`, `recreate`,
`settle`, `restart` and `deleteManaged`, which deletes one managed object behind the
controller's back. A sequence you write yourself can also carry a `fault`, which makes the
proxy refuse, delay or drop the requests it matches. This is
`targets/toy-widget/sequences/fault.json`:

<!-- embed: targets/toy-widget/sequences/fault.json -->
```json
{"seed": 20260920, "target": "toy-widget", "ops": [
  {"i": 0, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget",
    "metadata": {"name": "widget"}, "spec": {"count": 1}}},
  {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps"},
    "action": {"error": 500}, "until": {"count": 30}}},
  {"i": 2, "t": "update", "patch": {"spec": {"count": 3}}}]}
```

An invariant ignores any window the proxy applied a fault in, so what a fault tests is how
the controller behaves once the fault stops. A controller backs off while its requests fail,
so once the faults stop botbox gives it as long as they lasted, plus `settle`, to converge.
That includes a fault still active when the sequence ends, like the one above: botbox clears
it and waits for the controller before it tears the run down. A fault that matches no request
changes nothing and hides nothing ([DESIGN.md §5.2](DESIGN.md#52-proxy)).

Field values come from the CRD's own schema: its numeric ranges, enums, patterns and list
lengths. A schema that says only `type: string` yields a random word, so the schema is not a
safety net. Where it allows more than your controller does, `generate.mutate`
lists the only paths a sequence changes and `generate.overlay` tightens one path's schema, as
`examples/cert-manager/target.yaml` does. Naming a path botbox cannot draw from is a
configuration error, not a silent skip ([DESIGN.md §8.3](DESIGN.md#83-generation-constraints-and-admission-webhooks)).

G5 compares what your controller manages before and after a restart. It already skips what
every restart moves, such as `metadata.resourceVersion`. If your controller stamps a field of
its own at startup, name it in `equalIgnore`. Quote a key that holds a dot or a slash, and write
`[*]` for every item of a list:

```yaml
equalIgnore:
  - metadata.annotations["example.com/started-at"]
  - status.conditions[*].lastHeartbeatTime
```

Keep the list in block style, because YAML claims the brackets inside a one-line `[...]` list.
botbox refuses a list index such as `[0]`, and a label or annotation key that the dots split,
when it loads the target ([DESIGN.md §8.1](DESIGN.md#81-targetyaml)). A key names nothing
inside a list, so a run notes a path such as `status.conditions.lastHeartbeatTime` and says
where the `[*]` goes.

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

In a sequence you write, put a `settle` op after a `restart`, and one before it unless the op
before it settles. G5 compares the states the controller settled in on either side, and leaves a
note instead of a verdict when another op changed something in between.

## Reading a report

A run that violates an invariant prints the ID, what it saw and where the evidence is, then
exits 1. A configuration or harness error exits 2, so your CI can tell a find from a broken
target. The evidence is in `botbox-out/<timestamp>-<seed>/run-<n>/`:

- `report.md` — what failed, the command that reproduces it, the sequence and the evidence.
- `report.json` — the same, for a machine.
- `sequence.json` — the sequence the rest of the directory is evidence of.
- `sequence.shrunk.json` — a smaller sequence the deadline left unrun. Present only then.
- `requests.jsonl` — every request the target made, as the proxy saw it.
- `objects.jsonl` — every version of every object the Observer saw.
- `target.log` — the target's own output.
- `kubeconfig` — what the target was pointed at, which is the proxy and not the cluster.

The report quotes the last twenty requests and the last twenty object versions the check
chose from, says how many that was, and names the file holding the rest. A G4 or a
property also quotes the state of the objects your controller managed where it failed,
over the kinds your target declares: a second table with its own bound of twenty and
the count beside it. Passing runs are not kept ([DESIGN.md §5.7](DESIGN.md#57-report)).

A G4 also quotes your `ready`, the error evaluating it, and your CR's status where it
failed. The status holds whatever your controller wrote, so the report cuts it: twenty
conditions, 200 bytes of each field and 1000 bytes of the rest. `objects.jsonl` holds it
whole.

### When G4 fails on op 0

The first settle wait expired. What follows `expired with no fault active` says why:

- `ready never held: evaluating ready "…": no such key: …` means your `ready` reads a
  field the CR does not have. Check the spelling, and guard an optional field with `has()`.
- `ready never held: it evaluated to false` means your controller never reached the state
  your `ready` describes. The report's Ready predicate section shows the CR's conditions
  and status, which is where a reason such as `0/10 replicas available` appears. Compare
  that status with your `ready`: a misspelled field under `has()` also evaluates to false.
  envtest runs only the API server and etcd: no Deployment, ReplicaSet or Pod controller
  runs, so a CR that waits on a Deployment's replicas never becomes ready there.
- `ready held from … on, but the namespace never held still for stable (2s)` means your
  controller converged and kept writing. The Object versions table lists the writes. A
  status field rewritten on every reconcile, such as a timestamp, does this.
- `ready held until …` means `ready` held and then stopped holding.

After a `delete`, `the CR … was still being deleted, held by the finalizers …` means
nothing removed those finalizers within `settle`.

A controller that converges, only more slowly than `timeouts.settle` allows, needs a wider
`settle`. Where your controller repeated a failing request, the line names it and its
count: an error loop that backs off can fail too rarely for G6 to count. A `ready` that yields
something other than a bool is a configuration error, and botbox exits 2 naming it.

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
`--deadline` room for your controller, because a run that overruns it, or an invocation it stops
before the last run, exits 2 rather than reporting a find. Cache the control plane and the target as
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
| G4 | Convergence. `ready` holds within `T_settle` of every spec change, and again once a fault stops. |
| G5 | Restart-stable. Restarting the target does not change converged state. |
| G6 | No error loop. The target does not repeat one failing request more than `N_errloop` times. |

[docs/bug-matrix.md](docs/bug-matrix.md) shows which check catches each bug seeded into the toy controller of [DESIGN.md §9](DESIGN.md#9-toy-target-widget), and CI regenerates it from real runs. Each bug's sequence also runs against the toy with no bug, and CI fails if a check fires there.

## Development and internals

- `make setup` installs the envtest control plane, and `make help` lists every target.
- `make test`, `make test-envtest`, `make test-example` and `make test-example-external-secrets` are the tiers CI runs on every PR.
- A block after `<!-- embed: path -->` holds that file byte for byte, and `make test` enforces it.
- [DESIGN.md](DESIGN.md) is the governing design. Code and docs must not contradict it.
- [docs/journal.md](docs/journal.md) and [docs/spikes/](docs/spikes/) hold the milestone journal and the experiments behind DESIGN.md §15.

## License

Apache-2.0. See [LICENSE](LICENSE).
