# botbox

Botbox is a tool to find bugs in your Kubernetes controller.

It fakes the API server, injecting various events (changes to resources, faults, restarts).  It then
checks if certain expectations hold, including custom properties you can specify.

## Install

```sh
go install github.com/rosenhouse/botbox/cmd/botbox@latest
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1
index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 --index $index -p path)"
```

### Against kind

botbox starts its own API server by default. That server runs no controller manager and no
kubelet. botbox emulates the garbage collector, but no Pod runs, a Pod bound to a node never
finishes deleting, and the status of a Deployment, a Job or a PersistentVolumeClaim never
changes. A `ready` that waits on that status never holds, and a controller that requeues
while it waits can hide a missed watch. botbox warns when your target manages such a kind.
To test such a controller, or against a real garbage collector, point botbox at a throwaway
cluster:

```sh
kind create cluster --kubeconfig kind.kubeconfig
botbox run --target target.yaml --kubeconfig kind.kubeconfig
kind delete cluster --kubeconfig kind.kubeconfig
```

- botbox installs the target's `crds`, replacing any CRD of the same name, and leaves them
  installed.
- Each run creates a namespace and deletes it at the end, unless botbox is killed.
- Your controller still runs on your machine, behind botbox's proxy. Do not also deploy it to
  the cluster, or run a second botbox against the cluster at the same time: botbox would
  count the other copy's work as your controller's.
- The cluster puts the `default` ServiceAccount and the `kube-root-ca.crt` ConfigMap in every
  namespace. botbox waits for them and never counts them, or anything else there before your
  controller starts, as your controller's.
- The cluster may add more objects later. If they are of a kind your target manages, label
  your own objects and declare a `selector` in `target.yaml`, so that botbox counts only
  those:

  ```yaml
  selector: app.kubernetes.io/managed-by=my-controller
  ```
- botbox counts a change the cluster makes to an object your target manages as your
  controller's. The garbage collector can delete a child seconds after its owner, especially
  just after botbox installs the owner's CRD. If that delete comes after `stable` of quiet,
  G2 fails. A wider `stable` avoids that.

