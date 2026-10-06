# Reading a failure

The [README](../README.md#reading-a-failure) shows how to read a failure. This page says what
each file holds, and what each message means.

## The summary

Once reconciler-fuzzer has read or drawn its sequences, it makes `reconciler-fuzzer-out/<timestamp>-<seed>/` and
writes `summary.json` and `summary.md` there. It rewrites them as each run starts and when the
invocation ends. They list each planned run: its seed, how it ended, the faults the proxy
applied, the times your controller exited, and what the checks could not judge. `summary.json`
also holds each run's sequence as reconciler-fuzzer drew or read it. Its `schema` changes when a field
changes meaning or goes away.

A run that was under way when reconciler-fuzzer was killed reads `unfinished`. A run that had already
failed reads `violation`, even while reconciler-fuzzer was still minimizing its sequence. Its `run-<n>/`
then holds no report, and `summary.md` says whether `run-<n>/` holds the failing run's evidence
or a partial run of the minimized sequence.

## The evidence

As soon as a drawn run fails, reconciler-fuzzer prints the check, as in
`run 1: G3 failed, and minimizing its 12 ops can take minutes.` Then it minimizes the
sequence: it removes each op the failure does not need. Each removal it tries replays a whole
run. Where your controller fails with no CR at all, the minimized sequence lacks even the
`create`.

A passing run leaves only its entry in the summary. A failing run writes its evidence in
`run-<n>/`:

- `report.md` says what failed, gives the command that reproduces it, and quotes the sequence
  and the evidence.
- `report.json` holds the same, for a machine.
- `sequence.json` holds the sequence that the rest of the directory is evidence of. reconciler-fuzzer
  minimizes a drawn sequence before it reports, so this is the minimized one, unless the
  deadline or an interrupt cut minimizing short. `summary.json` keeps the sequence as drawn.
- `sequence.shrunk.json` holds a smaller failing sequence that reconciler-fuzzer found but did not run
  again for its evidence, because the deadline or an interrupt came first. It exists only then.
- `requests.jsonl` holds every request your controller made, as the proxy saw it.
- `objects.jsonl` holds every version of every object reconciler-fuzzer observed.
- `collector.jsonl` holds each delete that reconciler-fuzzer's garbage collector tried on envtest: when,
  the object, each owner and why it counts as gone, and the result: `deleted`, `not found`,
  `conflict` or `error`. A finalizer can still hold an object marked `deleted`. `not found` says
  the object was already gone. `conflict` says the object changed after the collector read it,
  and the line's `error` names the precondition that failed: the UID or the resourceVersion.
  The result `error` covers every other failure. Where the line's `error` then names
  `context canceled`, reconciler-fuzzer stopped the collector while the delete was in flight. The
  collector's deletes never pass the proxy, so `requests.jsonl` lacks them. The last lines can
  race reconciler-fuzzer's own cleanup as the run ends. The file is empty where the collector tried no
  delete, and absent on a cluster with a garbage collector of its own.
- `target.log` holds your controller's own output.
- `kubeconfig` is the kubeconfig your controller was given. It points at the proxy rather than
  the API server, and names the run's namespace.

`objects.jsonl` writes each value of a Secret's `data` and annotations as a marker, such as
`[redacted 6 bytes hmac-sha256:8c7ef51307f40278]`. Equal values share a marker within one
invocation, so you can see which value changed without learning it. reconciler-fuzzer hides nothing else.
It writes a Secret's labels, your sample, your CRs and other objects, the `--launch-arg` values
and your controller's log as they are. Keep credentials out of them before you share
`reconciler-fuzzer-out/`.

## What a report quotes

A report quotes the last twenty requests and the last twenty object versions that the check
looked at. It says how many it looked at, and names the file that holds them all. A G4 or a
property also quotes the objects your controller managed where it failed, over the kinds your
target declares. That table has its own bound of twenty, and gives each object's metadata
only. `objects.jsonl` holds each version whole.

A G4 also quotes your `ready`, the error evaluating it, and your CR's status where it failed.
The status holds whatever your controller wrote, so the report cuts it: twenty conditions, 200
bytes of each field and 1000 bytes of the rest.

A G5 lists each field the restart changed, with its value before and after, and the line reconciler-fuzzer
prints names the first. If your controller stamps one of those fields at startup, paste its
path into `equalIgnore` as written. A Secret's values appear there as markers too.

## When a property fails

Each run ends with a teardown, which deletes every CR and checks once more. A property runs
where its `when` says, once for each CR. Its default `when`, `checkpoint`, runs it wherever a
settle wait ends and at the end of the teardown. It binds that CR's `metadata`, `spec` and
`status`, and `managed`: the objects whose ownerReferences name that CR, and those that name no
CR. Where no CR exists, it runs once with empty `metadata`, `spec` and `status` over every
managed object, and the report says that no CR existed. So every property can run where no CR
exists, and one whose `when` is `checkpoint` or `end` does so at the teardown unless it skips
that checkpoint. Guard it with `has()`, or begin it with `!has(metadata.name) ||`, which holds
there.

A `checkpoint` or `end` property skips a checkpoint where the proxy
[held a request](targets.md#faults), where your controller was
[still starting](#restarts-and-crash-loops), or where a [fault](targets.md#faults) was active
or your controller was still owed time to recover from one. The run notes each.

The report quotes the property's description, the versions of the CR it failed on, and the
managed objects' metadata. Read the values the property judged from `objects.jsonl`. From the
evidence directory, this prints each version of the ConfigMap `widget-0`'s data:

```sh
jq -c 'select(.name == "widget-0") | .object.data' objects.jsonl
```

## When a settle wait fails G4

A settle wait expired. What follows `expired with no fault active` says why:

- `ready never held: evaluating ready "…": no such key: …` means your `ready` reads a field the
  CR does not have. Check the spelling, and guard an optional field with `has()`.
- `ready never held: it evaluated to false` means your controller never reached the state your
  `ready` describes. The report's Ready predicate section shows the CR's conditions and status,
  where a reason such as `0/10 replicas available` appears. Compare that status with your
  `ready`: a misspelled field under `has()` also evaluates to false. envtest runs no Deployment,
  ReplicaSet or Pod controller, so a CR that waits on a Deployment's replicas never becomes
  ready there. Run such a target [against a cluster](targets.md#against-a-cluster).
- `ready held from … on, but the namespace never held still for 2s (timeouts.stable)` means
  your controller converged and kept writing. The Object versions table lists the writes. A
  status field rewritten on every reconcile, such as a timestamp, does this.
- `ready held until …` means `ready` held and then stopped holding.
- `but the target was waiting to restart`, or `but the target restarted in the last 2s
  (timeouts.stable)`, means your controller exited. The line counts the exits since it last
  converged, and quotes the last.
- `but the target had requested no resource outside leader election since …`, or `but the
  target had won no lease since …` for a controller that elects a leader, means your
  controller had not come back from a restart, or had not started, when the wait gave up.
  `until the last 2s (timeouts.stable)` means it came back too late to run for `stable` before
  then. A controller slow to start needs a wider `settle`.
- `no CR was left to be ready, but the namespace never held still …` means something kept
  writing after the CR was gone.

A controller that converges, only more slowly than `timeouts.settle` allows, needs a wider
`settle`. Where your controller repeated a failing request, the line names it and its count.
An error loop that backs off can repeat too rarely for G6 to count it. A `ready` that yields
something other than a bool is a configuration error, and reconciler-fuzzer exits 2 naming it.

## When a CR does not go

After a `delete`, the run waits up to `timeouts.delete` for the CR to go, and then up to
`settle` for the rest to settle. A `recreate` waits as long for the old CR to go before it
creates the new one. A CR still there `timeouts.delete` after its deletion fails G3, which names
the finalizers still on it.

Where a fault reached into the deletion, G3 cannot judge it. The settle wait's G4 then says
`the CR … was still being deleted, held by the finalizers …`. Where the CR's deletion deadline
held the wait open past `settle`, the line gives `timeouts.delete is …` in place of
`timeouts.settle is …`. A `recreate` whose old CR a fault keeps past the wait stops the run, and
the run notes it. reconciler-fuzzer then clears the fault, and the settle wait after the last fault stopped
says that line where the CR still does not go.

## When G1, G2, G6 or G7 fails

- G1's `the target made … API requests in …, where thresholds.quiet allows …` counts the
  requests your controller made while reconciler-fuzzer expected it to be quiet. A controller that
  resyncs on a timer needs a `quiet` that [fits the timer](targets.md#thresholds).
- G2's `the target changed … objects in …, where a converged target changes nothing` means your
  controller kept rewriting what it manages, such as a timestamp in a status. Its
  `the target made … status writes in …` counts every status write, even one that changed
  nothing.
- G6's `the target repeated the failing request … times within … (timeouts.settle), where
  thresholds.errloop allows …` names the request your controller kept retrying.
  [Thresholds](targets.md#thresholds) says how `errloop` relates to `settle`.
- G7's `the … that op … (deleteManaged) deleted never came back …` means your controller did
  not recreate the object. Check that it watches the kind, and that the object carries the
  controller ownerReference that `Owns()` follows. List a kind your controller leaves deleted
  by design under [`notRecreated`](targets.md#deleted-objects).

## Restarts and crash loops

Once a settle wait has converged, reconciler-fuzzer restarts a controller that exits, as a kubelet would:
at once, then after 10s, doubling up to 5 minutes. Before that, an exit ends the invocation
with exit 2. The run prints a note for each exit, and quotes the line your controller wrote as
it stopped.

A settle wait does not converge while your controller waits to restart, nor before it has been
back for `stable` since it last started. Your controller is back once it requests a resource
outside leader election. reconciler-fuzzer takes a controller that reads a Lease with a get to elect a
leader, and counts it back only once it wins its lease. reconciler-fuzzer has no other sign.

- After a `restart` op, your controller has `settle` to come back, and `settle` past its return
  to converge.
- A controller that crashes again within `stable` of each return never converges, even where it
  wrote its converged state first. G4 reports it and quotes the last exit.
- After an exit during a fault, or while your controller recovers from one, reconciler-fuzzer restarts
  your controller and gives it `settle` past its return to converge.
- While a fault is active, only the first exit during each op gets `settle` past its return. A
  later exit during the op gets it only where the next op lands before your controller has had
  `settle` past its return. Otherwise the wait can end before your controller restarts. A wait
  can also end before a restarted controller has won its lease back.
- reconciler-fuzzer does not check a `checkpoint` or `end` property where a wait ends, or the teardown
  checks, before your controller could have converged: while it waits to restart, or before it
  has been back for `stable` since it last started. The run notes it.
- reconciler-fuzzer applies no op while your controller waits to restart after an exit during a fault. A
  controller that keeps crashing under a fault therefore fails G4 once the fault stops, or once
  the teardown clears it.
- G7 notes, rather than judges, a `deleteManaged` that comes before your controller is back
  from a restart. It also notes one where your controller exited, or waited to restart, during
  the op or its settle wait.

`--launch-arg --bug=12` makes the toy divide by its Widget's count once it has written its
status. `targets/toy-widget/sequences/b12.json` sets a count of 0, and the toy crashes after
each start:

```
run 1: the target exited during op 1 (update) with exit status 2 after writing "panic: runtime error: integer divide by zero [recovered, repanicked]"
run 1: the target exited during op 1 (update) with exit status 2 after writing "panic: runtime error: integer divide by zero [recovered, repanicked]"
run 1: P1 is not evaluated at the checkpoint after op 1 (update): the target was waiting to restart, so it may not yet have acted on what P1 reads
run 1: G4 the settle wait after op 1 (update) expired with no fault active: in 5.038s (timeouts.settle is 5s), ready held from 12ms on, but the target was waiting to restart; the target exited 2 times since it last converged, last with exit status 2 after writing "panic: runtime error: integer divide by zero [recovered, repanicked]"
  at 2026-09-24T00:57:57.490964784Z; 15 requests, the first get /api 200; 5 versions, the first toy.reconciler-fuzzer/v1/Widget widget; the target managed 0 objects of the kinds it declares
```

## Interrupts

SIGINT, SIGTERM, SIGHUP and a terminal's Ctrl-C all stop reconciler-fuzzer cleanly. reconciler-fuzzer abandons the
run under way, stops your controller and the control plane, and deletes the run's namespace.
`reconciler-fuzzer run` and `reconciler-fuzzer replay` name the unfinished run's directory. A run that failed before
the interrupt still says why. Then reconciler-fuzzer dies of the signal, a few seconds after it arrived. A
second signal kills reconciler-fuzzer at once and leaves those processes running, as SIGKILL does.

GitHub Actions cancels a job by sending the step's shell SIGINT and, 7.5s later, SIGTERM. The
shell passes neither on, so the CI recipe runs reconciler-fuzzer with `exec`.

## When reconciler-fuzzer exits 2

Exit 2 means reconciler-fuzzer could not test your controller, and the message says why. These are the
usual causes and what to change.

- `KUBEBUILDER_ASSETS` names the directory holding `etcd` and `kube-apiserver`. Install them as
  the [README](../README.md#install) shows, or point `--kubeconfig` at a cluster.
- A key target.yaml does not take fails with its line, as in `line 6: timeouts.setle is not a
  key; did you mean settle?`.
- `launch.binary` is relative to the directory you run reconciler-fuzzer in. `crds`, `sample` and
  `fixtures` are relative to target.yaml.
- A controller that stops before its first settle wait converges ends the invocation, whether a
  flag, a taken port or the first CR stopped it. reconciler-fuzzer quotes the line it wrote that says why:
  the line a panic opens with, the line above a flag's usage text, or the last line above any
  stack trace. `target.log` in the run directory holds the rest.
- If reconciler-fuzzer had created the CR and your controller had requested a resource before it stopped,
  the CR may have crashed it. The message then names the run's `sequence.json` for
  `reconciler-fuzzer replay`.
- A controller that binds a fixed port, such as a health probe on `:8081`, collides with a
  second invocation of itself. Give it a free port in `launch.args`, or with `--launch-arg`.
- reconciler-fuzzer exits 2, rather than reporting a find, when the deadline ends a run or stops reconciler-fuzzer
  before its last run.
