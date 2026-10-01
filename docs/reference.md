# Reference

This page lists every key of target.yaml, every field of a sequence file, every op and every
fault field. `make test` fails when one of them has no row here. [targets.md](targets.md) says
how to choose their values.

## target.yaml

botbox reads `crds`, `sample` and `fixtures` relative to target.yaml, and `launch.binary`
relative to the directory it runs in. A key it does not take is an error. A duration is a Go
duration, such as `30s` or `1m30s`. botbox refuses a cluster-scoped `primary`, kind under
`manages` or fixture, because a run [owns one namespace](../README.md#what-botbox-cannot-test-yet).

This example sets every key but `equal`:

<!-- embed: docs/reference/target.yaml -->
```yaml
# This sets every key but equal, which botbox refuses beside equalIgnore. It
# loads, because it names the toy controller's files. It would not run: the toy
# has no spec.prefix, labels no child and manages no Secret.
name: widget-controller
version: v1.2.3
crds:
  - ../../targets/toy-widget/crds/
primary: toy.botbox/v1/Widget
sample: ../../targets/toy-widget/widget.yaml
fixtures:
  - ../../targets/toy-widget/config.yaml
manages:
  - v1/ConfigMap
  - v1/Secret
notRecreated:
  - v1/Secret
selector: app.kubernetes.io/managed-by=widget-controller
ready: >-
  has(status.observedGeneration) && status.observedGeneration == metadata.generation
  && has(status.ready) && status.ready == spec.count
equalIgnore:
  - metadata.annotations["example.com/started-at"]
  - status.conditions[*].lastHeartbeatTime
properties:
  - id: P1
    description: status.ready never exceeds the ConfigMaps present.
    cel: '!has(status.ready) || status.ready <= managed.filter(o, o.kind == "ConfigMap").size()'
    when: checkpoint
generate:
  mutate:
    - spec.count
  overlay:
    spec.count: {minimum: 1, maximum: 5}
  maxCRs: 2
  distinct:
    - spec.prefix
  fixtures:
    ../../targets/toy-widget/config.yaml:
      mutate:
        - data.label
launch:
  binary: bin/widget-controller
  args:
    - --kubeconfig=$KUBECONFIG
    - --health-probe-bind-address=127.0.0.1:0
  env:
    WATCH_NAMESPACE: $NAMESPACE
timeouts:
  settle: 30s
  stable: 10s
  delete: 60s
thresholds:
  errloop: 10
  quiet: 1
```

| Key | Default | Meaning |
|---|---|---|
| `name` | required | It names the target. A sequence file gives the same name in `target`. |
| `version` | none | It is free text, such as your controller's release. Reports print it. |
| `crds` | none | It lists files or directories of CRD YAML, which botbox installs. A directory contributes its `.yaml`, `.yml` and `.json` files. |
| `primary` | required | It names your CR's kind as `group/version/Kind`. |
| `sample` | required | It names a file that holds one CR of the primary kind, with a `metadata.name`. Every sequence botbox draws creates it first, and changes copies of it. |
| `fixtures` | none | It lists files of objects that botbox creates in each run's namespace before op 0, such as a Secret your controller reads. A fixture sets no `metadata.namespace`. botbox never counts a fixture as your controller's. |
| `manages` | none | It lists the kinds your controller creates, as `group/version/Kind`, or `v1/Kind` for the core group. botbox watches them and judges your controller by them. |
| `notRecreated` | none | It lists the managed kinds your controller leaves deleted, or recreates under another name. G7 does not require them back. Each is also under `manages`. |
| `selector` | every object | Only the managed objects this label selector matches count as your controller's. |
| `ready` | `has(status.observedGeneration) && status.observedGeneration == metadata.generation` | It is CEL that says whether a CR is ready, or `go:<name>`. G4 requires it of every CR. |
| `equal` | none | It names a `go:<name>` hook that replaces G5's comparison of the states on either side of a restart. It takes no `equalIgnore`. |
| `equalIgnore` | none | It lists paths that G5 does not compare. |
| `properties` | none | It lists checks of your own. Each must hold on every CR. |
| `properties[*].id` | required | It names the property in output, such as `P1`. No two properties share one. |
| `properties[*].description` | none | It says what the property means. Reports print it. |
| `properties[*].cel` | required | It is CEL that says whether the property holds. |
| `properties[*].when` | `checkpoint` | It says where botbox evaluates the property: `always` at every change botbox observes, `checkpoint` wherever the checks run, and `end` at the last checkpoint. |
| `generate.mutate` | each spec path generation can draw a value for | It lists the dotted spec paths generation may change. Generation changes no others. Without it, `botbox run` prints each spec path it leaves alone, and why. |
| `generate.overlay` | none | It maps a dotted path to schema keywords. For generation, they win over the CRD's keywords there, and the CRD keeps those they do not name. botbox reads `additionalProperties`, `enum`, `exclusiveMaximum`, `exclusiveMinimum`, `format`, `items`, `maxItems`, `maxLength`, `maxProperties`, `maximum`, `minItems`, `minLength`, `minProperties`, `minimum`, `pattern`, `properties`, `required`, `type`, `x-kubernetes-int-or-string` and `x-kubernetes-list-type`, and refuses any other. |
| `generate.maxCRs` | `3` | It bounds the CRs a sequence creates, the sample included. `1` keeps every sequence to the sample. |
| `generate.distinct` | none | It lists dotted paths at which the sample holds a string, such as a field that names a child. Each CR after the first appends its `-2` or `-3` there, so no two CRs share a value. |
| `generate.fixtures` | none | It maps a file, written as `fixtures` lists it, to what generation may do to its objects. Generation may delete the objects of any file it names. |
| `generate.fixtures[*].mutate` | none | It lists paths to strings that generation may set to a short word of letters and digits, written as `equalIgnore` writes a path. Every object in the file holds a string there. |
| `launch.binary` | required | It names your controller's executable, relative to the directory botbox runs in, or a name on `PATH`. |
| `launch.args` | none | It lists your controller's arguments. botbox replaces `$KUBECONFIG` with the path of the kubeconfig it writes, and `$NAMESPACE` with the run's namespace. `--launch-arg` appends more. |
| `launch.env` | none | It maps variables that botbox sets over those your controller inherits, with the same replacements. botbox sets `KUBECONFIG` itself. Quote a name or value that YAML would change, such as `0022` or `yes`. |
| `timeouts.settle` | `30s` | It says how long a settle wait gives your controller to converge after a change. |
| `timeouts.stable` | `10s` | It says how long nothing may change before a settle wait converges. G1 and G2 judge a window this long. It is shorter than `settle`. |
| `timeouts.delete` | `60s` | It says how long a deleted CR has to go, with everything it manages. A `recreate` and a `deleteFixture` wait as long for their object to go. |
| `thresholds.errloop` | `10` | G6 fails a failing request repeated more than this many times within `settle`. It is positive. |
| `thresholds.quiet` | `0` | It bounds the requests one quiet window may hold for G1, and the status writes that change nothing for G2. It is not negative. |

DESIGN.md writes `settle`, `stable`, `delete`, `errloop` and `quiet` as `T_settle`,
`T_stable`, `T_delete`, `N_errloop` and `N_quiet`.

### CEL and hooks

`ready` binds `metadata`, `spec` and `status` of one CR, and an empty map to one the CR
lacks. Reading a key a map lacks is an error, so guard an optional field with `has()`. An
error while evaluating `ready` means not ready. A property also binds `managed`: the managed
objects whose ownerReferences name that CR or no CR. Where no CR exists, a property runs once,
with empty `metadata`, `spec` and `status`, over every managed object, and a violation there
says that no CR existed. The string extensions are available. An expression that yields no
bool, and an error while evaluating a property, end the run as a configuration error.

`go:<name>` names a function registered with `target.RegisterReady` or `target.RegisterEqual`.
Only a botbox built with that function can load the target.

### equalIgnore paths

A path joins keys with `.`, and `[*]` names every item of a list or value of a map. A key
that holds `.`, `[`, `]`, `"`, `*`, `/`, `:` or whitespace goes in brackets as a JSON string,
as in `metadata.annotations["example.com/started-at"]`. Write the list in block style, and
quote a path that starts with `[` or holds `: ` or ` #`. botbox refuses a list index such as
`[0]`.

G5 always skips `metadata.resourceVersion`, `metadata.uid`, `metadata.creationTimestamp`,
`metadata.generation`, `metadata.managedFields`, `status.conditions[*].lastTransitionTime`,
and each ownerReference whose owner is gone.

## Sequences

A sequence file is JSON, such as the `sequence.json` of a failing run.
`botbox run --target target.yaml <file>...` runs files as written, and `botbox replay` runs
one. botbox refuses a field it does not know, and an op that lacks a field its type needs or
carries one it does not take.

This example holds every op and every fault field. `make test-envtest` runs it against the
toy controller of `targets/toy-widget/target.yaml`, which passes it:

<!-- embed: docs/reference/sequence.json -->
```json
{"seed": 20260922, "target": "toy-widget", "ops": [
  {"i": 0, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget",
    "metadata": {"name": "widget"}, "spec": {"count": 1}}},
  {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps", "fraction": 0.5},
    "action": {"error": 500}, "until": {"count": 2}}},
  {"i": 2, "t": "update", "patch": {"spec": {"count": 3}}},
  {"i": 3, "t": "fault", "spec": {"match": {"verb": "patch", "resource": "widgets", "name": "widget-*"},
    "action": {"delay": "500ms"}, "until": {"for": "10s"}}},
  {"i": 4, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget",
    "metadata": {"name": "widget-2"}, "spec": {"count": 1}}},
  {"i": 5, "t": "update", "cr": "widget-2", "patch": {"spec": {"count": 2}}, "noSettle": true},
  {"i": 6, "t": "fault", "spec": {"match": {"verb": "delete", "resource": "configmaps"},
    "action": {"drop": true}, "until": {"op": 8}}},
  {"i": 7, "t": "update", "cr": "widget-2", "patch": {"spec": {"count": 0}}},
  {"i": 8, "t": "settle"},
  {"i": 9, "t": "restart"},
  {"i": 10, "t": "settle"},
  {"i": 11, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
  {"i": 12, "t": "updateFixture", "kind": "v1/ConfigMap", "name": "widget-config",
    "patch": {"data": {"label": "blue"}}},
  {"i": 13, "t": "deleteFixture", "kind": "v1/ConfigMap", "name": "widget-config", "until": {"op": 14}},
  {"i": 14, "t": "recreate", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget",
    "metadata": {"name": "widget"}, "spec": {"count": 2}}},
  {"i": 15, "t": "delete", "cr": "widget-2"}]}
```

Op 1 refuses about half the ConfigMap creates until it has refused two. Op 3 delays each
patch of a Widget whose name matches `widget-*` by 500ms, for 10s. Op 6 drops every ConfigMap
delete until op 8, so op 7's wait runs out, which the fault excuses. Op 8's wait gives the toy
time to recover.

### Sequence fields

| Field | Meaning |
|---|---|
| `seed` | It seeds the draws `match.fraction` makes. A replay repeats them where your controller repeats its requests in order. |
| `target` | It gives the target's `name`. botbox refuses a sequence for another target. |
| `ops` | It lists the ops in order. The last one settles. |

### Ops

Every op carries `i` and `t`. A settle wait follows each op that settles. It ends once
`ready` holds on every CR and nothing has changed for `timeouts.stable`, or once
`timeouts.settle` runs out. After a fault, a restart or a CR's deletion, it can run longer,
while the checks still give your controller time. The checks run where it ends.

| Op | Needs | May carry | Settles | What it does |
|---|---|---|---|---|
| `create` | `obj` | `noSettle` | yes | It creates `obj`, whose name no live CR has. |
| `update` | `patch` | `cr`, `noSettle` | yes | It applies `patch` to the CR as a JSON merge patch. |
| `delete` | none | `cr`, `noSettle` | yes | It deletes the CR and waits up to `timeouts.delete` for it to go. |
| `recreate` | `obj` | `cr`, `noSettle` | yes | It deletes the CR, waits up to `timeouts.delete` for it to go, and creates `obj`, which has the CR's name. |
| `settle` | none | none | yes | It waits for your controller to converge. |
| `restart` | none | none | no | It kills your controller and starts it again. |
| `fault` | `spec` | none | no | It adds a fault the proxy applies to your controller's requests. |
| `deleteManaged` | `kind`, `index` | none | yes | It deletes a managed object behind your controller's back. G7 requires an object of its kind and name once the wait ends, unless `notRecreated` lists the kind. |
| `updateFixture` | `kind`, `name`, `patch` | none | yes | It applies `patch` to a fixture as a JSON merge patch. |
| `deleteFixture` | `kind`, `name`, `until.op` | none | no | It deletes a fixture, waits up to `timeouts.delete` for it to go, and creates it again before op `until.op` acts. |

- End a sequence with an op that settles.
- An `update` or a `delete` acts on a live CR, and a `recreate` on a CR an op before it
  created.
- Put a `settle` after a `restart`, and one before it unless the op before it settles. G5
  compares the states those waits end in.
- Put a `settle` between a `noSettle` op and a `deleteManaged`.
- Put a `settle` between a `noSettle` `delete` and a `create` of its CR, or use a `recreate`,
  which waits for the old CR to go.

### Op fields

| Field | Meaning |
|---|---|
| `i` | It gives the op's position, counting from 0. |
| `t` | It names the op's type. |
| `cr` | It names the CR that an `update`, `delete` or `recreate` acts on. It defaults to the sample's `metadata.name`. |
| `kind` | A `deleteManaged` names a kind under `manages`, and a fixture op its fixture's kind. Both write it as target.yaml does, such as `v1/ConfigMap`. |
| `index` | It picks the object of `kind` that a `deleteManaged` deletes, counting from 0 by creationTimestamp, then name. Where fewer exist, the op deletes nothing, and the run notes that. |
| `name` | It gives the fixture's `metadata.name`. |
| `obj` | It holds the CR that a `create` or `recreate` writes, with a `metadata.name`. |
| `patch` | It holds a JSON merge patch (RFC 7386). |
| `spec` | It holds a `fault` op's fault. |
| `until.op` | It names the op before which a `deleteFixture` creates its fixture again. That op comes later, up to the last, and no op before it acts on the fixture. |
| `noSettle` | `true` skips the settle wait after a `create`, `update`, `delete` or `recreate`. |

### Fault fields

A fault sets exactly one action. It ends at the first of its `until` triggers, or at the end
of the run where it sets none. The proxy tries faults in op order, and the first that
applies to a request wins. The checks do not judge a window a fault applied in, and they
give your controller as long as the faults lasted, plus `timeouts.settle`, to recover.
botbox clears a fault still active at the end, and waits for your controller to recover. A
fault that applies to no request tests nothing, and the run notes it.

| Field | Default | Meaning |
|---|---|---|
| `match.verb` | every verb | It is one of `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` and `deletecollection`. |
| `match.resource` | every resource | It names the plural the API server serves, such as `configmaps`, in any group. It matches subresource requests too, such as those to `widgets/status`. botbox refuses one that holds a slash or that the API server does not serve. |
| `match.name` | every name | It is a glob over the object name in the request path, as Go's `path.Match` reads it. A list, a watch, a `deletecollection` and a create of an object carry an empty name there, which `*` matches. |
| `match.fraction` | every request | It gives the share of matching requests the fault applies to, above 0 and up to 1. The sequence's `seed` draws which. |
| `action.error` | none | The proxy answers with this status, from 400 to 599, and forwards nothing. |
| `action.delay` | none | The proxy holds the request this long, such as `"500ms"`, then forwards it. It is not negative, and 0 leaves it unset. |
| `action.drop` | none | `true` has the proxy close the connection without an answer. It forwards nothing. |
| `until.op` | none | The fault ends before this op acts. It is above the fault's own `i`. Past the last op, the fault lasts to the end. |
| `until.count` | none | The fault ends once it has applied to this many requests. It is above 0. |
| `until.for` | none | The fault ends this long after its op, such as `"2s"`. It is above 0. |

A fault names a resource, such as `configmaps`, because it matches the URL of a request.
`manages`, `deleteManaged` and the fixture ops name a kind, such as `v1/ConfigMap`, as an
object's `apiVersion` and `kind` do.
