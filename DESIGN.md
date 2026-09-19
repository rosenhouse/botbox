# DESIGN.md — botbox

A black-box property-based and fault-injection harness for Kubernetes controllers.

This document is the governing design for the repo. Coding agents implement against it,
the PR review agent reviews against it, and the triage agent reads it when explaining a
failure. If the code and this document disagree, one of them is wrong and the PR must say which.

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

- Not a formal verifier. No proofs, no model checker.
- Not a chaos platform for production clusters. Test clusters only (envtest, kind).
- Not a conformance suite for the Kubernetes control plane itself.
- Not a mocking framework for unit tests of reconcilers. (In-process mode exists for
  speed, but observation is still via the API server.)
- Not opinionated about controller framework. controller-runtime, kube-rs, client-go
  by hand, and operator-sdk targets are all equally valid.

## 3. Provenance

All fixtures, targets, and examples in this repo are either (a) upstream open-source
projects referenced by name and version, or (b) designs original to this repo. No
employer code, documentation, cluster topology, or customer configuration is an input
to this project. The toy target (§9) is deliberately generic and exists only to
exercise the harness.

## 4. Vocabulary

| Term | Meaning |
|---|---|
| **Target** | A controller under test plus its CRD(s) and how to launch it. |
| **Launcher** | How a target's process is started, stopped, and restarted. |
| **Proxy** | An HTTP reverse proxy between the target and the API server. Observes every request; injects faults. |
| **Observer** | Watches the cluster state directly (not via the proxy) and records object versions and events. |
| **Op** | One step in a test sequence: create, update, delete, restart, or fault. |
| **Sequence** | An ordered list of ops plus a seed. The unit of generation, replay, and shrinking. |
| **Invariant** | A generic check that applies to every target. IDs `G1..Gn`. |
| **Property** | A per-target check declared by the target. IDs `P1..Pn`. |
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

### 5.1 Launcher

```go
type Launcher interface {
    Start(ctx context.Context, apiServer string) error // apiServer is the proxy URL
    Stop(ctx context.Context) error
    Restart(ctx context.Context) error
}
```

Implementations, in order of delivery:

- `InProcess` — a controller-runtime `manager.Manager` (or any `func(rest.Config) error`)
  run as a goroutine with a `rest.Config` pointed at the proxy. Restart cancels the
  context and starts a fresh instance. Fast; default for this repo's own targets.
- `Binary` — exec a local binary with `KUBECONFIG` pointed at the proxy. Restart is
  SIGKILL + re-exec. Required for adoptability against controllers not importable as Go.
- `Image` — run a container image against a kind cluster, proxy running in-cluster or
  via port-forward. Later milestone.

### 5.2 Proxy

An `httputil.ReverseProxy` in front of the API server. The target is given a plain
HTTP (localhost) or self-signed HTTPS endpoint; the proxy attaches the real credentials
upstream.

Responsibilities:

- **Record** every request: verb, group/version/resource, namespace, name, status code,
  latency, timestamp. This log is the primary signal for G1.
- **Stream** long-lived watch responses without buffering (`FlushInterval = -1`).
- **Inject faults** according to an active `FaultSpec`:

```go
type FaultSpec struct {
    Match  RequestMatcher // verb, resource, name pattern, fraction
    Action FaultAction    // Error{code}, Delay{d}, Drop{}, DropWatchEvents{kinds}
    Until  Trigger        // op index, duration, or count of matched requests
}
```

Watch-event dropping requires parsing the chunked JSON watch stream and filtering
events. This is the hardest fault; it is a M5 stretch goal and may ship after the rest.

The proxy is the adoptability story: it works against any controller with zero
changes to that controller.

### 5.3 Observer

Independent informers on the real API server (not through the proxy) for the target's
CRD(s) and any resource kinds the target declares it manages. Records, per object:
resourceVersion history with timestamps, generation vs observedGeneration where present,
finalizers, ownerReferences, deletion timestamps.

The Observer must never affect the target. It has its own credentials and never writes.

