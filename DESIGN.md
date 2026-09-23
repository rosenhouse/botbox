# DESIGN.md — botbox

A black-box property-based and fault-injection harness for Kubernetes controllers.

This document is the governing design for the repo. Coding agents implement against it,
and the reviewers it spawns review against it. If the code and this document disagree, one of them is wrong and the PR must say
which. §15 records the decisions behind the design and the evidence for them.

---

## 1. Thesis

Most controller bugs are not logic errors in the happy path. They are failures to
reconcile correctly across partial progress, stale reads, missed events, API errors,
and restarts. Formal verification (see Anvil / Welder) can prove liveness properties
such as Eventually Stable Reconciliation (ESR), but requires rewriting the controller.

`botbox` tests the same class of properties against **unmodified** controllers by:

1. Generating random but valid sequences of operations on a custom resource.
2. Observing the controller only through the Kubernetes API (no instrumentation).
3. Injecting faults at the API boundary and by restarting the controller process.
4. Checking a small set of **generic invariants** that need no per-controller
   configuration, plus optional per-controller **properties**.
5. Shrinking any failing sequence to a minimal reproducer and emitting a report.

The controller is a black box. If it talks to an API server, it can be tested.

## 2. Non-goals

- botbox is not a formal verifier. It offers no proofs and no model checker.
- botbox is not a chaos platform for production clusters. It runs against test clusters
  only, envtest or kind.
- botbox does not test the Kubernetes control plane itself.
- botbox is not a mocking framework for reconciler unit tests. It observes only through
  the API server.
- botbox is not opinionated about controller framework. controller-runtime, kube-rs,
  client-go by hand and operator-sdk targets are all equally valid.
- botbox does not replace the target's admission webhook. It generates from the CRD
  schema, so rules that only a webhook enforces are constrained per target (§8.3).

## 3. Provenance

All fixtures, targets, and examples in this repo are either (a) upstream open-source
projects referenced by name and version, or (b) designs original to this repo. No
employer code, documentation, cluster topology, or customer configuration is an input
to this project. The toy target (§9) is deliberately generic and exists only to
exercise the harness. The adoption examples (§10, M4 and M7) drive cert-manager and
external-secrets, upstream open-source projects, unmodified and pinned by version.

## 4. Vocabulary

| Term | Meaning |
|---|---|
| **Target** | A controller under test plus its CRD(s) and how to launch it. |
| **Launcher** | How a target's process is started, stopped, and restarted. |
| **Proxy** | An HTTP reverse proxy between the target and the API server. Observes every request; injects faults. |
| **Observer** | Watches the cluster state directly (not via the proxy) and records object versions and events. |
| **Test cluster** | The API server a run executes against: an envtest control plane that botbox starts (default), or an existing cluster given by kubeconfig (§5.8). |
| **Primary CR** | The one custom resource that a sequence's CR ops act on. Declared by the target. |
| **Fixture** | An object botbox applies to the run namespace before op 0, such as an Issuer. Never mutated; not a managed object. |
| **Managed object** | An object of a kind the target declares it manages, in the run namespace, that neither botbox nor the cluster created (§6). |
| **Op** | One step in a test sequence. The ops on the primary CR are `Create`, `Update`, `Delete` and `Recreate`; the control ops are `Restart`, `Fault`, `Settle` and `DeleteManaged` (§5.4). A CR op may set `noSettle` to skip the Runner's implicit settle wait (§5.5). |
| **Sequence** | An ordered list of ops plus a seed. The unit of generation, replay, and shrinking. |
| **Invariant** | A generic check that applies to every target. IDs `G1..Gn`. |
| **Property** | A per-target check declared by the target. IDs `P1..Pn`. |
| **Checkpoint** | A point at which invariants and properties are evaluated: when a settle wait ends, whether converged or expired; after each `Restart`, `Fault` and `DeleteManaged` op once the following settle ends; where the teardown's recovery wait ends (§5.5); and once after the teardown deletion window. |
| **Run** | Execution of one sequence against a clean namespace. |
| **Report** | Machine- and human-readable output of a failing run. |

## 5. Architecture

```
                 ┌──────────────┐  ops (create/update/delete)  ┌──────────────┐
  Generator ───▶ │    Runner    │ ───────────────────────────▶ │  API server  │
  (rapid)        │              │ ◀────── Observer (watch) ─── │  (envtest /  │
                 │  restart/    │                              │   kind)      │
                 │  fault ops   │                              └──────▲───────┘
                 └──┬───────┬───┘                                     │
                    │       │ fault control                           │
                    ▼       ▼                                         │
            ┌───────────┐ ┌─────────┐   all target API traffic   ┌────┴────┐
            │ Launcher  │ │  Proxy  │ ◀───────────────────────── │ Target  │
            └───────────┘ └─────────┘ ──────────────────────────▶└─────────┘
                                │ request log
                                ▼
                          Invariant engine ──▶ Report
```

In envtest mode the test cluster also includes botbox's garbage-collector emulation
(§5.8). It writes to the API server directly, never through the proxy.

### 5.1 Launcher

```go
type Launcher interface {
    Start(ctx context.Context, kubeconfig string) error // kubeconfig points at the proxy
    Stop(ctx context.Context) error                     // graceful: SIGTERM, then SIGKILL after a grace period
    Restart(ctx context.Context) error                  // crash: SIGKILL, then Start
    Status() Status                    // is the target still running, and why it stopped if not
    Exited() <-chan struct{}           // closed once the running target has stopped
}
```

Implementations:

- `Binary` — the primary launcher and the only one required through M6. Exec a local
  binary. botbox writes a kubeconfig whose server is the proxy URL, exports it as
  `KUBECONFIG`, and substitutes `$KUBECONFIG` in `launch.args`. The target's stdout and
  stderr go to `target.log` in the run directory. `Restart` sends SIGKILL, waits for the
  process to be reaped, then execs again, so fixed ports and lock files are released.
  botbox does not probe the target for health; the settle wait after the first op absorbs
  startup.
- `InProcess` — deferred. It may return if envtest run time becomes the bottleneck (§14).
- `Image` — run a container image against a kind cluster, with the proxy in-cluster or
  reached by port-forward. Phase 2 (§10, M8).

### 5.2 Proxy

The proxy is an `httputil.ReverseProxy` in front of the test cluster. It listens on
`127.0.0.1` over plain HTTP, so the target's traffic is HTTP/1.1; client-go negotiates
HTTP/2 only over TLS. The proxy's upstream transport comes from the test cluster's
`rest.Config` via `rest.TransportFor`, so it carries whatever that cluster uses: a client
certificate on envtest, a token or exec credential on a kubeconfig cluster. The proxy
strips any inbound `Authorization` header.

Responsibilities:

- **Record** every request: verb, group/version/resource, namespace, name, status code,
  latency, timestamp. This log is the primary signal for G1.
- **Stream** long-lived watch responses without buffering (`FlushInterval = -1`).
- **Inject faults** according to an active `FaultSpec`:

```go
type FaultSpec struct {
    Match  RequestMatcher // verb, resource, name pattern, fraction
    Action FaultAction    // Error{code}, Delay{d}, Drop{}, DropWatchEvents{kinds}
    Until  Trigger        // op index, duration, or count of requests it applied to
}
```

Two details that matter for any client-go based target:

- An injected error replaces the upstream response: the proxy sets `Content-Type:
  application/json`, drops `Content-Length` and `Content-Encoding`, and writes a
  `metav1.Status` body. client-go picks its decoder from the response `Content-Type`, so
  this works for the typed clients that negotiate protobuf.
- Requests carrying an `Upgrade` header (exec, port-forward, websocket) are passed through
  unrecorded and never faulted. Controllers do not use them.

Watch-event dropping requires parsing the watch stream (chunked JSON, or length-delimited
protobuf frames for typed clients) and filtering events. This is the hardest fault and
lands in phase 2 (§10, M8). Until then, `DeleteManaged` ops simulate a missed event by
deleting an object behind the target's back.

### 5.3 Observer

The Observer runs independent informers on the real API server, not through the proxy,
for the target's CRD(s) and the resource kinds the target declares it manages. It records,
per object: resourceVersion history with timestamps, generation vs observedGeneration
where present, finalizers, ownerReferences, deletion timestamps, and the UID, on which
attribution (§6) and the garbage-collector emulation (§5.8) rely.

The Observer must never affect the target. It has its own credentials and never writes.
Deleting a managed object behind the target's back is a Runner control op
(`DeleteManaged`, §5.4), never an Observer action.

### 5.4 Generator

The generator is built on `pgregory.net/rapid` and produces a `Sequence`:

