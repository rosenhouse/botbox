# Writing a target

The [README](../README.md#your-own-controller) walks through the toy's `target.yaml`.
[reference.md](reference.md) lists every key with its default. This page says how botbox uses
the keys, and how to choose their values.

## Timeouts

A settle wait follows each op that [settles](reference.md#ops). It ends once `ready` holds on
every CR and nothing has changed for `timeouts.stable`, or once `timeouts.settle` runs out. G4
fails a wait that runs out with no fault active.

- A slow controller needs a wider `settle`. The quiet window sits inside `settle`, so your
  controller has `settle - stable` to stop writing.
- botbox refuses to load a target whose `stable` is at least as wide as `settle`, since no
  wait could then converge.
- A narrower `stable` also shortens the windows that G1 and G2 judge.
- After a `delete`, the run waits up to `timeouts.delete` for the CR to go, and then up to
  `settle` for the rest to settle. A slow cleanup needs a wider `delete`, not a wider
  `settle`.

## Thresholds

A controller that resyncs on a timer makes requests after it has converged, and G1 fails it by
default. `thresholds.quiet` is how many requests one `stable` window may hold. A timer with
interval `i` ticks at most `floor(stable / i) + 1` times in a window. Multiply that by the
requests one tick makes, and add up every timer your controller runs, such as one per CR. A 15s
resync that makes one request needs `quiet: 1` under the default `stable` of 10s.

`quiet` also bounds the status writes that change nothing, which G2 counts. A write that
changes something fails G2 whatever `quiet` is. If the timer that makes it fires more often
than once per `stable`, your controller fails G4 instead, because the settle wait never sees
`stable` of quiet. Keep `quiet` as low as your timer allows, since G1 lets a slow loop of
that many requests through.

G6 fails a controller that repeats one failing request more than `thresholds.errloop` times
within `settle`. controller-runtime's default backoff repeats a request 11 times in its first
5.1s, which the default of 10 catches. A 5s `settle` holds only 10 of those, so it needs
`errloop: 9` or less.

## Launch

Each run creates its own namespace, and the kubeconfig botbox hands your controller names it.
In `launch.args` and in the values of `launch.env`, botbox replaces `$NAMESPACE` with the run's
namespace and `$KUBECONFIG` with the kubeconfig's path. It expands no other spelling, such as
`$(NAMESPACE)` or `${NAMESPACE}`. An operator-sdk operator watches the namespace that
`WATCH_NAMESPACE` names, and it may read `POD_NAMESPACE` for leader election:

```yaml
launch:
  binary: bin/manager
  env:
    WATCH_NAMESPACE: $NAMESPACE
    POD_NAMESPACE: $NAMESPACE
```

YAML reads an unquoted `0022` as 18 and `yes` as true, so botbox refuses a name or value that
YAML would change. Quote such a name or value.

Your controller also inherits botbox's environment, but a report's replay command does not
record it. Declare what your controller needs in `launch.env`, so that a replay reproduces the
run.

Turn leader election off, as `--leader-elect=false` does in a kubebuilder project. A restarted
controller would otherwise wait for its old lease to run out. Give each port your controller
binds a free one, such as `127.0.0.1:0`, so that two invocations do not collide.

## Garbage collection

envtest runs no garbage collector, so botbox runs its own over the kinds your target declares.
It deletes an object once every owner the object names is gone. It treats a foreground or
orphan delete as a background one. It finds an owner by group, kind and name, at any version
the API server serves, and then compares the UID. A failing run's `collector.jsonl` records
each delete it tried and how the delete ended.

botbox counts as live an owner it cannot resolve: one of a kind your target does not declare,
or one named at a version the API server does not serve. It never deletes an object that names
such an owner. The run prints a note for each such object and owner, and the report carries
it. A real garbage collector cannot resolve an unserved version either, so fix that reference
in your controller. If your controller creates an owner of an undeclared kind, add the kind to
`manages`. Otherwise, run botbox [against a cluster](#against-a-cluster), whose garbage
collector resolves every kind.

## Several CRs

Every sequence botbox draws first creates your `sample`, with drawn values in some of its spec
fields. A drawn `create` adds a second or a third CR, named after your sample with `-2` or `-3`,
so that botbox tries several CRs side by side. `generate.maxCRs: 1` keeps every sequence to
your sample, for a controller that takes one CR per namespace.

If your CR names a child in its spec, as cert-manager's `spec.secretName` names its Secret,
list that path under `generate.distinct`. Each CR after the first then appends its suffix to
your sample's value there. Otherwise two CRs name one child, and botbox reports the fight that
follows as your controller's.

Deleting a CR must remove the objects whose ownerReferences name it. An object that names no
CR may belong to any CR, so it may stay until the last CR goes. A property's `managed` holds the
objects that name the CR it judges, and those that name no CR.

## Generated values

botbox draws field values from your CRD's schema: its numeric ranges, enums, patterns, list
lengths and map sizes. Every CR it draws also passes the CRD's validation rules, CEL
`x-kubernetes-validations` included, because botbox checks each draw with the API server's own
code. That check sees the CR botbox writes, not the status your controller writes, so a rule
that reads status can still refuse a draw.

A schema that says only `type: string` yields a random word. No webhook of yours runs, so
narrow what generation draws to what your controller and your webhooks accept:

- Write in your `sample` what your webhooks would add.
- `generate.mutate` lists the only spec paths a sequence changes. Without it, `botbox run`
  prints each spec path it leaves alone, and why.
- `generate.overlay` tightens one path's schema, as `examples/cert-manager/target.yaml` does.
  An int-or-string field needs an overlay that says which it is: `type: integer`, or
  `type: string` with a `pattern` or an `enum`.

botbox exits 2 on a path or an overlay keyword it cannot draw from, and on a path where the CRD
refuses every value botbox draws into your sample. If the API server still refuses a CR, as a
webhook or a status rule might, botbox exits 2 and names the `sequence.json` that holds the op.

## Deleted objects

A `deleteManaged` deletes one managed object behind your controller's back. G7 then requires
your controller to recreate an object of that kind and name before the run settles. Where your
`ready` still holds without the object, the run settles once nothing has changed for `stable`.
Your controller then has `stable` to recreate it, however wide `settle` is.

If your controller leaves a kind deleted by design, or recreates it under a new name, list the
kind under `notRecreated`. cert-manager lists CertificateRequest, because a Ready Certificate
does not replace a deleted request.

Drawn sequences hold few `deleteManaged` ops, so
[pin one per managed kind](../README.md#pin-sequences).

## Fixtures

Your controller may read an object it does not own, such as a Secret or an Issuer. Declare it
under `fixtures`. botbox creates each fixture in the run's namespace before the first op, and
never counts it as your controller's. Name its file under `generate.fixtures` to let
generation change it:

```yaml
generate:
  fixtures:
    secret.yaml:                              # This file is under fixtures.
      mutate:                                 # Generation may set these strings.
        - data.token
```

Generation then also draws two ops:

- `updateFixture` sets one of those strings to a short word of letters and digits. Name only
  strings in which your controller accepts any such word, because `ready` must hold after each
  change. A Secret's `data` decodes the word to arbitrary bytes.
- `deleteFixture` deletes the fixture, and creates it again before the next op that settles.
  botbox waits up to `timeouts.delete` for the fixture to go, so a finalizer your controller
  puts on it may hold it that long. Your `ready` may fail while the fixture is gone, so no
  settle wait runs then.

A controller that reads a fixture without watching it misses a change until something else
reconciles its CR. G5 then reports what a restart changes. G7 never asks your controller to
recreate a fixture. Without `generate.fixtures`, generation leaves every fixture alone.

## equalIgnore

G5 compares what your controller manages before and after a restart. It already skips what
every restart moves, such as `metadata.resourceVersion`. If your controller stamps a field of
its own at startup, name it in `equalIgnore`:

```yaml
equalIgnore:
  - metadata.annotations["example.com/started-at"]
  - status.conditions[*].lastHeartbeatTime
```

Quote a key that holds a dot or a slash, and write `[*]` for every item of a list. Keep the
list in block style, because YAML claims the brackets inside a one-line `[...]` list. botbox
refuses a list index such as `[0]`, and a label or annotation key that the dots split. A key
names nothing inside a list, so a run notes a path such as
`status.conditions.lastHeartbeatTime` and says where the `[*]` goes. A G5 report gives each
path that changed in the form `equalIgnore` takes.

## Sequences you write

A sequence file is JSON, such as a failing run's `sequence.json`. botbox runs it as written and
never minimizes it. `botbox replay --target target.yaml sequence.json` runs one, and
`botbox run --target target.yaml a.json b.json` runs several. This is
`examples/cert-manager/sequences/issue.json`, reflowed:

```json
{"seed": 20260920, "target": "cert-manager", "ops": [
  {"i": 0, "t": "create", "obj": {"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
    "metadata": {"name": "example"}, "spec": {"secretName": "example-tls",
    "commonName": "example.test", "dnsNames": ["example.test"],
    "issuerRef": {"kind": "Issuer", "name": "selfsigned"}}}},
  {"i": 1, "t": "delete"}]}
```

[reference.md](reference.md#sequences) lists every op and field. botbox checks a sequence
before it runs it:

- A `create` names its CR in `obj`. An `update`, `delete` or `recreate` acts on your sample's
  CR, unless it names another in `cr`, as in `{"i": 2, "t": "delete", "cr": "example-2"}`.
- botbox refuses an op on a CR that no op before it creates, and an `update` or `delete` of a
  CR deleted since it was last created.
- botbox refuses a `create` of a CR that a `noSettle` `delete` removed, unless an op between
  them settles, because a finalizer may still hold the old CR. A `recreate` waits for the old
  CR to go.
- A `deleteFixture` names the op before which botbox creates the fixture again, as in
  `{"i": 2, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": 3}}`.

Put a `settle` op after a `restart`, and one before it unless the op before it settles. G5
compares the states your controller settled in on either side. It notes, rather than judges,
what another op may have changed in between. The `settle` after a `restart` also waits for your
controller to [come back](failures.md#restarts-and-crash-loops). G7 notes, rather than judges, a
`deleteManaged` that comes before your controller is back.

## Faults

A sequence you write can carry a `fault`, which makes the proxy refuse, delay or drop the
requests it matches. This is `targets/toy-widget/sequences/fault.json`:

<!-- embed: targets/toy-widget/sequences/fault.json -->
```json
{"seed": 20260920, "target": "toy-widget", "ops": [
  {"i": 0, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget",
    "metadata": {"name": "widget"}, "spec": {"count": 1}}},
  {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps"},
    "action": {"error": 500}, "until": {"count": 30}}},
  {"i": 2, "t": "update", "patch": {"spec": {"count": 3}}}]}
```

The checks do not judge a window the proxy applied a fault in, so a fault tests how your
controller behaves once the fault stops. A controller backs off while its requests fail, so
once the faults stop botbox gives it as long as they lasted, plus `settle`, to converge. That
includes a fault still active when the sequence ends, like the one above. botbox clears it and
waits for your controller before it tears the run down.

A delay's window lasts until it stops and the proxy releases the last request it held. A settle
wait counts a held request as a change until the proxy releases it. A watch counts only until
the proxy forwards it. A request held as the wait's time runs out keeps the wait open until
`settle` past its release. It keeps a `recreate`'s wait for its old CR open the same way. Where
a wait ends with a request held, or released within `stable`, botbox does not check your
properties there, and the run notes it.

The proxy tries faults in op order, the first that applies to a request wins, and each runs
out on its own `until`. A fault that matches no request changes nothing and hides nothing, and
the run notes it. `match.verb` is a Kubernetes verb such as `create` or `list`, and
`match.resource` is the plural the API server serves, such as `configmaps`. botbox refuses any
other value, because the fault would match nothing. It also refuses a value that would test
something else, such as a `fraction` of 0 or 50, or an `until.count` of 0.

The deadline botbox derives allows for how long each fault can hold a run open. A few faults
that stop one after another can hold it open for hours, so give a sequence with faults a
`--deadline`.

## Against a cluster

botbox starts envtest by default: an API server and etcd, with no controller manager and no
kubelet. botbox emulates the garbage collector, but no Pod runs, a Pod bound to a node never
finishes deleting, and the status of a Deployment, a Job or a PersistentVolumeClaim never
changes. A `ready` that waits on that status never holds, and a controller that requeues while
it waits can hide a missed watch. botbox warns when your target manages such a kind. To test
such a controller, or against a real garbage collector, point botbox at a throwaway cluster:

```sh
kind create cluster --kubeconfig kind.kubeconfig
botbox run --target target.yaml --kubeconfig kind.kubeconfig
kind delete cluster --kubeconfig kind.kubeconfig
```

- botbox installs the target's `crds`, replacing any CRD of the same name, and leaves them
  installed.
- Each run creates a namespace and deletes it at the end, even when you interrupt botbox. A
  SIGKILL leaves it behind.
- Your controller still runs on your machine, behind botbox's proxy. Do not also deploy it to
  the cluster, and do not run a second botbox against the cluster at the same time. botbox
  would count the other copy's work as your controller's.
- The cluster puts the `default` ServiceAccount and the `kube-root-ca.crt` ConfigMap in every
  namespace. botbox waits for them. It never counts them, or anything else there before your
  controller starts, as your controller's.
- The cluster may add more objects later. If they are of a kind your target manages, label
  your own objects and declare a `selector`, such as
  `selector: app.kubernetes.io/managed-by=my-controller`. botbox then counts only those.
- botbox counts a change the cluster makes to an object your target manages as your
  controller's. The garbage collector can delete a child seconds after its owner, especially
  just after botbox installs the owner's CRD. If that delete comes after `stable` of quiet, G2
  fails. A wider `stable` avoids that.

`make test-kind` runs the toy controller this way.