### 5.4 Generator

Built on `pgregory.net/rapid`. Produces a `Sequence`:

- Ops on the primary CR: `Create`, `Update` (field-level mutation), `Delete`, `Recreate`.
- Control ops: `Restart` (launcher), `Fault{FaultSpec}`, `Settle` (wait for convergence
  before continuing; used to make properties checkable mid-sequence).
- **Schema-driven mutation** from the CRD's OpenAPI v3 schema: numeric ranges, enums,
  string patterns, optional-field presence, list length. Generic and works on any CRD.
- **Hand-written generators** per target override schema-driven ones for fields with
  semantics the schema does not capture.

Every sequence is serializable to JSON (§7) so it can be replayed without rapid.

### 5.5 Runner

Executes one sequence:

1. Create a fresh namespace. Start the target via the Launcher (or reuse if the target
   is cluster-scoped and already running; declared by the target).
2. Apply ops in order. After each op that mutates the CR, wait up to `settleTimeout` for
   convergence (G4) unless the op is explicitly `NoSettle`.
3. Evaluate invariants and properties at each checkpoint and at end of sequence.
4. Tear down the namespace. Stop the target if it was started for this run.

Cleanup between runs is namespace deletion, not API server restart, because rapid's
shrinker re-invokes the test function many times.

**Shrinking** is sequence-level: rapid drives it, and additionally a custom pass removes
ops one at a time and replays from clean state, keeping the shorter sequence if it still
fails. Fault ops shrink toward "no fault" and shorter durations.

### 5.6 Invariant engine

Consumes the Proxy request log and the Observer state history. Each invariant is a pure
function over those two inputs plus the target's declaration. Invariants are
**eventual**: they have a window and a stability requirement to absorb Kubernetes'
asynchrony. Flakiness is a bug in the harness, not the target; tune windows, don't retry.

### 5.7 Report

A failing run emits `report.json` and `report.md` containing: the minimized sequence,
the violated invariant/property with the concrete evidence (request log excerpt, object
version timeline), the target and versions, the seed, and a one-line replay command.

The triage agent reads `report.md` and proposes a root-cause hypothesis. Its output is
advisory and goes in a PR comment, never in the report itself.

## 6. Generic invariants

All windows and thresholds are configurable per target; defaults below.

| ID | Name | Statement | Signal |
|---|---|---|---|
| **G1** | Bounded reconciliation | With the CR spec unchanged and no faults active, the target's non-watch API request rate falls to zero (excluding periodic resync and leader-election traffic) within `T_settle` (default 30s) and stays there for `T_stable` (default 10s). | Proxy log |
| **G2** | No churn | Once converged under a stable spec, no object the target manages changes resourceVersion for `T_stable`. Status subresource writes that do not change content count as churn. | Observer |
| **G3** | Clean deletion | After deleting the CR, every object the target manages for it is deleted and the CR's finalizers are cleared within `T_delete` (default 60s). Nothing the target manages remains. | Observer |
| **G4** | Convergence | Within `T_settle` after any spec change, and within `T_settle` after faults stop, the target's `Ready` predicate holds and `status.observedGeneration == metadata.generation` where the field exists. This is ESR as a test. | Observer + target predicate |
| **G5** | Restart-stable | Restarting the target at any point in a sequence does not change the eventually-converged state. Formally: converged state after `[ops..., Restart, Settle]` equals converged state after `[ops..., Settle]` under the target's equality predicate. | Observer |
| **G6** | No error loop | The target does not make the same failing request (same verb/resource/name, 4xx/5xx) more than `N_errloop` (default 20) times within `T_settle` under a stable spec with no faults. | Proxy log |

G1, G2, G3, and G6 require nothing from the target except which resource kinds it
manages (inferable from ownerReferences plus an optional declared label selector).
G4 and G5 need a `Ready` predicate; a default of `observedGeneration == generation`
is used when none is declared.

## 7. Sequence format