- Ops on the primary CR: `Create`, `Update` (field-level mutation), `Delete`, `Recreate`.
- Control ops: `Restart` (launcher), `Fault{FaultSpec}`, `Settle` (wait for convergence
  before continuing; used to make properties checkable mid-sequence), and
  `DeleteManaged{kind, index}` (delete one managed object behind the target's back, §7).
  The Runner issues `DeleteManaged` directly to the API server, as it does the CR ops.
- **Schema-driven mutation** from the CRD's OpenAPI v3 schema: numeric ranges, enums,
  string patterns, optional-field presence, list length, map size. Generic and works on
  any CRD.
- **Valid by the CRD's own rules.** Every create, recreate and update the generator draws
  passes the CRD as the API server judges the CR botbox wrote: defaults, value
  validations, list types and `x-kubernetes-validations` rules, transition rules included.
  The check does not see the status the controller writes, so a CRD rule that reads status
  can still refuse a draw. The generator runs the API server's own code for this (D@48). A
  create keeps a drawn field only if the CRD accepts it. A refused update is drawn again up
  to eight times, then becomes a `Settle`. Judging consumes no randomness. The sample must
  pass its CRD.
- Generation starts from the target's `sample` object. When the target declares
  `generate.mutate`, only those paths are mutated, and `generate.overlay` tightens the
  schema for a path (§8.3). An overlay keyword the generator does not read is a
  configuration error. For each path it may change, the generator draws up to 100 values
  into the sample until the CRD accepts one. A `generate.mutate` path is a configuration
  error too when the generator cannot draw a value for it, such as a set longer than its
  enum, or when the CRD refuses every value drawn. Without `generate.mutate`, botbox
  prints each spec path it leaves alone, and why: its schema says too little to draw
  from, such as an int-or-string, the generator cannot draw a value for it, or the CRD
  refuses every value drawn for it.
- **Hand-written generators** per target override schema-driven ones for fields with
  semantics the schema does not capture. In-repo targets only.

Every sequence is serializable to JSON (§7) so it can be replayed without rapid. A seed
names a sequence for one build of botbox and one target declaration. A golden test records
what the seeds the repository runs by number draw, so a change to a draw is deliberate
(D@55).

### 5.5 Runner

The Runner executes one sequence:

1. Create a fresh namespace. On a kubeconfig cluster, wait for what its controller
   manager adds to the namespace (§5.8). Exclude what the namespace holds from the
   managed objects (§6). Apply the target's fixtures. Start the target via the Launcher.
2. Apply ops in order. After each op that mutates the CR or a managed object, wait up to
   `T_settle` for convergence unless the op sets `noSettle`, and longer while the target is
   still owed time to recover from a fault that stopped (§6). The wait ends once the
   `Ready` predicate holds and neither the CR nor a managed object has changed for
   `T_stable`, so a checkpoint lands after the target's reaction, not before it. A wait
   that expires while no fault excuses it records a G4 violation. A fault excuses it while
   active, which is once the proxy has applied it and until the proxy stops (D36), and
   while the target is still owed time to recover from it (§6). A wait also ends
   where the target's process exits, and the Runner checks the target is running before it
   applies each op. A target that stopped ends the run as a harness error naming the op it
   was at (§11), because the ops behind it would run against nothing.
3. Evaluate invariants and properties at each checkpoint (§4). A run ends at its first
   violation. More than `N_objects` (default 500) managed objects in the namespace ends
   the run as a harness limit, reported as such rather than as a finding.
4. Tear down. Clear every active fault. If the target is still owed time to recover from
   a fault, which is so for a fault the teardown just cleared, wait for convergence as
   step 2 does and checkpoint where the wait ends. This recovery wait is judged as an op's
   wait is, so the caller's deadline can end it and no later step. A run that ended at a
   violation or a harness error gets none. Then wait `T_stable`, which is the last quiet
   window (§6). Delete the primary CR if it still exists and wait for the G3 window. A
   target that stopped cleaned nothing up, so the run ends as that harness error rather
   than at a verdict on the deletion. A run that ended at a harness error judges no
   deletion either, because its ops did not all run. Then
   force-remove any finalizer still present in the run namespace; the report notes each one
   (D37). G3 judged the deletion window, which closed before this. Delete every remaining
   object
   in the namespace that botbox or the target created. Stop the target if it was started
   for this run. Delete the namespace. Namespace names are never reused, so a namespace
   that never finishes terminating (envtest, §5.8) is harmless.

Cleanup between runs never restarts the API server, because rapid's shrinker re-invokes
the test function many times.

**Shrinking** is sequence-level: rapid drives it, and additionally a custom pass removes
ops one at a time and replays from clean state, keeping the shorter sequence if it still
fails. Fault ops shrink toward "no fault" and shorter durations. Shrinking stops at the
run deadline (§11).

### 5.6 Invariant engine

The engine consumes the Proxy request log and the Observer state history. Each invariant
is a pure function over those two inputs plus the target's declaration. Invariants are
**eventual**: they have a window and a stability requirement to absorb Kubernetes'
asynchrony. They are therefore blind to transient states; a transient state is caught by
a property (§8.1) or made permanent by a `Restart` or `Fault`. Flakiness is a bug in the
harness, not the target; tune windows, don't retry.

### 5.7 Report

A failing run emits `report.json` and `report.md` containing: the minimized sequence and
how many of its ops the run reached, the violated invariant or property with the concrete
evidence (request log excerpt, object version timeline), the target and versions, the
seed, and a one-line replay command. That command repeats the target, the kubeconfig and
every launch argument the run had, quoted so that `sh` and `zsh` read each word as
written. The run directory also holds recordings of the run (§11), so a report can be
re-examined without re-running. A readiness verdict and a
property violation also quote the state of the objects the target managed where it failed,
in a table of its own, bounded on its own, and say how many there were: a child the
target never created has no version to quote, and the count is what a report about a
missing one turns on (D39).
Every violation says how many entries it chose each excerpt from, because a report that
counted only what it was handed would claim every bounded excerpt was whole. It also says
the instant it judged, which aligns the two tables, and whose history a one-object
timeline is.

A report is a snapshot taken where the check failed, and the recordings beside it are
finalized when the run ends. A request still open at the snapshot, which a watch usually
is, therefore carries no latency in the report and its own in `requests.jsonl`.

### 5.8 Test cluster

botbox owns the API server a run executes against.

- **envtest** (default). botbox starts `kube-apiserver` and `etcd` from
  `KUBEBUILDER_ASSETS` (installed by `setup-envtest`) using
  `sigs.k8s.io/controller-runtime/pkg/envtest` inside `pkg/cluster`. This is the one
  harness package allowed to import controller-runtime (§11). It starts its own control
  plane even where `USE_EXISTING_CLUSTER` is set.
- **kubeconfig**. An existing cluster, normally kind. Used by `make test-kind` and, in
  phase 2, by `Image` targets. botbox installs the target's CRDs there, creating or
  replacing each one, and leaves them installed. The cluster runs
  `kube-controller-manager`, whose garbage collector replaces the emulation below. It also
  adds the `default` ServiceAccount and the `kube-root-ca.crt` ConfigMap to every
  namespace. A run waits up to 30 s for those of a kind it watches, and a missing one is a
  harness error. The cluster serves one botbox invocation at a time: a target that watches
  every namespace acts in another invocation's run namespace too, and each invocation
  would count that work as its own target's.

envtest runs only the API server and etcd. There is no `kube-controller-manager` and no
kubelet, so nothing garbage-collects owned objects, namespaces never finish terminating, no
default service accounts appear, no pods run, and no workload's status changes.
Consequences:

- **Garbage-collector emulation.** In envtest mode botbox runs a minimal collector over
  the run namespace: it deletes a managed object that has at least one ownerReference
  once every owner in its `ownerReferences` is gone; an object with none is never touched.
  Like kube's garbage collector, it maps a reference's apiVersion and kind through
  discovery, so any version the API server serves resolves. It then finds the owner by
  (group, kind, name) in the run namespace and compares the UID; a name match with a
  different UID counts as gone. An owner of a kind botbox does not watch, or named at a
  version the API server does not serve, is treated as live, so the emulator never deletes
  an object whose owners it cannot resolve. The run notes each unresolved reference once
  per object that carries it (§6). The emulator is watch-driven and deletes within 1 s of
  the owner's deletion event. It does not patch dangling ownerReferences off a dependent
  that still has a live owner. `blockOwnerDeletion`, foreground and orphan policies are
  not modelled. Its writes bypass the proxy and never count as target traffic. On a
  kubeconfig cluster it is off.
- **Self-cleanup.** The Runner empties the run namespace itself (§5.5, step 4).
- **No workloads.** A kind that runs Pods, and a claim Pods mount, keep the status they
  were created with: Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob,
  ReplicationController, Pod and PersistentVolumeClaim. No ReplicaSet or Pod appears for
  them. A `ready` that waits on that status never holds, and a target that requeues while
  it waits can hide a missed watch. When `manages` names one of these kinds, an envtest
  invocation says so once, before the control plane starts, and names `--kubeconfig` and
  kind. A G4 report repeats it among its notes. botbox does not emulate these controllers
  (D@35).
- **No finalizers that only `kube-controller-manager` removes.** botbox starts the API
  server with its garbage collector off, and with the `StorageObjectInUseProtection`
  admission plugin disabled beside envtest's own `ServiceAccount`. A delete therefore adds
  no `orphan` or `foregroundDeletion` finalizer, whatever its propagation policy, and the
  object goes at once. No PersistentVolumeClaim carries `kubernetes.io/pvc-protection`,
  and no PersistentVolume carries `kubernetes.io/pv-protection`. On a kubeconfig cluster a
  Job or ReplicationController deleted without a policy orphans its Pods, and a claim keeps
  its finalizer until no Pod uses it.
- **No admission webhooks.** See §8.3.

## 6. Generic invariants

All windows and thresholds are configurable per target. The defaults below are sized for
real targets; the toy target sets much shorter ones (§9).

| ID | Name | Statement | Signal |
|---|---|---|---|
| **G1** | Bounded reconciliation | Once the settle wait has ended, on convergence or at `T_settle` (default 30s) or later after a fault (§5.5), the target makes no further API request for `T_stable` (default 10s). Watches do not count, nor does any request to `coordination.k8s.io` leases, since leader election reads as well as writes, nor any request that names no resource, such as a health probe or a discovery read. | Proxy log |
| **G2** | No churn | Once converged under a stable spec, the primary CR, the set of managed objects and their resourceVersions do not change for `T_stable`. Status subresource writes that do not change content count as churn. A status write whose content is unchanged does not move resourceVersion, so it is counted from the proxy log. | Observer + proxy log |
| **G3** | Clean deletion | After deleting the CR with no faults active, every object the target manages for it is deleted and the CR's finalizers are cleared within `T_delete` (default 60s). Nothing the target manages remains. | Observer |
| **G4** | Convergence | Within `T_settle` after any spec change, and after faults stop within as long as they lasted plus `T_settle`, the target's `Ready` predicate holds with `T_stable` of quiet behind it (§5.5). This is ESR as a test. | Observer + target predicate |
| **G5** | Restart-stable | Restarting the target does not change converged state. The snapshots taken before and after a `Restart` are equal under the target's equality predicate. | Observer |
| **G6** | No error loop | The target does not make the same failing request (same verb/resource/name, 4xx/5xx) more than `N_errloop` (default 20) times within `T_settle` under a stable spec with no faults. A 409 Conflict on an `update` or a `patch` does not count. | Proxy log |

**The quiet window.** G1 and G2 judge the `T_stable` that follows a settle wait, which
ends where the run converged or where the wait gave up (§5.5). Measuring
it from the op instead leaves it unjudged for a target that converges quickly, because the
teardown begins one `T_stable` after the settle wait ends. The `T_stable` the teardown
waits before it deletes is a window of its own, closing at the teardown boundary (§5.5
step 4). A run judges one window per op whose settle wait it saw end, plus the teardown's:
a sequence whose last op does not settle has only the teardown's, and where the last op
did settle the two overlap, so traffic in the overlap breaks both. A window a later op or
a fault reaches into is not judged, where a fault's window runs from the first request the
proxy faulted with it to the request or the instant its trigger ran out (D36). The
teardown clears every fault before its window opens, so a fault it cleared did not reach
into it. It waits for convergence first only where the target is still owed time to
recover from a fault (§5.5 step 4), so a sequence ends with an op that settles.

**Recovery from faults.** A target backs off while its requests fail, and one that doubles
its delay retries within as long as it has been failing. Once faults stop, G4 therefore
gives the target as long as they lasted plus `T_settle`. The faults are those whose
windows reach past the last settle wait that converged, because a target that converged
had recovered. They lasted from the first request the proxy faulted with any of them, or
from that convergence if it came later, to the instant the last of them stopped. A settle
wait does not give up before that time has passed, and one that expired is excused only
while a fault is active or that time is still owed. A spec change made within that time is
judged at the later of the two deadlines. A settle wait that converged sooner ends that
time early.

**The teardown boundary.** No invariant window reaches past the instant the Runner
begins the teardown (§5.5 step 4), because from there on botbox is the one changing the
namespace and the attribution below no longer holds. A window that would close after it
is not judged, rather than judged early: judging early would hold the target to a shorter
window than §6 gives it, and where the boundary falls would depend on harness timing. G3
is the exception, since §5.5 step 4 opens its window deliberately, and §4's teardown
checkpoint still evaluates properties. A primary CR with a deletionTimestamp need not
satisfy `Ready`: it is being deleted, so G3 judges it, not G4.

**Attribution.** A managed object is any object of a declared managed kind in the run
namespace that neither botbox nor the cluster created. Fixtures and the primary CR are
botbox's. The cluster's are what the namespace holds before the fixtures and the target,
once §5.8's wait is over. Both are excluded by name, so an object the cluster recreates
stays excluded. The namespace is private to one run, since a kubeconfig cluster serves one
invocation at a time (§5.8). Everything else in it came from the target, except what a
cluster adds later: the optional selector leaves that out.
ownerReferences and the selector refine attribution to a particular CR; they are not
required for it.

**Deletion.** Owned children are removed by the cluster's garbage collector (real on kind,
emulated on envtest, §5.8). G3 therefore fails on orphans, meaning children with no
ownerReference to the CR, and on finalizers that never clear. The teardown watches the
namespace until it is clean or `T_delete` expires (§5.5 step 4). A namespace that came
clean satisfies G3 at that instant, which is how a target that cleans up promptly is
judged rather than left unjudged: the run stops watching long before `T_delete` is up. An
object a `DeleteManaged` op took inside the window is not cleanup: G3 notes it (D38).