`make test-kind` runs the toy controller this way ([DESIGN.md §5.8](DESIGN.md#58-test-cluster)).

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
that. `make test-example` runs this same control. It fails unless the default configuration
passes, the control fails on G3 naming that Secret, and the control's evidence hides the
Secret's private key. A nightly workflow draws its own seeds.

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
that control. It fails unless the control reports G3 and its evidence hides the Secret's value:

```
run 1: seed 20260922, sequence examples/external-secrets/sequences/orphan.json
run 1: G3 the v1/Secret example-secret was still there 1m0s after the CR was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-22T16:43:05.836050746Z; 1 version, the first v1/Secret example-secret
  the evidence is in botbox-out/external-secrets-control/20260922T164150Z-20260922/run-1
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
  - issuer.yaml                               # applied to the run namespace before op 0, deleted after the last
manages:                                      # group/version/Kind, or v1/Kind for the core group
  - v1/Secret
  - cert-manager.io/v1/CertificateRequest
notRecreated:                                 # managed kinds your controller leaves deleted
  - cert-manager.io/v1/CertificateRequest
ready: >-                                     # CEL over metadata, spec, status; must yield bool
  has(status.conditions) && status.conditions.exists(c,
    c.type == "Ready" && c.status == "True"
    && has(c.observedGeneration) && c.observedGeneration == metadata.generation)
launch:
  binary: bin/cert-manager-controller       # relative to the working directory, not to this file
  args:
    - --kubeconfig=$KUBECONFIG                # replaced with a kubeconfig for the proxy
    - --leader-elect=false                    # a target runs with leader election off
    - --enable-certificate-owner-ref=true
timeouts:                                     # optional; 30s, 10s and 60s by default
  settle: 30s                                 # the whole budget for one spec change
  stable: 10s                                 # the quiet it has to end in, carved out of settle
  delete: 60s                                 # how long a deletion has to come clean
thresholds:                                   # optional; 10 and 0 by default
  errloop: 10                                 # how often one failing request may repeat within settle
  quiet: 0                                    # how many requests one stable window may hold
```

A slow controller needs a wider `settle`. The quiet window sits inside the settle budget,
so the controller has `settle - stable` to stop writing. A `stable` at least as wide as
`settle` leaves it none, so botbox refuses to load that target rather than reporting G4
against your controller. A narrower `stable` also shortens the windows G1 and G2 judge.

A controller that resyncs on a timer makes requests after it has converged, and G1 fails
it by default. `quiet` is how many requests one `stable` window may hold. A window holds
at most one tick more than `stable` divided by the interval, rounded down. Multiply those
ticks by the requests one tick makes, and add up every timer your controller runs, such
as one per CR. A 15s resync that makes one request needs `quiet: 1` under the default
`stable`. `quiet` also bounds the status writes that change nothing, which G2 counts. A
write that changes something fails G2 whatever `quiet` is, or G4 if the timer is faster
than `stable`, because the settle wait then never sees `stable` of quiet. Keep `quiet` as
low as your timer allows, since G1 lets a slow loop of that many requests through.

G6 fails a controller that repeats one failing request more than `errloop` times within
`settle`. controller-runtime's default backoff repeats one 11 times in its first 5.1s,
which the default catches. A 5s `settle` holds only 10 of them, so it needs `errloop: 9`
or less.

botbox tests namespaced kinds only. It refuses a cluster-scoped primary, managed kind or
fixture before the first run, and it refuses a fixture that sets `metadata.namespace`. It
watches only the run namespace, so it does not see a child your controller creates in
another namespace.

Each run creates its own namespace, and the kubeconfig botbox hands your controller names
that namespace. `launch.env` sets variables for your controller. In its values and in
`launch.args`, botbox replaces the text `$NAMESPACE` with the run namespace and
`$KUBECONFIG` with the kubeconfig's path. It expands no other spelling, such as
`$(NAMESPACE)` or `${NAMESPACE}`. An operator-sdk operator watches the namespace that
`WATCH_NAMESPACE` names, and it may read `POD_NAMESPACE` for leader election:

```yaml
launch:
  binary: bin/manager
  env:
    WATCH_NAMESPACE: $NAMESPACE
    POD_NAMESPACE: $NAMESPACE
```

YAML reads an unquoted `0022` as 18 and `yes` as true, so botbox refuses a name or value
that YAML would change. Quote such a name or value.

Your controller also inherits botbox's environment, but a report's replay command does not
record it. Declare what your controller needs in `launch.env`, so that a replay reproduces
the run.

envtest runs no garbage collector, so botbox runs its own over the kinds your target
declares. It deletes an object once every owner the object names is gone. It treats a
foreground or orphan delete as a background one. It finds an owner by group, kind and name,
at any version the API server serves, and then compares the UID.
It counts as live an owner of a kind your target does not declare, or one named at a version
the API server does not serve, so it never deletes an object that names one. The run prints
a note for each such object and owner, and the report carries it. A real garbage collector
cannot resolve an unserved version either, so fix that reference in your controller. If your
controller creates an owner of an undeclared kind, add the kind to `manages`. Otherwise,
point `botbox run --kubeconfig` at a cluster such as kind, whose garbage collector resolves
every kind.

Every sequence starts by creating your `sample`, then draws from `update`, `delete`, `recreate`,
`settle`, `restart` and `deleteManaged`, which deletes one managed object behind the
controller's back. G7 then requires your controller to recreate an object of that kind and
name before the run settles. Where your `ready` still holds without the object, the run settles
once nothing has changed for `stable`, so your controller has `stable` to recreate it, however
wide `settle` is. If your controller leaves a kind deleted by design, or recreates it under a
new name, list the kind under `notRecreated`. cert-manager lists CertificateRequest, because a
Ready Certificate does not replace a deleted request.

Your controller may read an object it does not own, such as a Secret or an Issuer. Declare it
under `fixtures`, and name its file under `generate.fixtures` to let generation change it:

```yaml
generate:
  fixtures:
    secret.yaml:                              # a file under fixtures
      mutate:                                 # strings in it generation may set
        - data.token
```

Generation then also draws `updateFixture`, which sets one of those strings to a short word
of letters and digits, and `deleteFixture`, which deletes the fixture until the next op that
settles and then creates it again. botbox waits up to `timeouts.delete` for a deleted fixture
to go, so a finalizer your controller puts on it may hold it that long. Your `ready` may fail
while the fixture is gone, so no settle wait runs without it. A controller that reads the fixture without watching it misses
the change until something else reconciles its CR, and G5 reports what a restart then
changes. G7 never asks your controller to recreate a fixture. A sequence you write names the
op before which botbox creates a deleted fixture again:
`{"i": 2, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": 3}}`.
Without `generate.fixtures`, generation leaves every fixture alone.

A sequence you write yourself can also carry a `fault`, which makes the proxy refuse, delay or
drop the requests it matches. This is `targets/toy-widget/sequences/fault.json`:

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
it and waits for the controller before it tears the run down. The proxy tries faults in op
order, the first that applies to a request wins, and each runs out on its own `until`. A fault
that matches no request changes nothing and hides nothing ([DESIGN.md §5.2](DESIGN.md#52-proxy)).
`match.verb` is a Kubernetes verb such as `create` or `list`, and `match.resource` is the
plural the API server serves, such as `configmaps`. botbox refuses any other value, because
the fault would match nothing. A run notes each fault the proxy applied to no request.

Field values come from the CRD's own schema: its numeric ranges, enums, patterns, list
lengths and map sizes. Every CR botbox draws also passes the CRD's validation rules, CEL
`x-kubernetes-validations` included, because botbox checks each draw with the API server's
own code. That check sees the CR botbox writes and not the status your controller writes, so
a rule that reads status can still refuse a draw. A schema that says only `type: string`
yields a random word, so the schema is not a safety net. Where it allows more than your
controller does, `generate.mutate` lists the only paths a sequence changes and
`generate.overlay` tightens one path's schema, as `examples/cert-manager/target.yaml` does.
An int-or-string field needs an overlay that says which it is: `type: integer`, or
`type: string` with a `pattern` or an `enum`. Naming a path or an overlay keyword botbox
cannot draw from is a configuration error, not a silent skip. So is a path where the CRD
refuses every value botbox draws for it into your sample. Without `generate.mutate`, botbox
prints each spec path it leaves alone, and why. If the API server still refuses a CR, as a
webhook or a status rule might, botbox exits 2 and names the `sequence.json` that holds the
op ([DESIGN.md §8.3](DESIGN.md#83-generation-constraints-and-admission-webhooks)).

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
a failure, and `make test-example` runs every pinned sequence so none can rot.

In a sequence you write, put a `settle` op after a `restart`, and one before it unless the op
before it settles. G5 compares the states the controller settled in on either side, and leaves a
note instead of a verdict when another op changed something in between. G7 likewise notes a
`deleteManaged` that follows a `restart` before your controller has requested a resource outside
leader election, since botbox cannot otherwise tell that it is back. The `settle` op after the
`restart` waits for that request.

## Reading a report

A run that violates an invariant prints the ID, what it saw and where the evidence is, then
exits 1. A configuration or harness error exits 2, so your CI can tell a find from a broken
target. The evidence is in `botbox-out/<timestamp>-<seed>/run-<n>/`:

- `report.md` — what failed, the command that reproduces it, the sequence and the evidence.
- `report.json` — the same, for a machine.
- `sequence.json` — the sequence the rest of the directory is evidence of.
- `sequence.shrunk.json` — a smaller sequence the deadline left unrun. Present only then.
- `requests.jsonl` — every request the target made, as the proxy saw it.
- `objects.jsonl` — every version of every object the Observer saw. Each value of a Secret's
  `data` and annotations is a marker such as `[redacted 6 bytes hmac-sha256:8c7ef51307f40278]`.
- `target.log` — the target's own output.
- `kubeconfig` — the kubeconfig the target was given. It points at the proxy rather than the
  cluster, and names the run namespace.

Equal Secret values share a marker within one invocation, so you can see which value changed
without learning it. botbox hides nothing else. A Secret's labels, your sample, your CRs and
other objects, the `--launch-arg` values and your controller's log are written as they are, so
keep credentials out of them before you share `botbox-out/`.

The report quotes the last twenty requests and the last twenty object versions the check
chose from, says how many that was, and names the file holding the rest. A G4 or a
property also quotes the state of the objects your controller managed where it failed,
over the kinds your target declares: a second table with its own bound of twenty and
the count beside it. Passing runs are not kept ([DESIGN.md §5.7](DESIGN.md#57-report)).

A G4 also quotes your `ready`, the error evaluating it, and your CR's status where it
failed. The status holds whatever your controller wrote, so the report cuts it: twenty
conditions, 200 bytes of each field and 1000 bytes of the rest. `objects.jsonl` holds it
whole.

A G5 report lists each field the restart changed, with its value before and after, and the line
botbox prints names the first. If your controller stamps one of those fields at startup, paste
its path into `equalIgnore` as written. A Secret's values appear there as markers too.

Once a settle wait has converged, botbox restarts a controller that exits, as a kubelet
would: at once, then after 10s, doubling up to 5 minutes. The run prints a note for each
exit, quoting the line the controller wrote as it stopped. A settle wait does not converge
while the controller waits to restart. Nor does it converge until the controller has
requested a resource outside leader election since it last started and then run for
`stable`, because botbox has no other sign that it is back. After a `restart` op, a
controller has `settle` to come back, and `settle` past its return to converge. A
controller that crashes again within `stable` of each return never converges, even where
it wrote its converged state first, so G4 reports it and quotes the last exit.
A controller that exits during a fault, or while it recovers from one, has the same once
botbox restarts it. G7 notes a `deleteManaged` after a restart that follows an exit as
it does one after a `restart`, and notes one where your controller exited, or waited to
restart, during the op or its settle wait.
The toy controller converges a count of 0 and then crashes under `--launch-arg --bug=12`,
and `targets/toy-widget/sequences/b12.json` sets one:

```
run 1: the target exited during op 1 (update) with exit status 2 after writing "panic: runtime error: integer divide by zero [recovered, repanicked]"
run 1: the target exited during op 1 (update) with exit status 2 after writing "panic: runtime error: integer divide by zero [recovered, repanicked]"
run 1: G4 the settle wait after op 1 (update) expired with no fault active: in 5.038s, ready held from 12ms on, but the target was waiting to restart; the target exited 2 times since it last converged, last with exit status 2 after writing "panic: runtime error: integer divide by zero [recovered, repanicked]"
  at 2026-09-24T00:57:57.490964784Z; 15 requests, the first get /api 200; 5 versions, the first toy.botbox/v1/Widget widget; the target managed 0 objects of the kinds it declares
```

### When a settle wait fails G4

A settle wait expired. What follows `expired with no fault active` says why:

- `ready never held: evaluating ready "…": no such key: …` means your `ready` reads a
  field the CR does not have. Check the spelling, and guard an optional field with `has()`.
- `ready never held: it evaluated to false` means your controller never reached the state
  your `ready` describes. The report's Ready predicate section shows the CR's conditions
  and status, which is where a reason such as `0/10 replicas available` appears. Compare
  that status with your `ready`: a misspelled field under `has()` also evaluates to false.
  envtest runs only the API server and etcd: no Deployment, ReplicaSet or Pod controller
  runs, so a CR that waits on a Deployment's replicas never becomes ready there. botbox
  warns of this when `manages` names such a kind. Run such a target
  [against kind](#against-kind).
- `ready held from … on, but the namespace never held still for stable (2s)` means your
  controller converged and kept writing. The Object versions table lists the writes. A
  status field rewritten on every reconcile, such as a timestamp, does this.
- `ready held until …` means `ready` held and then stopped holding.
- A line that goes on `but the target was waiting to restart`, or `but the target
  restarted in the last stable`, means your controller exited. The line counts the exits
  since it last converged and quotes the last.
- A line that goes on `but the target had requested no resource outside leader election
  since …` means your controller had not come back from a restart, or had not started, when
  the wait gave up. `until the last stable` means it came back too late to run for
  `stable` before then. A controller slow to start needs a wider `settle`.

After a `delete`, the run waits up to `timeouts.delete` for the CR to go and then up to
`settle` for the rest to settle, so a slow cleanup needs no wider `settle`. A `recreate`
waits as long for the old CR to go before it creates the new one. A CR still there
`timeouts.delete` after its deletion fails G3, which names the finalizers still on it.
Where a fault reached into the deletion, G3 cannot judge it, and `the CR … was still being
deleted, held by the finalizers …` names them instead. `no CR was left to be ready, but the
namespace never held still …` means something kept writing after the CR was gone.

A controller that converges, only more slowly than `timeouts.settle` allows, needs a wider
`settle`. Where your controller repeated a failing request, the line names it and its
count: an error loop that backs off can fail too rarely for G6 to count. A `ready` that yields
something other than a bool is a configuration error, and botbox exits 2 naming it.

## When botbox exits 2

Exit 2 means botbox could not test your controller, and the message says what to change.

- `KUBEBUILDER_ASSETS` names the directory holding `etcd` and `kube-apiserver`. Install them
  as [Install](#install) shows, or point `--kubeconfig` at a cluster.
- A key target.yaml does not take fails with its line, as in `line 6: timeouts.setle is
  not a key; did you mean settle?`.
- `launch.binary` is relative to the directory you run botbox from. `crds`, `sample` and
  `fixtures` are relative to target.yaml.
- A controller that stops before its first settle wait converges ends the invocation,
  whether a flag, a taken port or the first CR stopped it. botbox quotes the line it wrote
  as it stopped, above any stack trace, and `target.log` in the run directory holds the
  rest. If botbox had created the CR and the controller had requested a resource from the
  API server, the CR may have crashed it, and the message names the run's `sequence.json`
  for `botbox replay`. A controller that binds a fixed port, such as a health probe on
  `:8081`, collides with a second invocation of itself. Give it a free port in
  `launch.args`, or with `--launch-arg`.

## Running in CI

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- run: go install github.com/rosenhouse/botbox/cmd/botbox@<commit>   # a commit of main
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
[.github/workflows/ci.yml](.github/workflows/ci.yml) does. The job needs no cluster and no registry.

Pin botbox to a commit, because `@latest` tracks main. Fix the seed on pull requests, and draw
fresh seeds on a schedule, as [nightly.yml](.github/workflows/nightly.yml) does. A seed names a
sequence for one build of botbox and one target declaration: its CRD schema, `sample`,
`generate` and `manages`. A botbox upgrade, or a pull request that edits any of those, draws
different sequences under the same seed. To tell whether a failure comes from the change under
review, replay its `sequence.json` against the base branch's controller.

## Invariants

Seven generic invariants apply to every target. [DESIGN.md §6](DESIGN.md#6-generic-invariants) states them exactly, with their windows, thresholds and attribution rules.

| ID | Checks |
|---|---|
| G1 | Bounded reconciliation. Under an unchanged spec, one quiet window holds no more requests than `quiet` allows, zero by default. |
| G2 | No churn. Once converged, the managed objects and their resourceVersions stop changing. |
| G3 | Clean deletion. Deleting the CR removes everything it manages and clears its finalizers. |
| G4 | Convergence. `ready` holds within `T_settle` of every change to the spec or a fixture, and again once a fault stops or the controller is back from a `restart`. A controller waiting to restart, or not yet back, has not converged. |
| G5 | Restart-stable. Restarting the target does not change converged state. |
| G6 | No error loop. The target does not repeat one failing request more than `N_errloop` times. |
| G7 | Self-healing. An object `deleteManaged` deletes exists again, by kind and name, once the run settles. |

[docs/bug-matrix.md](docs/bug-matrix.md) shows which check catches each bug seeded into the toy controller of [DESIGN.md §9](DESIGN.md#9-toy-target-widget), and CI regenerates it from real runs. Each bug's sequence also runs against the toy with no bug, and CI fails if a check fires there.

## Development and internals

- `make setup` installs the envtest control plane, and `make help` lists every target.
- `make test`, `make test-envtest`, `make test-example` and `make test-example-external-secrets` are the tiers CI runs on every PR.
- `make test-kind` runs the toy through `--kubeconfig` against a kind cluster that it creates and deletes. It needs Docker. The nightly workflow runs the same runs with `make test-kind-runs`.
- A block after `<!-- embed: path -->` holds that file byte for byte, and `make test` enforces it.
- [DESIGN.md](DESIGN.md) is the governing design. Code and docs must not contradict it.
- [docs/journal.md](docs/journal.md) and [docs/spikes/](docs/spikes/) hold the milestone journal and the experiments behind DESIGN.md §15.

## License

Apache-2.0. See [LICENSE](LICENSE).