```json
{
  "seed": 8675309,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "...", "kind": "Widget", "spec": {"count": 3}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps", "fraction": 0.5}, "action": {"error": 500}, "until": {"op": 3}}},
    {"i": 2, "t": "update", "patch": {"spec": {"count": 5}}},
    {"i": 3, "t": "restart"},
    {"i": 4, "t": "settle"},
    {"i": 5, "t": "delete"}
  ]
}
```

`botbox replay sequence.json` re-executes exactly this. Reports embed the minimized
sequence in this format.

## 8. Target contract

A target is declared in Go for in-process use or in YAML for black-box use. Both
express the same fields.

```go
type Target interface {
    Name() string
    CRDs() []string                       // paths to CRD YAML
    Manages() []schema.GroupVersionKind   // resource kinds the target creates/owns
    Selector() labels.Selector            // optional: label selector for managed objects lacking ownerRefs
    Ready(obj *unstructured.Unstructured) bool   // convergence predicate; default observedGeneration==generation
    Equal(a, b ClusterSnapshot) bool      // optional: converged-state equality for G5; default deep-equal on spec+status of managed objects
    Generators() map[string]rapid.Generator // optional: per-field overrides
    Launcher() Launcher
}
```

```yaml
# target.yaml (black-box)
name: some-upstream-operator
crds: [crds/]
manages: [apps/v1/Deployment, v1/Service, v1/ConfigMap]
selector: app.kubernetes.io/managed-by=some-operator
ready: 'status.conditions.exists(c, c.type == "Ready" && c.status == "True")'   # CEL; JSONPath+value also accepted
launch:
  binary: ./bin/operator
  args: ["--kubeconfig", "$KUBECONFIG"]
```

Open question: CEL vs JSONPath for `ready`. Start with JSONPath+value (trivial), add CEL
when a real target needs it.

## 9. Toy target: `Widget`

Purpose: exercise every invariant and prove the harness catches known bugs. Deliberately
boring.

- `Widget.spec.count` (int, 0–10). The controller ensures exactly `count` ConfigMaps
  named `<widget>-<i>` exist, owned by the Widget, each containing `index: i`.
- `Widget.status.ready` (int) = number of ConfigMaps present; `status.observedGeneration`.
- Finalizer `widget.botbox/cleanup` on the Widget; removed after children are gone.
- Uses ownerReferences on children **except** where a seeded bug says otherwise.

### 9.1 Seeded bug catalog (`--bug=<id>`)

| ID | Bug | Class (Sieve taxonomy) | Should trip |
|---|---|---|---|
| B1 | Writes `status.ready = count` before creating children | intermediate-state | G4 |
| B2 | Uses `generateName` for children; re-reconcile creates duplicates | non-idempotent | G2, G3 |
| B3 | Omits ownerReference on child `<widget>-0` | orphan | G3 |
| B4 | Reads `count` from `status.ready` instead of `spec.count` | stale-state | G4 |
| B5 | Treats NotFound on child Get as an error and requeues forever | error loop | G6, G1 |
| B6 | Updates status on every reconcile even when unchanged | churn | G2 |
| B7 | Does not delete children on `count` decrease | scale-down | G4 |
| B8 | Does not `Own()` ConfigMaps, so a deleted child is never recreated | unobserved-state | G4 (after Observer deletes a child) |
| B9 | Removes finalizer before deleting children | intermediate-state | G3 |
| B10 | Creates children then crashes before status write (only triggers under Restart op) | intermediate-state | G4, G5 |

Acceptance for M3 and M5: a matrix in `README.md` showing which invariant catches each
bug, generated by CI, with no empty rows.

## 10. Milestones

Each milestone ends with something runnable and a journal entry (§12).

**M0 — Scaffold.** Go module, `setup-envtest`, `make test` running envtest, CI on PR.
`DESIGN.md` (this), `CLAUDE.md`, one skill `add-invariant`. Claude Code PR review
against this document; failure-triage workflow posting a comment. Acceptance: a trivial
PR gets a review comment that cites a section of this doc.