**What the proxy cannot see.** G1 and G6 observe only requests that leave the target
process. Reads served from a client-side cache are invisible, so a reconcile loop that
makes no API calls is outside what botbox can detect.

**What G6 does not count.** A 409 Conflict on an `update` or a `patch` is the API
server's optimistic-concurrency contract: the target is meant to re-read and write again,
and controllers that share one object's status conflict constantly. A 409 on a `create`
is AlreadyExists, which says the object is there, so a target that keeps re-creating it is
looping and G6 counts it; so is a 409 on any other verb. G6 also counts a failing watch,
which G1 excludes: G1 ignores a watch because a watch that hangs is the target waiting,
while a watch that fails returns at once and repeating it is a loop.

**Notes.** A check that could not judge something records a note naming it: G3 for a
deletion whose deadline the run did not reach, that a fault reached into, or that botbox
took an object inside, G5 for a `Restart` missing a snapshot or with a change of botbox's
or a fault between its snapshots. The Runner carries the last checkpoint's notes out and
`botbox` prints them at the end of the run, because a check that was skipped otherwise
reads like one that passed. G5 also notes an `equalIgnore` path it could not follow
(§8.1), since it then compares a field the target meant it to skip. The Runner also notes
each ownerReference the collector could not resolve (§5.8), since the object that carries
it stays, and G3 would report it without saying why.

**Readiness.** G3 and G6 require nothing from the target except which resource kinds it
manages. G4 needs a `Ready` predicate. G1, G2 and G5 need none of their own, but they read
the settle wait, which the predicate ends. The default is
`has(status.observedGeneration) && status.observedGeneration == metadata.generation`, and
a target whose primary CR lacks that field must declare `ready` (§8.4).

**G5 evaluation.** G5 is evaluated once per `Restart`. The Runner snapshots whenever a
settle wait converges, implicit or explicit. A `Restart` is compared against the last
converged snapshot before it and the first converged snapshot after it. If either is
missing, G5 is not evaluated for that `Restart` and the report says so. The same holds
where botbox applied, between the two snapshots, an op that changed the CR or a managed
object: a `Create`, `Update`, `Delete`, `Recreate`, or a `DeleteManaged` that deleted
something. It also holds where a fault's window reaches between them. A difference could
then be the op's or the fault's. A `Settle`, another `Restart`, a `DeleteManaged` that
deleted nothing or a `Fault` that faulted no request leaves the comparison standing.
Snapshots are keyed by kind and name.

**G5 equality.** The default ignores exactly `metadata.resourceVersion`, `metadata.uid`,
`metadata.creationTimestamp`, `metadata.generation`, `metadata.managedFields`,
`status.conditions[*].lastTransitionTime`, and any ownerReference whose owner no longer
exists. G5 finds an owner by group, kind and name, whatever version the reference names,
and then by UID. It compares everything else, including labels, annotations, finalizers
and the remaining ownerReferences. A target excludes further paths with `equalIgnore`,
written in the form of the paths above (§8.1). Along an ignored path, a map or a list
left empty counts as absent, so an ignored annotation that only one side carries compares
equal. An item that `[*]` names stays even when left empty, so the items still count.

## 7. Sequence format

```json
{
  "seed": 8675309,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "...", "kind": "Widget", "spec": {"count": 3}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps", "fraction": 0.5}, "action": {"error": 500}, "until": {"op": 3}}},
    {"i": 2, "t": "update", "patch": {"spec": {"count": 5}}, "noSettle": true},
    {"i": 3, "t": "settle"},
    {"i": 4, "t": "restart"},
    {"i": 5, "t": "settle"},
    {"i": 6, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 7, "t": "delete"}
  ]
}
```

Details the example does not show:

- Any CR op may carry `"noSettle": true`, which skips the Runner's implicit settle wait.
- A sequence ends with an op that settles, or nothing judges the state it leaves behind
  (§6, D33). That rules out a trailing `noSettle`, `restart` or `fault`.
- G5 judges a `restart` only between two converged settle waits with no CR op, no
  `deleteManaged` that deleted something and no fault's window between them (§6). Put a
  `settle` op after a `restart`, and one before it unless the op before it settles.
- A fault may outlast the sequence. The teardown then clears it and waits for the target
  to recover (§5.5).
- Each `fault` op adds a fault of its own, even where its spec equals another's. The proxy
  tries faults in op order, the first that applies to a request wins, and each runs out on
  its own `until`.
- `update` applies `patch` as a JSON merge patch (RFC 7386).
- `recreate` is a delete, a wait for the object to disappear, and a create of `obj`.
- `deleteManaged` selects the i-th managed object of `kind`, ordered by creationTimestamp
  then name. The index is resolved at execution time and the chosen object is recorded by
  name in the report. An index that resolves to nothing is skipped and reported as a note,
  since a target that manages fewer objects than the sequence expected is behaving, not
  failing. A kind the target does not declare in `manages` is a configuration error.

`botbox replay --target target.yaml sequence.json` re-executes exactly this. Reports
embed the minimized sequence in this format.

## 8. Target contract

A target is declared in YAML. In-repo targets, including the toy, are declared the same
way. Go code can attach named hooks (§8.4) when CEL or the schema is not enough; no
phase-1 target needs one.

### 8.1 target.yaml

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
  has(status.conditions) && status.conditions.exists(c,
    c.type == "Ready" && c.status == "True"
    && has(c.observedGeneration) && c.observedGeneration == metadata.generation)
properties:                                   # optional per-target checks, IDs P1..Pn
  - id: P1
    description: A Ready Certificate's Secret exists.
    cel: >-
      !has(status.conditions)
      || !status.conditions.exists(c, c.type == "Ready" && c.status == "True")
      || managed.exists(o, o.kind == "Secret" && o.metadata.name == spec.secretName)
    when: checkpoint                          # always | checkpoint | end
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
    - --metrics-listen-address=127.0.0.1:0
timeouts:                                     # optional; defaults in §6
  settle: 30s                                 # stable has to be shorter than this
  stable: 10s
  delete: 60s
thresholds:                                   # optional; defaults in §6
  errloop: 20                                 # N_errloop for G6
```

A settle wait ends once the Ready predicate holds and nothing has changed for `stable`,
within `settle` (§5.5), so the target has `settle - stable` to react before the quiet
window has to open. A `stable` at least as wide as `settle` leaves it none, and every op
that writes then expires. Loading such a target is a configuration error rather than a run
that reports G4 against a target that did nothing wrong.

`manages` names kinds as `group/version/Kind`, with `v1/Kind` for the core group. An
optional `selector` (label selector) refines attribution (§6). Paths under `generate` are
dotted schema property names, which the CRD schema validates. A Go hook may replace the
equality predicate as `equal: go:<name>` (§8.4). A hook takes no `equalIgnore`, since
nothing would read it.

`equalIgnore` lists further paths G5 ignores (§6). A path joins keys with `.`. A key that
holds `.`, `[`, `]`, `"`, `*`, `/`, `:` or whitespace goes in brackets as a JSON string,
and `[*]` names every item of a list or value of a map:

