# Checks

Every run applies [G1](#g1-bounded-reconciliation) to [G7](#g7-self-healing) and your
[properties](#properties).

## G1 Bounded reconciliation

Once a settle wait has ended, your controller makes no more than `thresholds.quiet` requests
in the next `timeouts.stable`. Watches, leader election and requests that name no resource,
such as a health probe, do not count. A resync timer usually fails it, and needs a `quiet` that
[fits the timer](targets.md#thresholds). A `RequeueAfter` that polls a converged CR for what
a watch would deliver is a bug: watch the input rather than raise `quiet`.

`the target made … API requests in …, where thresholds.quiet allows …` counts the requests
your controller made while reconciler-fuzzer expected it to be quiet.

## G2 No churn

Once a settle wait has ended, your controller changes no CR and no object it manages for
`timeouts.stable`. A timestamp in a status usually fails it.

`the target changed … objects in …, where a converged target changes nothing` means your
controller kept rewriting what it manages. `the target made … status writes in …` means it
wrote status more often than [`thresholds.quiet`](targets.md#thresholds) allows, even where the
writes changed nothing.

## G3 Clean deletion

A deleted CR goes within `timeouts.delete`, with its finalizers and every object it owns.
Nothing your controller manages remains once no CR does. A child that lacks an ownerReference
to its CR usually fails it, since [the garbage collector](targets.md#garbage-collection)
deletes only what names an owner.

`the … was still there … (timeouts.delete) after … was deleted` names an object left behind,
and says whether it is orphaned:

- An orphaned object carries no ownerReference to the CR. Give it one, or delete it before
  your controller removes the CR's finalizer.
- An object that is not orphaned waited on something else: a finalizer of its own, another
  owner that still exists, or an owner the collector could not resolve, which it counts as
  live. `objects.jsonl` shows its finalizers and ownerReferences. The run notes each owner the
  collector could not resolve with `reconciler-fuzzer's garbage collector never deletes …`.

`the CR … still carried the finalizers … (timeouts.delete) after its deletion` means your
controller did not remove its finalizer. Where the run also notes `the teardown force-removed
the finalizers of …`, the cleanup never ended: check that it runs and then removes the
finalizer, and look for a finalizer of a child's own that the note names. Without that note,
the cleanup ended late and needs a wider [`timeouts.delete`](targets.md#timeouts).

Where a fault reached into the deletion, the run notes the deletion rather than judging it,
and [G4](#g4-convergence) judges the settle wait instead.

## G4 Convergence

Within `timeouts.settle` of each change, `ready` holds on every CR and nothing changes for
`timeouts.stable`. A fault, a restart or a CR's deletion gives your controller longer, as
[Timeouts](targets.md#timeouts) and [Faults](targets.md#faults) say. A settle wait that runs
out otherwise fails, and what follows `expired with no fault active` says why:

- `ready never held: evaluating ready "…": no such key: …` means your `ready` reads a field the
  CR does not have. Check the spelling, and guard an optional field with `has()`.
- `ready never held: it evaluated to false` means your controller never reached the state your
  `ready` describes. The report's Ready predicate section shows the CR's conditions and status,
  where a reason such as `0/10 replicas available` appears. Compare that status with your
  `ready`: a misspelled field under `has()` also evaluates to false. envtest runs no Pod, so a
  CR that waits on a Deployment's replicas never becomes ready there. Run such a target
  [against a cluster](targets.md#against-a-cluster).
- `ready held from … on, but the namespace never held still for … (timeouts.stable)` means
  your controller converged and kept writing. The Object versions table lists the writes. A
  status field rewritten on every reconcile, such as a timestamp, does this.
- `ready held until …` means `ready` held and then stopped holding. The Object versions table
  shows the version where it stopped.
- `but the target was waiting to restart`, or `but the target restarted in the last …
  (timeouts.stable)`, means your controller [exited](failures.md#restarts-and-crash-loops).
  The line counts the exits since it last converged, and quotes the last.
- `but the target had requested no resource outside leader election since …`, or `but the
  target had won no lease since …` for a controller that elects a leader, means your
  controller had not come back from a restart, or had not started, when the wait gave up.
  `until the last … (timeouts.stable)` means it came back too late to run for `stable` before
  then. A controller slow to start needs a wider `settle`.
- `no CR was left to be ready, but the namespace never held still …` means something kept
  writing after the CR was gone.
- `the CR … was still being deleted, held by the finalizers …` means a CR whose deletion a
  fault reached into never went. Where its deletion deadline held the wait open past `settle`,
  the line gives `timeouts.delete is …` in place of `timeouts.settle is …`.

`the CR … was not ready … after …` means `ready` did not hold `timeouts.settle` after an op or
after a [fault](targets.md#faults) stopped, or by the later time a fault or restart gives your
controller. It names the op or `the fault stopped`, then any error evaluating `ready`.

A controller that converges, only more slowly than `timeouts.settle` allows, needs a wider
`settle`. Where your controller [repeated a failing request](#g6-no-error-loop), either line
names it and its count. A
`ready` that yields something other than a bool is a configuration error, and
reconciler-fuzzer exits 2 naming it.

The report quotes your `ready`, the error evaluating it, and your CR's status where it failed.
The status holds whatever your controller wrote, so the report cuts it: twenty conditions, 200
bytes of each field and 1000 bytes of the rest.

## G5 Restart-stable

What your controller manages after a `restart` op equals what it managed before, compared
where the settle waits on either side converge. A field set at startup usually fails it. So
does a change the controller sees only on restart, such as one to a
[fixture](targets.md#fixtures) it does not watch.

`… the Restart at op …` names the first object that changed, appeared or went. The report lists
each field the restart changed, with its value before and after, and a Secret's values as
markers. If your controller stamps one of those fields at startup, paste its path into
[`equalIgnore`](targets.md#equalignore) as written. Otherwise, your controller missed a change
before the restart, so check that it watches what it reads.

## G6 No error loop

Your controller repeats one failing request no more than `thresholds.errloop` times within
`timeouts.settle`. A 409 Conflict on an update or a patch does not count. A retry of an error
that never clears usually fails it.

`the target repeated the failing request … times within … (timeouts.settle), where
thresholds.errloop allows …` names the request your controller kept retrying. A 403 usually
means your [`rbac`](targets.md#rbac) lacks a permission.
[Thresholds](targets.md#thresholds) says how `errloop` relates to `settle`. An error loop that
backs off can repeat too rarely for this check to count it.

## G7 Self-healing

An object that a `deleteManaged` op deleted exists again, by kind and name, when the settle
wait after the op ends. [Deleted objects](targets.md#deleted-objects) says how long your
controller has.

`the … that op … (deleteManaged) deleted never came back …` means your controller did not
recreate the object. Check that it watches the kind, and that the object carries the controller
ownerReference that `Owns()` follows. List a kind your controller leaves deleted by design
under [`notRecreated`](targets.md#deleted-objects).

The run notes, rather than judges, a `deleteManaged` that comes before your controller is back
from a restart, or one during which your controller exited or waited to restart.

## Properties

A property must hold on every CR where its `when` says. Its default `when`, `checkpoint`,
evaluates it wherever a settle wait ends and at the end of the teardown, which deletes every CR.
A property also runs where no CR exists, as after a `delete` or at the end of the teardown,
and fails there with `the property did not hold where no CR existed: …`. Guard it with
`has()`, or begin it with `!has(metadata.name) ||`. reconciler-fuzzer exits 2 with `no such key`
where a property reads a field that is missing. [CEL and hooks](reference.md#cel-and-hooks)
says what it binds.

A `checkpoint` or `end` property skips a checkpoint your controller may not have caught up to:
under a [fault or a held request](targets.md#faults), or while your controller is
[still starting](failures.md#restarts-and-crash-loops). The run notes each.

`the property did not hold on the CR …: …` and the report quote the property's description. The
report also quotes the versions of the CR it failed on, and the
managed objects' metadata. Read the values the property judged from `objects.jsonl`. From the
evidence directory, this prints each version of the ConfigMap `widget-0`'s data:

```sh
jq -c 'select(.name == "widget-0") | .object.data' objects.jsonl
```