**M1 — Toy target.** `Widget` CRD and controller with `--bug` flag and B1–B10
implemented behind it. Plain envtest tests for the happy path. Acceptance: `--bug=0`
passes happy-path tests; each `--bug=N` is reachable.

**M2 — Proxy and Observer.** Reverse proxy with request log and streaming watches;
Observer with version history. `InProcess` launcher. Acceptance: run the toy target
through the proxy, dump the request log, see reconciles.

**M3 — Generic invariants.** G1–G6 implemented as pure functions with tests on recorded
fixtures. Runner executes hand-written sequences. Acceptance: every seeded bug that does
not need faults or restarts (B1–B9) is caught by the invariant in §9.1; the bug matrix
is generated by CI.

**M4 — Generation and shrinking.** rapid-driven sequences; schema-driven mutation from
CRD OpenAPI; JSON sequence format; `replay` command; sequence-level shrinker.
Acceptance: with `--bug=B2`, the harness finds and shrinks a failure to ≤ 3 ops without
a hand-written sequence.

**M5 — Faults and report.** FaultSpec injection (Error, Delay, Drop); `Restart` op;
`report.json` and `report.md`; triage agent reads the report. B10 caught. Watch-event
dropping is a stretch. Acceptance: full bug matrix has no empty rows; a report from a
seeded bug gets a triage comment with a correct root cause.

**M6 (phase 2) — Second target.** A multi-cluster sync controller as `Binary` target,
forcing two-API-server envtest and cross-cluster faults. Separate design addendum.

## 11. Repo conventions

- Go, latest stable. `controller-runtime` for the toy target and `InProcess` launcher
  only; the harness core must not depend on controller-runtime types beyond `rest.Config`.
- Layout: `cmd/botbox/`, `pkg/proxy`, `pkg/observe`, `pkg/invariant`, `pkg/generate`,
  `pkg/run`, `pkg/report`, `pkg/target`, `targets/toy-widget/`, `docs/`.
- Test tiers: `go test ./...` = unit (no API server); `make test-envtest` = envtest;
  `make test-kind` = kind, nightly or on demand. Envtest tests must pass in under 5
  minutes total on CI.
- Every PR description names the milestone and the invariant/property IDs it touches.
- No flaky-test retries in CI. A flaky harness test is a P0 bug in the harness.
- Seeds are always printed. Every failure is reproducible from seed + sequence.

## 12. How agents work in this repo

- **Coding agent (Claude Code):** implements one milestone or sub-task per PR. Reads
  this document first. Does not change invariant definitions without a PR that edits §6
  in the same change.
- **Review agent (PR workflow):** checks the PR against §6, §8, §11. Must cite the
  section it is applying. Flags any new dependency of `pkg/` on controller-runtime.
- **Triage agent (failure workflow):** reads `report.md` and the failing test output,
  proposes one root-cause hypothesis and one next experiment. Never edits code.
- **Journal:** `docs/journal.md`, one entry per milestone, recording what the agents
  got right, what they got wrong, and which prompt, skill, or convention change fixed
  it. This is a first-class deliverable of the repo.

## 13. Prior art

- Anvil / Welder (UIUC): formal liveness verification of controllers; ESR is the
  property G4 approximates.
- Acto (UIUC, SOSP'23): schema-driven operator testing; source of the generation
  approach and oracle ideas.
- Sieve (UIUC, OSDI'22): controller fault injection via instrumentation; source of the
  bug taxonomy in §9.1. `botbox` differs by injecting at the API boundary instead.
- rapid: Go property-based testing with shrinking.
- envtest, kind, kwok: test control planes.

## 14. Open questions

1. CEL vs JSONPath for black-box predicates (§8).
2. Whether to also scrape controller-runtime metrics for G1 when available, as a
   cross-check on the proxy log.
3. Watch-event dropping: parse the stream in the proxy, or simulate by deleting objects
   behind the target's back via the Observer? (The latter is simpler and may be enough.)
4. Cluster-scoped targets and namespace-per-run: how to isolate.