```yaml
equalIgnore:
  - status.lastSyncTime
  - metadata.annotations["probe.example.com/started-at"]
  - status.conditions[*].lastHeartbeatTime
```

YAML gives `[`, `]`, `: ` and ` #` meanings of their own, so the list is written in block
style, and a path that starts with `[` or holds `: ` or ` #` goes in single quotes. The
loader refuses a malformed path, naming the offset. It refuses a list index such as `[0]`,
since a restart can reorder a list. It refuses a key that the dots split where it can
tell: a key outside brackets that holds `/`, such as the `io/name` of
`app.kubernetes.io/name`, and a path that goes more than one step below the `labels` or
`annotations` of any `metadata`, which map keys to strings. A key names nothing inside a
list, and only an object shows where a list is. G5 therefore notes a key that meets a list
and names the path with `[*]` in its place (§6).

A property's `when` says where it is evaluated: `always` on every Observer event before
the teardown boundary (§6), `checkpoint` at each checkpoint (§4), `end` at the last
checkpoint only.

cert-manager v1.21.2 binds its healthz server to `0.0.0.0:9403`. The one flag that moves
it, `--internal-healthz-listen-address`, is hidden, and upstream says the prefix and the
hiding are there to discourage overriding it and that the flag may be renamed or removed.
botbox does not build on it, so runs against this target are sequential. Its metrics
server does take a supported flag, and the ephemeral port above keeps it out of the way.

### 8.2 Go form

```go
type Target struct {
    Name, Version string
    CRDs          []string
    Primary       schema.GroupVersionKind
    Sample        *unstructured.Unstructured
    Fixtures      []*unstructured.Unstructured
    Manages       []schema.GroupVersionKind
    Selector      labels.Selector
    Ready         func(*unstructured.Unstructured) bool // compiled from `ready`, or a hook
    Equal         func(a, b Snapshot) bool              // §6 default plus `equalIgnore`, or a hook
    Properties    []Property
    Generate      GenerateSpec
    Launch        LaunchSpec
    Timeouts      Timeouts
}

type Property struct {
    ID, Description string
    Eval            func(cr *unstructured.Unstructured, managed []*unstructured.Unstructured) bool
    When            PropertyWhen // Always, Checkpoint, End
}

type Snapshot struct {
    GVK    schema.GroupVersionKind
    Name   string
    Object *unstructured.Unstructured
}
```

`pkg/target` loads the YAML into this struct. Everything downstream consumes the struct.
The Runner keys snapshots by kind and name, never by UID, so a recreated object compares
against its predecessor.

### 8.3 Generation constraints and admission webhooks

botbox generates from the CRD's OpenAPI v3 schema and keeps the rules the CRD states,
`x-kubernetes-validations` included, on the CR botbox writes. It cannot keep a rule that
reads the status the controller writes (§5.4). Phase 1 does not install the target's
admission webhooks. Rules that only a webhook enforces are therefore invisible to the
generator. For cert-manager these include: a Certificate needs at least one of
`commonName`, `dnsNames`, `ipAddresses`, `uris` or `emailAddresses`; `duration` must
parse as a Go duration; `renewBefore` must be shorter than `duration`; a `dnsNames` entry
must be a DNS name, which neither the CRD schema nor the API server checks, so the
overlay spells out an RFC 1123 label. The target keeps generation inside the valid subset
with `sample`, `generate.mutate` and `generate.overlay`. A generated spec that the target rejects or ignores because it
violates such a rule is a target-declaration bug, not a finding; the journal records each
rule that had to be encoded this way. A CR op the API server refuses ends the invocation as
a configuration error that names the run and the sequence file holding the op.

### 8.4 Predicates

`ready` and each property's `cel` are CEL expressions compiled with `cel-go`. Equality is
not CEL: it is the §6 default with the `equalIgnore` paths also ignored, or a Go hook.
JSONPath is not supported anywhere.

`ready` binds `metadata`, `spec` and `status` to the corresponding top-level fields of the
primary CR as dynamic maps; a missing field binds to an empty map. A property binds those
three plus `managed`, the list of managed objects as dynamic maps, each carrying
`apiVersion`, `kind`, `metadata` and the object's other top-level fields. The standard
macros (`exists`, `all`, `has`, `map`, `filter`) and the string extensions are available. At a checkpoint the harness reads `managed` from the API server, not from the
Observer cache, so cross-informer ordering cannot produce a false finding.

A compile error or a non-boolean result is a configuration error (exit code 2, §11), never
a finding. An expression is evaluated against objects that may not yet carry the fields it
reads, so it must guard optional fields with `has()`. An evaluation error while polling
for readiness means "not ready"; it becomes a G4 finding only if it persists past
`T_settle`, and the report quotes the CEL error. An evaluation error in a property is a
configuration error.

A Go hook is a function registered under a name in `pkg/target` and referenced as
`ready: go:<name>` or `equal: go:<name>`. Hooks exist for in-repo targets only.

## 9. Toy target: `Widget`

The toy exercises every invariant and proves that the harness catches known bugs. It is
deliberately boring. It builds as the binary `bin/toy-widget` and is declared in
`targets/toy-widget/target.yaml`; `--bug` is passed through `launch.args`.

- `Widget.spec.count` (int, 0–10, required). The controller ensures exactly `count`
  ConfigMaps named `<widget>-<i>` exist, owned by the Widget, each containing `index: i`.
- `Widget.status.ready` (int) holds the number of ConfigMaps present. The controller also
  sets `status.observedGeneration`.
- The Widget carries the finalizer `widget.botbox/cleanup`. On deletion the controller
  lists the children by ownerReference, deletes any that remain, and removes the finalizer
  when none remain. Children also carry ownerReferences, so the collector and the
  finalizer are two independent cleanup paths.
- The controller sets those ownerReferences **except** where a seeded bug says otherwise.
- `ready`: `has(status.observedGeneration) && status.observedGeneration ==
  metadata.generation && has(status.ready) && status.ready == spec.count`.
- `P1`, `when: checkpoint`: `status.ready` never exceeds the number of Widget-owned ConfigMaps
  present. In CEL, `!has(status.ready) || status.ready <= managed.filter(o, o.kind ==
  "ConfigMap").size()`.
- `timeouts: {settle: 5s, stable: 2s, delete: 10s}`. The toy converges in milliseconds.

### 9.1 Seeded bug catalog (`--bug=<id>`)

| ID | Bug | Class | Should trip |
|---|---|---|---|
| B1 | Writes `status.ready = count` before creating children, and holds that state for 3 s so P1 trips deterministically; the hold must satisfy `T_stable < hold < 2 × T_stable`, which also lands the children in the quiet window | intermediate-state | G1, G2, P1 |
| B2 | Uses `generateName` for children and never deletes surplus ones, so every reconcile adds duplicates | non-idempotent | G1, G2 |
| B3 | Omits the ownerReference on child `<widget>-0` and counts children by name, so it converges and the orphan surfaces on deletion | orphan | G3 |
| B4 | Reads `count` from `status.ready` instead of `spec.count` | stale-state | G4 |
| B5 | Treats NotFound on child Get as an error and requeues forever. The Get is an uncached read (`mgr.GetAPIReader()`), so the failing request reaches the proxy | error loop | G6, G1 |
| B6 | Writes a fresh `status.lastSyncTime` (microsecond precision, so consecutive writes differ) on every reconcile, so every write re-triggers the controller | churn | G1, G2 |
| B7 | Does not delete children on `count` decrease | scale-down | G4 |
| B8 | Does not `Own()` ConfigMaps, so a deleted child is never recreated. The toy's `ready` does not depend on the children, so G4 stays true | unobserved-state | P1 (at the checkpoint after a `DeleteManaged` op), G5 (after a `Restart` recreates the child) |
| B9 | Removes the finalizer on the first deletion reconcile, before deleting children, and omits ownerReferences on every child, so no path cleans up | cleanup-ordering | G3 |
| B10 | Writes status only from an in-memory flag set when it created children. After a `Restart` the flag is gone, so a later scale-down converges the children but leaves `status` stale (a scale-up creates a child and re-arms the flag) | intermediate-state | G4 |
| B11 | Believes a child is present from the moment it asks the API server to create it, and never asks again. The belief outlives whatever removed the child, so a refused create, a scale-down or a `DeleteManaged` leaves the toy one child short for good, with no error and no requeue | unconfirmed-write | G4 |

Three of the classes are Sieve's bug patterns (§13): intermediate-state, stale-state,
and unobserved-state. The other classes are this repo's own.

The matrix runs one `b<id>.json` sequence per bug, under that bug and again against the
toy with no bug, where no check may fire. No sequence carries a fault (§10, M3), so
B11's row scales down and back up: the belief outlives the child the toy itself deleted.
The unconfirmed write needs a fault, and §10 M6's acceptance test runs `b11-fault.json`
for it. B11 is the deterministic form of §5.6's "a transient state is made permanent by a
`Fault`", so that settle wait expires whatever the windows are. A `Restart` heals B11,
because the belief lives in the process. `fault.json`, the README's fault example, holds a
fault that outlasts the sequence. The envtest tier runs it, not the matrix: the toy with
no bug recovers once the teardown clears the fault, and B11 fails G4 there.

Acceptance for M3: a matrix in `docs/bug-matrix.md` showing which invariant or property
catches each bug, generated by CI, with no empty rows and no check firing on any sequence
against the toy with no bug. The README links to it.

## 10. Milestones

Each milestone ends with something runnable and a journal entry (§12). A milestone may
span several PRs.

**M0 — Scaffold.** Deliverables:

- Go module `github.com/rosenhouse/botbox` on Go 1.26.
- `LICENSE` (Apache-2.0).
- `Makefile` with `setup`, `test`, `test-envtest`, `fmt`, `vet`.
- `setup-envtest` pinned through `ENVTEST_K8S_VERSION` and `ENVTEST_INDEX_URL`.
- `pkg/cluster` starting and stopping an envtest control plane, with a unit test and an
  envtest-tagged smoke test that reads the server version, so the envtest tier is not
  empty.
- CI on PR running the unit and envtest tiers.
- A `.claude/settings.json` SessionStart hook that runs `make setup`, so a Claude Code
  web session can run envtest without manual steps.
- `README.md` in the §11 shape with a status line.
- `docs/journal.md`.
- One skill, `add-invariant`, documenting the procedure used from M3 on.

Acceptance: `make test-envtest` passes both in CI and in a fresh web session.

**M1 — Toy target.** `Widget` CRD and controller built as `bin/toy-widget` with `--bug`
and B1–B10 implemented behind it; `targets/toy-widget/target.yaml`. Plain envtest tests
for the happy path. Acceptance: `--bug=0` passes happy-path tests; each `--bug=N` is
reachable.

**M2 — Proxy, Observer, Binary launcher.** Reverse proxy with request log and streaming
watches; Observer with version history; `Binary` launcher; `target.yaml` loader with CEL
`ready` and properties; `pkg/cluster` extended with garbage-collector emulation.
Acceptance: an envtest test starts the cluster, the proxy and the toy binary, creates a
Widget from its sample, and asserts that the request log shows the reconcile and the
Observer shows the ConfigMaps.

**M3 — Generic invariants and Runner.** G1–G6 and declared properties implemented as pure
functions with tests on recorded fixtures. Runner executes hand-written JSON sequences
with `Create`, `Update`, `Delete`, `Recreate`, `Settle`, `Restart` and `DeleteManaged`
(the `Restart` op is a launcher call, so it lands here; faults do not). `botbox replay`.
Acceptance: every seeded bug of §9.1 with a fault-free sequence is caught by the
invariant or property that section names, and no check fires on that sequence against the
toy with no bug; `docs/bug-matrix.md` is generated by CI.

**M4 — Adoption: cert-manager.** `examples/cert-manager/` with `target.yaml`,
`crds/cert-manager.crds.yaml` (the release asset for the pinned version; the in-tree
`deploy/crds/*.yaml` are development-only), `issuer.yaml`, `certificate.yaml`,
hand-written sequences, `quickstart.sh`, and a `Makefile` that obtains the controller by
shallow-cloning the pinned tag and running `go build` (about 2 s to clone and 90 s to
build cold; cached in CI). On every PR, `make test-example` runs `quickstart.sh`, which
must pass, and then a negative control with `--enable-certificate-owner-ref=false`, which
must fail G3 naming the retained Secret. The negative control proves the harness observes
the target instead of passing vacuously. `README.md` is rewritten usage-first per §11 and
embeds `quickstart.sh`. Acceptance: the example job is green; the embedded quickstart is
byte-identical to the script, enforced by the embed test; the journal records what an
adopter has to supply.

**M5 — Generation and shrinking.** rapid-driven sequences; schema-driven mutation from
CRD OpenAPI with `generate.mutate` and `generate.overlay`; sequence-level shrinker;
`botbox run --runs --seed`. Acceptance: with `--bug=B2`, the harness finds and shrinks a
failure to ≤ 3 ops without a hand-written sequence; the cert-manager example switches to
generated runs, with fixed seeds on PRs and random seeds nightly.

**M6 — Faults and report.** FaultSpec injection (Error, Delay, Drop); `report.json` and
`report.md`. Acceptance: a fault makes the toy fail an invariant it passes without the
fault, and its report names the invariant, the minimized sequence and the evidence.

**M7 — Adoption: external-secrets.** `examples/external-secrets/` with `target.yaml`,
`crds/external-secrets.yaml` (the release asset for the pinned version, a whole install
manifest of which envtest keeps the 25 CRDs), `secretstore.yaml`, `externalsecret.yaml`,
hand-written sequences, `quickstart.sh`, and a `Makefile` that obtains the controller by
shallow-cloning the pinned tag and running `go build -tags fake` at the repository root,
where its main package lives (about 2 s to clone and 150 s to build cold; cached in CI).
The second adoption is against a controller unlike the first: it binds every port
ephemerally, so its runs may overlap; it carries no `observedGeneration`, so readiness
reads a version string; and no flag makes it orphan the Secret it manages, so the
negative control is a sequence whose CR sets `spec.target.creationPolicy: Orphan`. On
every PR, `make test-example-external-secrets` runs `quickstart.sh` and the pinned
sequences, which must pass, and then that control, which must fail G3. Acceptance:
`make test-example-external-secrets` passes on a clean checkout, the orphan control fails
G3 naming the Secret, and both are green in CI.

**M8 (phase 2) — Multi-cluster target.** A multi-cluster sync controller as `Binary`
target, forcing two-API-server envtest and cross-cluster faults; watch-event dropping in
the proxy; the `Image` launcher. Separate design addendum.

## 11. Repo conventions

- **Language and pins.** Go `1.26.0` in `go.mod` (cert-manager v1.21.2 and the
  Kubernetes 0.37 libraries require it); CI uses `go-version-file: go.mod`. Library pins
  live in `go.mod` only: `k8s.io/{api,apimachinery,client-go,apiextensions-apiserver,apiserver}`
  v0.37.x,
  `sigs.k8s.io/controller-runtime` v0.25.x, `pgregory.net/rapid` v1.3.x,
  `github.com/google/cel-go` v0.30.x. Tool and target pins live in one Makefile variable
  each: `ENVTEST_K8S_VERSION`, `SETUP_ENVTEST_VERSION`, `CONTROLLER_GEN_VERSION` (which
  also pins the envtest release index), `KIND_VERSION`, `KIND_NODE_IMAGE` (by digest),
  and one trio per adopted example:
  `CERT_MANAGER_VERSION` and `EXTERNAL_SECRETS_VERSION`, each with the `_COMMIT` the tag
  must name and the `_CRDS_SHA256` of its checked-in CRDs, so a moved tag or an edited
  asset fails rather than passing quietly. Values live in the Makefile only. Bumps are
  their own PRs, never mixed with features.
- **controller-runtime boundary.** Only `targets/toy-widget/` and `pkg/cluster` may
  import it. The rule covers the root module; the spike modules under `docs/spikes/` are
  separate and exempt. Everything else uses client-go and apimachinery.
- **Layout.** `cmd/botbox/`, `pkg/cluster`, `pkg/proxy`, `pkg/observe`,
  `pkg/invariant`, `pkg/generate`, `pkg/run`, `pkg/report`, `pkg/target`,
  `targets/toy-widget/`, `examples/cert-manager/`, `examples/external-secrets/`, `docs/`,
  and `bin/` for git-ignored build output.
- **CLI.** `botbox run --target <yaml> [--runs N] [--seed S] [--out DIR] [--deadline D] [--kubeconfig FILE] [--launch-arg ARG]... [<sequence.json>...]`;
  `botbox replay --target <yaml> [--out DIR] [--deadline D] [--kubeconfig FILE] [--launch-arg ARG]... <sequence.json>`;
  `botbox matrix --target <yaml> --sequences <dir> [--out FILE] [--deadline D] [--kubeconfig FILE] [--launch-arg ARG]...`;
  `botbox version`.
  `botbox run` draws its sequences or runs the ones named, never both, since `--runs`
  says how many to draw. `--deadline` defaults to 4m, and the shrinker stops there and
  reports the smallest failing sequence it found. `--launch-arg` appends to `launch.args`
  (repeatable; a later flag wins), which is how the bug matrix selects `--bug=N`.
  `--kubeconfig` selects an existing cluster instead of envtest and installs the target's
  CRDs there (§5.8); `KUBEBUILDER_ASSETS` locates the envtest binaries. Exit codes: 0, all runs
  passed; 1, an invariant or property failed and a report was written; 2, configuration or
  harness error, or a deadline that stopped the invocation before its last run.
- **Output.** `--out` defaults to `botbox-out/`. Each invocation writes
  `<out>/<timestamp>-<seed>/`, taking the next free name where a second invocation of one
  seed opens a directory in the same second. Each failing run writes `run-<n>/` under it
  with `report.json`, `report.md`, `sequence.json`, `requests.jsonl`, `objects.jsonl`,
  `target.log` and the `kubeconfig` the target was given, plus `sequence.shrunk.json`
  where the deadline ended the shrink pass before its result could be run there. Passing
  runs are not persisted.
- **Test tiers.** `make test` = unit, no API server. `make test-envtest` = envtest, under
  5 minutes on CI. `make test-example` and `make test-example-external-secrets` = the two
  adopted examples under envtest, each under 10 minutes on CI including obtaining the
  binary (cached). All four run on every PR. The `-nightly` target beside each example
  runs it on seeds botbox draws, with the negative control. `make test-kind` = the toy
  through `--kubeconfig` against a kind cluster it creates and deletes, nightly or on
  demand. It passes `b0.json` and fixed seeds, and fails B3 on G3 and B8 on P1 as its
  negative controls. It installs the pinned kind into `bin/` and needs Docker.
- **Network assumptions.** Every tier below kind reaches only `proxy.golang.org`,
  `sum.golang.org`, `github.com`, `raw.githubusercontent.com` and GitHub's release-asset
  hosts (`*.githubusercontent.com`). No tier assumes a container registry: the Claude Code
  web sandbox cannot reach `quay.io`, and the agent must be able to run every PR tier
  locally. External targets are obtained by shallow git clone at a tag plus `go build`, or
  as a GitHub release asset, and are pinned.
- **Lint.** `gofmt` and `go vet` run in CI. golangci-lint may be added in its own PR.
- **README.** Usage-first; internals live here and in `docs/`. Order: what botbox does
  (five lines); install; quickstart against cert-manager, then what the second example
  adds; writing `target.yaml` for your own controller; reading a report; a CI recipe for
  adopters; a one-line-per-invariant table linking to §6; a closing "Design and
  internals" link to this document and to
  `docs/bug-matrix.md`. A fenced block preceded by `<!-- embed: <path> -->` has content,
  excluding the two fence lines, byte-identical to that file including its trailing
  newline; `<path>` is relative to the repository root; `make test` enforces it.
- **PRs.** Every PR description, issue, review and comment a Claude session posts begins
  with the line `🤖 Created by Claude 🤖` (CLAUDE.md). The description then names the
  milestone and the invariant/property IDs it touches, and carries a "Design change"
  section whenever it edits this document.
- **No flaky-test retries in CI.** A flaky harness test is a P0 bug in the harness.
- **Seeds are always printed.** Every failure is reproducible from its sequence, and from
  its seed with the same botbox build and target declaration (§5.4).

## 12. How agents work in this repo

- **Coding agent (Claude Code):** implements one milestone sub-task per PR. Reads
  this document first. Works red/green: a failing test first, then the minimum code
  (CLAUDE.md). Does not change invariant definitions without a PR that edits §6 in the
  same change.
- **Reviewers (subagents):** before claiming a change is done, the coding agent spawns
  at least one adversarial reviewer with fresh context, using the personas CLAUDE.md
  lists. A reviewer reads the diff against §6, §8 and §11, citing the section it applies,
  and it may run the code: start a cluster, drive the binary, mutate a function and check
  that a test dies. It flags any import of controller-runtime outside the two places §11
  allows, a README embed block that differs from its file, a post whose first line is not
  `🤖 Created by Claude 🤖`, and a PR description that lacks the milestone, the IDs, or
  the "Design change" section when this document changed. Reviewers never merge.
- **Journal:** `docs/journal.md`, one entry per milestone, recording what the agents
  got right, what they got wrong, and which prompt, skill, or convention change fixed
  it. This is a first-class deliverable of the repo.

**Autonomy.** Decided with the maintainer on 2026-09-20 (§15, D15, D16, D24):

- The coding agent decides alone: package internals within the §11 layout; names; test
  structure; windows within the §6 defaults; CLI flags consistent with §11; patch and
  minor dependency bumps; README wording; journal entries; how a milestone splits into
  PRs.
- When this document is ambiguous or wrong, the coding agent proceeds under a stated
  assumption and amends this document in the same PR under a "Design change" heading.
  This includes invariant statements in §6.
- The coding agent stops and asks before: changing the license, the module path or other
  public names; editing `CLAUDE.md`; changing CI secrets or permissions; publishing a
  release; replacing an adoption example.
- **Merging.** The agent merges its own PR once CI is green on the head commit, with a
  squash merge. Before it merges, the PR description must record the adversarial reviews
  it ran, what they found and how each finding was addressed, so the trail is auditable
  after the fact. An unaddressed blocking finding stops the merge. It never force-pushes
  a shared branch.
- **Continuation.** Work does not wait for a human. A scheduled Routine starts a fresh
  session every hour. Each session reads this document and the journal, then looks for an
  open Claude PR. If one exists and another session touched it within the last two hours,
  the new session leaves that PR alone and starts the next independent sub-task on its own
  branch; it stands down only when no independent sub-task exists. Otherwise it drives
  that PR to merge, then takes the next incomplete milestone sub-task, and keeps going
  until the milestone is done or its context is spent.
- **Economy.** Spend is acceptable when it is well spent. A session delegates bounded
  sub-tasks and every adversarial review to subagents with fresh context, picks a cheaper
  model where the task allows (Sonnet for mechanical work, Opus for design-heavy work),
  and keeps its own coordinating context small.
- **Fallback.** cert-manager and external-secrets are both adopted (§10, M4 and M7), so
  an example that stops working under envtest for a reason on its own side leaves the
  other standing. The agent records the blocker in the journal and asks before dropping
  either.

## 13. Prior art

- Anvil / Welder (UIUC) verify controller liveness formally. ESR is the property G4
  approximates.
- Acto (UIUC, SOSP'23) tests operators from the CRD schema. It is the source of the
  generation approach and the oracle ideas.
- Sieve (UIUC, OSDI'22) injects controller faults by instrumenting the controller, and
  names the intermediate-state, stale-state and unobserved-state classes used in §9.1.
  `botbox` injects at the API boundary instead.
- rapid gives Go property-based testing with shrinking.
- envtest, kind and kwok provide test control planes.

## 14. Open questions

1. Does G2 need a per-target exemption list for controllers that write heartbeat-style
   status fields? external-secrets under `refreshPolicy: Periodic` is the first real
   target that writes them, and only at an interval longer than `T_stable` does G2 see
   them: a shorter one keeps the settle wait from converging, and G4 reports it first
   (D40). D40 answered this target with a target-side setting. The question stands for a
   controller that offers no such setting.
2. How is a cluster-scoped primary CR (ClusterIssuer-like) isolated per run?
3. Should a later phase run the target's admission webhook in envtest, so that generation
   can widen beyond `generate.mutate`?
4. Is `InProcess` worth reviving for speed once envtest run time is measured?

## 15. Decision log

D1–D17 were taken on 2026-09-20 with the
maintainer and rest on a spike in this sandbox, written up in
`docs/spikes/2026-09-20-cert-manager-envtest.md`: envtest 1.37.0, cert-manager v1.21.2
built from source and run as a black-box binary.

- **D1 Binary is the primary launcher; InProcess is deferred.** One code path from the
  toy target onward, real crash restarts, and no controller-runtime in the harness.
- **D2 target.yaml is canonical.** The Go interface is the loaded form; hooks are
  optional and in-repo only.
- **D3 CEL for `ready` and for properties.** cert-manager's Certificate carries
  `observedGeneration` only inside conditions, so readiness needs a cross-field comparison
  JSONPath cannot express.
- **D4 envtest is the default test cluster and botbox owns it.** `pkg/cluster` may
  import `controller-runtime/pkg/envtest`; re-implementing envtest would be waste.
- **D5 Garbage-collector emulation and self-cleanup on envtest.** Spike: 10 s after a
  Certificate was deleted, its Secret and CertificateRequest were still present despite
  ownerReferences, and the namespace stayed `Terminating`.
- **D6 Attribution by namespace.** Everything in the run namespace that botbox or a
  fixture did not create is the target's. Amended by D@36 for a cluster with a controller
  manager.
- **D7 G2 covers the set of managed objects; G5 is measured within one run.** G2 as
  first written missed new objects appearing (B2). G5 as first written compared two runs,
  which random `generateName` suffixes make incomparable.
- **D8 `primary`, `sample`, `fixtures`, `generate.mutate` and `generate.overlay` join the
  target contract.** A Certificate needs an Issuer to exist first and a valid `issuerRef`
  no schema can invent; webhook-only rules need an allowlist.
- **D9 The cert-manager adoption milestone is M4, before generation.** Adoptability
  problems surface while the black-box contract is still cheap to change.
- **D10 External targets are obtained by shallow clone plus `go build`.** cert-manager's
  `cmd/controller` is a nested Go module with a `replace` directive and no separate tag,
  so `go install` refuses it; no standalone binary is published; the sandbox cannot reach
  `quay.io`.
- **D11 Pins.** Go 1.26.0; Kubernetes libraries 0.37.0; envtest 1.37.0; controller-runtime
  0.25.1; rapid 1.3.0; cel-go 0.30.0; cert-manager v1.21.2. All current on the decision
  date.
- **D12 The `Restart` op lands in M3.** With the Binary launcher it is one call.
- **D13 No metrics scraping; `DeleteManaged` before watch-stream filtering; namespaced
  primary CRs only.** Scraping controller-runtime metrics would break the black box.
- **D14 B2 trips G1 and G2, not G3; B9 omits ownerReferences as well as ordering the
  finalizer wrongly.** Garbage collection removes owned duplicates and owned children, so
  G3 cannot see either bug otherwise.
- **D15 The agent merges its own PR once CI is green on the head commit and the PR
  records the adversarial reviews it ran; it amends this document in the same PR when
  needed; a human is asked only for the items listed in §12.** Chosen by the maintainer
  for unattended progress, and amended by D24.
- **D16 License Apache-2.0; continuation by scheduled sessions, continuous rather than
  throttled.** Chosen by the maintainer, with the instruction that spend is fine when well
  spent: subagents with fresh context and cheaper models where the task allows.
- **D17 The cert-manager example ships a negative control.** With
  `--enable-certificate-owner-ref=false` the Secret is retained by design (cert-manager
  documents this), and botbox must report it as G3 because the target declares Secrets as
  managed.
- **D18 `ENVTEST_INDEX_URL` pins the setup-envtest release index to a tagged
  controller-tools ref.** The default index tracks `HEAD`, so a pinned
  `ENVTEST_K8S_VERSION` alone does not make asset resolution reproducible.
- **D19 Properties are declarable in `target.yaml`.** Invariants are eventual and cannot
  see a transient state such as B1, so the toy needs P1 to catch it.
- **D20 G2 counts no-op status writes from the proxy log.** The API server short-circuits
  an update whose bytes are unchanged, so resourceVersion does not move and the Observer
  sees nothing; B6 is restated as a write that does change content.
- **D21 Equality is the §6 default plus `equalIgnore`, not a CEL expression.** A
  CEL expression would have to re-implement the per-path exemptions a path list states
  directly.
- **D22 The toy's P1 is evaluated at checkpoints.** Evaluated on every Observer event, a
  `DeleteManaged` op makes every controller violate it during its reaction time.
- **D23 A settle wait ends on quiescence, G2 covers the primary CR, thresholds are
  per target, the collector ignores ownerless objects, and `--launch-arg` selects a bug.**
  Without these, the correct controller fails P1 after `DeleteManaged`, B6 escapes G2, B5
  escapes G6 under controller-runtime's backoff, B3 and B9 lose their evidence, and the
  bug matrix needs ten target files.
- **D24 Every Claude GitHub workflow is removed, and the merge gate rests on
  the adversarial reviews instead.** They ran eleven times across two PRs and posted
  nothing, because the repository holds no API secret, leaving a red check on every
  push. A reviewer that only reads a diff is also weaker than one that starts a
  cluster and mutates the code, which is where every finding so far came from. The
  `@claude` mention workflow went with them, so nothing in CI needs an API secret.
- **D25 No invariant window reaches past the teardown boundary, and a CR under deletion
  need not be `Ready`.** The rule was in the code for G1, G2 and G6 and in no document.
  G4 and `always` properties lacked it, so the teardown's own delete made a correct
  target read as unconverged: one bug-matrix row flapped between runs.
- **D26 G1's and G2's quiet window follows the settle wait, not the op.** The window
  `[T_settle, T_settle + T_stable]` after the op closes past the teardown boundary
  whenever the target converges in less than `T_settle`, so neither invariant was judged
  for a run that converged: the cert-manager example evaluated neither. The window now
  opens where the settle wait ended, converged or expired, which is what G2 already said
  in English. B1 gains G1 and G2 in the bug matrix, because it reports its children ready,
  is judged converged on that, and only then creates them.
- **D27 G6 ignores a 409 Conflict on a write and counts a failing watch.** Six
  cert-manager controllers write one Certificate's status, which conflicted nine times in
  one run against `N_errloop` 20. A conflict is the API server telling the target to
  re-read, so counting it would fail a correct target. A watch is the other way round: G1
  ignores one because a watch that hangs is the target waiting, while a watch that fails
  returns at once and repeating it is a loop. envtest's flow control turned three watches
  away with 429 in the same run, well under the threshold.
- **D28 cert-manager's healthz port moves only under a hidden flag, so runs stay
  sequential.** `--internal-healthz-listen-address` exists, and upstream hides it to
  discourage overriding it and records that it may be renamed or removed. The spike tried
  `--healthz-listen-address`, got "unknown flag", and read that as no flag at all. botbox
  does not build on a flag upstream discourages.
- **D29 G6 ignores a 409 only on an `update` or a `patch`.** D27 excluded every write.
  A create that collides returns AlreadyExists, which is not a lost race: the object is
  there, and a controller that re-creates a fixed-name object forever is the error loop
  G6 exists to catch. Thirty identical failing creates in 1.5 s reported nothing. A patch
  keeps the exclusion: a server-side-apply conflict answers 409 as well, and the proxy
  does not keep the response body that would tell the two apart.
- **D30 G1 ignores every lease request, not only a lease write, and every request that
  names no resource.** §6 excluded all lease traffic and the code excluded only writes,
  so one leader-election read inside the quiet window failed a correct target, and leader
  election is on by default in nearly every controller. A health probe, a discovery read
  and the RESTMapper refresh that follows one are not reconciliation either; G6 still
  counts them, because a failing request repeated forever is a loop wherever it points.
- **D31 A clean run namespace satisfies G3, and a check that judges nothing leaves a
  note.** The teardown stops watching as soon as the namespace empties, which is always
  before `deletion + T_delete`, so G3 was skipped in every configuration that passed: the
  example's `issue.json` was observed to 31.3 s against a 71.3 s deadline, and only the
  negative control ever reached a verdict. Emptiness decides the deletion at the instant
  it is seen. G3 and G5 now record what they could not judge, the Runner carries the last
  checkpoint's notes out, and `botbox` prints them, because a skipped check reads like a
  passing one from outside.
- **D32 The teardown's `T_stable` is a quiet window of its own, and G1's statement
  follows the settle wait.** §5.5 step 4 already called that wait the last quiet window
  and nothing implemented it, so a sequence ending in a `noSettle`, a `restart` or a
  `fault` was judged on no window at all. A fault that outlives the last op takes that
  op's window with it, and the teardown clears every fault as its own window opens, so
  that shape is judged now too. The teardown does not settle before it waits, so a
  sequence that ends while the target is working is judged on that work; every sequence
  here ends with an op that settles, and a teardown that settles first, which would also
  give §6's "within `T_settle` after faults stop" somewhere to be measured, waits for M6.
  G1's row promised `T_settle` to fall quiet while the code judged the `T_stable` after
  the settle wait, which is shorter whenever the target converges early. The statement
  now says what the code does and what G2 already said: the settle wait is where botbox
  judges the target converged, and B1 keeps the G1 row D26 gave it, because it reports
  itself converged and only then creates its children.
- **D33 A sequence ends with an op that settles, and generation adds the settle waits
  that leave the ops it drew judged.** D32 left this a statement about the sequences
  in the repo, and the generator of M5 then drew sequences ending in a `noSettle`, a
  `restart` or nothing at all: roughly half of them, each judged on a window that opens
  while the target is still working, so a correct target failed G4. `Sequence.Validate`
  rejects that shape now, which also keeps the shrink pass from proposing it, and
  generation appends the settle a drawn op needs. A `noSettle` in the middle of a
  sequence stands: skipping that wait is what it is for.

  Generation also wraps every drawn `restart` in settle waits, because G5 compares the
  converged state either side of one, and a change on either side leaves G5 nothing to
  judge (D42). That is a rule for generation, not for the format, because a hand-written
  sequence may mean to restart and change the spec at once, as `b10.json` does.
  `Options.MaxOps` therefore bounds the ops a draw makes, not the sequence's length: at
  most two settles join each drawn op.
- **D34 G3 reads the teardown's own observation, not a checkpoint.** The Runner records a
  teardown checkpoint only for a run that found no violation, and `cleaned` derived G3's
  "the namespace emptied" from that checkpoint, so exactly the runs where a bug fired lost
  it. G3 then fell through to its last case and reported that the run ended before a
  deadline the namespace had come clean well inside: six of the eleven bug matrix rows,
  reading as passes until D31 made notes visible. The teardown now stamps
  `Timeline.Cleaned` from what `awaitClean` saw, and G3 reads that. A run that never came
  clean leaves it zero, which is the case G3 exists to report.
- **D39 A readiness verdict quotes the children in a table of their own.**
  G4's evidence was the primary CR's version history alone, so a finding about a managed
  object left the object out. A count answers the case the child was never created in,
  since an object that does not exist has no version any table can hold, but it says
  nothing about a child that exists and is wrong. A verdict therefore carries two pieces
  of evidence: a timeline, the CR's versions nearest it, and a state, one version of each
  object the target managed at the instant it judged. Each takes a bound of its own, so
  that neither crowds the other out: a CR that writes its status twenty times in the last
  second would otherwise crowd out the child the finding is about, and a target managing
  more objects than the bound would crowd out the CR its statement names. One table of
  both also left a gap in the timeline unexplained, since a reader could not tell which
  rows were a history and which a state (#24). The children are taken one from each kind in
  turn, newest first, so that a bound too small for them drops the oldest of each kind
  rather than every object of the kinds that sort last.
- **D35 A report quotes the evidence nearest the violation, and carries the notes.**
  §5.7 asks a report to quote the request log and the version timeline and does not say
  how much. A run makes thousands of requests, and a report nobody reads is worth
  nothing. The checks already bound what they quote, at the twenty entries nearest the
  violation (`pkg/invariant`), which are the ones that explain it; the report quotes what
  it is given and names the file that holds the rest. A violation the Runner raises itself
  quotes the same way, from the request log and the CR history it has in hand. The
  report's own bound is a backstop against a caller that bounds nothing. A violation also
  says how many entries it chose its excerpts from, because a report that counted only
  what it was handed would claim every bounded excerpt was whole. A check keeps a
  timeline's last entries, and the report says so rather than calling them the nearest: a
  violation is stamped where its evidence opens as often as where it closes. A state is
  bounded and counted on its own (D39). Quoted versions leave their object bodies to
  `objects.jsonl`. The report also carries what no check could judge, for D31's reason: a
  report that omits "G3 could not be judged" reads like one where G3 passed, and it is the
  artefact a human actually reads.
- **D38 G3 credits no cleanup botbox performed.** A `DeleteManaged` op deletes a managed
  object behind the target's back (§5.4). Inside a CR deletion's window that deletes the
  evidence: G3 asked whether the object was gone by the deadline and never asked who
  removed it, so `b3.json` with one `deleteManaged` op added turned a reported orphan into
  "every run passed". The Runner resolves the op to an object and hands it to the engine.
  G3 notes an object botbox took inside the window rather than counting it as cleaned. It
  is a note and not a violation, because the target still had until the deadline; an
  object botbox took after the deadline was still there at the deadline, which G3 already
  reports. The object is matched by UID, since one the run recreated carries the same name
  and belongs to the CR that came after.
- **D37 A forced finalizer is a note, not a reason to withhold G3.** §5.5 step 4 said each
  forced removal invalidates G3 for the run, and nothing implemented it. Implementing it
  literally would have thrown away true findings. The teardown stamps the end of the
  deletion window and takes G3's checkpoint before it forces anything, and no check reads a
  version recorded after that instant, so nothing botbox forced was ever credited to the
  target. What was missing is visibility: an object no check judges — a fixture, or a child
  born after the deletion — can hold a finalizer while G3 passes, and nothing said so. The
  run notes what it forced.
- **D36 A fault excuses the target over the window the proxy applied it in.** The Runner
  used to open a fault's window where it injected the spec and close it where it dropped
  the spec, and only an op-index trigger made it drop one. A `Count` or `For` trigger runs
  out inside the proxy, so the Runner went on excusing the target for the rest of the run:
  one fault op left G1 to G4 and G6 unjudged from then on, and a fault matching a resource
  the target never touches did the same. The proxy now reports what it did with each
  fault, and the window runs from the first request it faulted to the request or the
  instant the trigger ran out. A fault that matched no request has no window: it changed
  nothing, so it excuses nothing. This is the vacuity §9.1's control row exists to catch,
  one layer up: a run that reports nothing because nothing was judged.
- **D40 The external-secrets sample sets `refreshPolicy: OnChange`.** Under the CRD's
  `Periodic` the controller rewrites the ExternalSecret's status on every
  `refreshInterval`. Which check reports those writes depends on the interval. At `10s`,
  shorter than `T_stable`, the quiet window never closes: the run fails G4, not G2, with
  "the settle wait after op 0 (create) expired with no fault active", and the request log
  holds the ten status writes that kept it open. At an interval longer than `T_stable`
  the settle wait converges and G2 counts them as churn. `refreshInterval: 0s` stops the
  writes and convergence with them, because the controller then skips a spec change
  unless it renames the target Secret, so readiness never reaches the new generation.
  `OnChange` syncs when the generation, the labels or the annotations change and writes
  nothing in between. It costs coverage: nothing botbox does reaches the periodic refresh
  path. This is the first real target to raise §14 question 1, and the answer taken here
  is a target-side setting rather than a per-target G2 exemption list.
- **D41 external-secrets' negative control is a sequence, not a launch flag.**
  cert-manager orphans its Secret under `--enable-certificate-owner-ref=false`, which
  `--launch-arg` carries (D17). external-secrets decides ownership per CR, in
  `spec.target.creationPolicy`, so the control is `sequences/orphan.json`, whose create
  sets `Orphan`. The Secret then carries no ownerReference, the collector of §5.8 has
  nothing to resolve, and G3 names the Secret. `deletionPolicy` is not a second control:
  its default `Retain` leaves a Secret that still carries an ownerReference, which the
  collector removes.
- **D42 G5 judges a restart only where botbox changed nothing between its snapshots.**
  A `restart` does not settle, so in `b10.json` the first converged state after the
  restart follows the update to `count` 1. G5 blamed the restart for that update and
  failed the toy with no bug. G5 notes such a restart instead. A `restart` that settles
  was rejected: it would reverse D33, which lets a hand-written sequence restart and
  change the spec at once, and it would change what replaying a sequence does. G5 loses
  every restart that an op of botbox's confounds, since it cannot judge those soundly. It
  also loses one next to an update that changes nothing. A fault's window between the
  snapshots confounds a restart too, so G5 leaves that restart unjudged, as every other
  check ignores a fault's window. The shrink pass matches a candidate on the check alone,
  so a candidate without the settle after a restart no longer keeps a G5 the next op
  caused. `b0.json` settles after its restart, so the control row judges G5.
- **D43 The matrix runs every sequence against the toy with no bug too.** Only `b0.json`
  ran against the correct toy, so a sequence that fails a correct controller showed up
  nowhere. Each sequence now runs a second time without its bug, and a check that fires
  there fails the matrix. B0's one run is both. A `?` there does not fail it, because the
  check found nothing: G5 leaves the restart of `b10.json` unjudged. The cost is a second
  run per sequence.
- **D44 After faults stop, a target has as long as they lasted plus `T_settle` to
  recover, and the teardown waits for it.** The README's fault example failed the toy with
  no bug: its fault outlived the sequence, the teardown cleared it and opened the quiet
  window at once, and the toy's next create landed there as G1. B11 never recovers and
  passed it, because nothing judged the time after the fault. With the fault's count at
  10, the correct toy failed G4 2.47 s after the fault stopped, because its wait ended
  `T_settle` after the update. `T_settle` alone is too short: controller-runtime doubles
  the toy's delay from 5 ms, and after a fault that spanned three expired waits its next
  create came 5.4 s after the fault was cleared. A target that doubles its delay retries
  within as long as it has been failing, so G4 gives it that span as well, starting no
  earlier than its last convergence. The Runner's waits and the engine share the rule, and
  the teardown's recovery wait is judged as an op's wait is. This is the teardown that
  settles first that D32 deferred, for faults only.
- **D45 `equalIgnore` paths have a grammar that the loader checks.** A dotted path
  cannot name an annotation key, which holds dots, and the loader accepted a path that
  named nothing. `["key"]` quotes a key as a CEL map index does, and `[*]` follows §6 and
  kubectl's JSONPath. A child created after the operator started carries no annotation
  until a restart stamps one, so an empty map or list along an ignored path counts as
  absent. `generate` keeps dotted schema property names, which the schema already checks.
  A key that meets a list is a note and not a configuration error, because `equalIgnore`
  applies to every managed kind, and one kind's list can be another kind's map.
- **D46 An owner resolves through any served version, and an unresolved one is a run
  note.** An operator that migrates between API versions can name its owner at `v1alpha1`
  while the target declares `v1`. The collector keyed an owner by its apiVersion, so it
  kept that child, G3 fired on a correct controller, and only a warning on stderr said why.
  kube's garbage collector maps a reference's apiVersion and kind through discovery and
  then compares the UID. The collector now does the same with the run's RESTMapper. A
  version the API server does not serve stays unresolved, because kube's collector cannot
  resolve it either. G5 has no mapper, so it ignores the version. Each unresolved reference
  is a run note that names the object it keeps, because G3 reports that object and the
  report is what a reader acts on.
- **D47 The proxy holds each fault op's fault by identity.** The proxy matched a held
  fault by its spec, so a spent fault displaced an equal one, and the toy with no bug failed
  G4. Each fault has an ID, and a removed fault keeps its window. The Runner drops a fault
  once its window is closed, so a request faulted just before a removal stays in it.
- **D@55 A golden test pins what fixed seeds draw.** Draws come from rapid's
  `Example(seed)`, which rapid documents as fit only for examples and which promises nothing
  across versions. A draw also depends on the CRD schema, the sample, `generate` and
  `manages`. The Makefile, the README and the envtest tier rely on what particular seeds
  draw. `pkg/generate` records the draws of those seeds for the toy, cert-manager and
  external-secrets. A change to generation or to rapid that moves a draw fails until the
  test is rerun with `-update`. The README tells CI to pin botbox to a commit and to replay
  a failing `sequence.json` against the base branch.
- **D@48 Generation keeps the CRD's own rules, judged by the API server's code.** The
  generator read part of the OpenAPI schema and no `x-kubernetes-validations`. With the
  rule `self.maxUnavailable <= self.count`, 15 of 100 drawn sequences broke it, and the
  first refusal ended the invocation with exit 2 and no word of where the sequence was.
  Real CRDs carry such rules: the external-secrets bundle holds 50, and cert-manager's
  Issuer states "exactly one of" in CEL. The generator now judges each drawn CR op with
  `k8s.io/apiextensions-apiserver`, as the API server does, and undoes or redraws what the
  CRD refuses. Reimplementing the rules on cel-go alone was rejected. Kubernetes CEL types
  each rule against the structural schema, escapes property names, adds its own libraries
  and sees defaults first. Each divergence would undo a valid draw or pass one the API
  server refuses. The API server's code moves no selected module version, because
  controller-runtime v0.25.1 already requires `k8s.io/apiserver` and
  `k8s.io/component-base` at v0.37.0. Naming them in `go.mod` adds seven modules to the
  module graph, none of them built, and the binary grows by 4.6 MB. An
  envtest test applies 200 drawn sequences per target to a real API server with no
  controller, so the two cannot drift apart unseen on a CR without status. With a
  controller, the API server judges an update with the stored status copied in, which the
  generator never sees. A CRD rule that reads status can therefore still refuse a draw. A
  refusal still exits 2, since a rule botbox cannot keep belongs in the target declaration
  (§8.3). The message names the run and its `sequence.json`, and names a status rule as a
  possible cause. Undoing a refused field biases draws away from a rule's boundary: a
  sample that sets one of two exclusive fields never switches to the other. When the CRD
  refuses all 100 values drawn for a field into the sample, the field would never move, so
  New reports it instead of skipping it silently. Each value is judged against the sample
  alone, so New also reports a field that only another field's change makes valid. A
  sample that accepts the field fixes that. No sample accepts both of two exclusive
  fields, so `generate.mutate` names only one of them.
- **D@36 A kubeconfig cluster gets the CRDs, and a run excludes what the cluster put in its
  namespace.** On kind, `--kubeconfig` installed no CRDs, so the first run failed to
  resolve the primary kind. With the CRD applied by hand, the toy with no bug failed G3 on
  `kube-root-ca.crt`: `deleteManaged` took that ConfigMap as the oldest, and
  kube-controller-manager recreated it. D6 assumed that only botbox and the target write
  to the namespace. Kubernetes' e2e framework waits for the `default` ServiceAccount and
  `kube-root-ca.crt` in each test namespace, so a run waits for those of a kind it
  watches. It then excludes, by name, everything the namespace holds, before fixtures and
  the target. Without the wait, a root CA published late counts as the target's. Objects
  a cluster adds later stay attributed to the target, and `selector` leaves them out.
  botbox installs the CRDs with envtest's `InstallCRDs`, which creates or replaces each
  one and waits until it is served. It leaves them, because deleting a CRD deletes every
  object of that kind in the cluster. A kubeconfig cluster serves one invocation at a time:
  two invocations on one kind cluster failed the correct toy on G1 and G6, because each toy
  reconciled the other's Widget.
  envtest also read `USE_EXISTING_CLUSTER`, which pointed botbox's default mode, collector
  emulation and all, at whatever `KUBECONFIG` named. `cluster.Start` now turns that off.
  `make test-kind` runs on kind v0.33.0 and its default node image, Kubernetes 1.37.0.
- **D@35 botbox names the kinds envtest never moves, and envtest adds no finalizer that
  only the controller manager removes.** An operator whose `ready` waited on its Deployment's `availableReplicas`
  failed G4 on every envtest run, and botbox said nothing about why. envtest runs no
  `kube-controller-manager` and no kubelet, so the Deployment's status stayed empty and no
  ReplicaSet or Pod appeared. Under the default `observedGeneration` predicate the same
  operator passed, but it requeued every 5 s while it waited, and so recreated a child it
  never watched. A seeded missed-watch bug passed. botbox therefore names these kinds and
  points to kind, rather than suggest a weaker `ready`. The list is explicit and short: the
  kinds that run Pods, and PersistentVolumeClaim. It matches by group and kind, at any
  version. Running `kube-controller-manager` beside envtest was rejected: setup-envtest
  ships no such binary, and no Pod would run without a kubelet anyway. A claim also
  carried `kubernetes.io/pvc-protection`, which nothing on envtest removes, so a correctly
  owned claim failed G3. Removing that finalizer in the collector was rejected, because it
  adds code and timing, and on envtest no Pod ever uses a claim. botbox disables the
  admission plugin instead. A Job or a ReplicationController, which a delete orphans by
  default, and any foreground delete likewise carried a finalizer that only the garbage
  collector removes, so a correctly owned Job failed G3 too. botbox therefore also turns
  off the API server's garbage collector, which adds those finalizers.
