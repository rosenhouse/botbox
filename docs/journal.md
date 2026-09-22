# Journal

One entry per milestone (DESIGN.md §12).

## M0 — 2026-09-20

This entry covers the design-decision session that preceded the scaffold, not the
scaffold PR itself.

### Right

A spike ran cert-manager
v1.21.2 unmodified against envtest 1.37.0 with no webhook installed. The Certificate
converged in 2.3 seconds. It showed zero churn. It was unchanged after a SIGKILL
restart. See `docs/spikes/2026-09-20-cert-manager-envtest.md`.

### Wrong in the first draft

- The design assumed envtest behaves like a cluster. envtest has no
  kube-controller-manager. ownerReference garbage collection never runs, and
  namespaces never finish terminating. This broke G3 and the original cleanup plan.
- The design delivered the `InProcess` launcher first. That choice would have delayed
  the black-box path that every real target needs.
- The design left `ready` as JSONPath-or-CEL. cert-manager's readiness needs a
  cross-field comparison that only CEL can express; JSONPath cannot.
- The design assumed a third-party controller could be fetched as a Go module or a
  binary. cert-manager's controller is an untagged nested module with a `replace`
  directive. It must be cloned and built instead.

### What fixed it

D1–D21 (§15) record the decisions. They added §5.8 on the test cluster, §8.3 on
webhook-only rules, the network assumptions in §11, and the autonomy rules in §12.

### Process notes

The sandbox could
reach `proxy.golang.org` and `github.com` but not `quay.io`. That gap drove the
"clone and build" rule for external targets.

M0 outcome (PR #3): a design review found seven blocking gaps and a maintainer review
two more; an exploratory test of the scaffold found none that blocked. All were fixed in
the PR before merge. The Claude Review workflow could not run on the PR that introduced
it, so the next PR is its first live test.

## M1 — 2026-09-20

### Right

The toy converged in milliseconds and survived SIGKILL at the earliest reachable point.
The CRD schema rejected every invalid count server-side. Every seeded-bug branch has a
test that fails when the branch is removed; a review still found one dead function.

### Wrong in the first draft

- The first deletion test ran at count 0, so the finalizer's child deletion was never
  exercised. A maintainer review caught it.
- `Status().Update` from a cached Widget produced 409 conflicts on most scale changes.
  Every Widget write is now a merge patch.
- B6 with `metav1.Time` stopped churning after one second, because consecutive writes
  were identical. The field is `MicroTime`.
- B3 as written never converged, so G4 would have caught it instead of G3. It now counts
  children by name.
- B2 was unobservable unless it also kept surplus children.
- P1 evaluated on every Observer event fails every controller after a `DeleteManaged`
  op (D22).
- Four catalog rows named a check that could not fire: B5 needs a per-target `errloop`
  threshold, B6 needs G2 to cover the primary CR, B8 is caught by P1 and G5 rather than
  G4, and B10 only after a scale-down. A settle wait now requires quiescence, so a
  checkpoint lands after the target's reaction.

### What fixed it

§9.1 rows B2, B3 and B6 say what the bugs do; D22 moves P1 to checkpoints. The CLAUDE.md
review personas ran as subagents; the exploratory tester drove the binary against envtest
with kubectl and found the 409s.

### Process notes

The Claude Review workflow cannot run without a repository API secret, so PRs #3 and #4
needed a human merge. Routines created from a session get no GitHub tools, so the
continuation ran in the session itself. Every Claude workflow was removed at the end of the
day (D24): they had posted nothing in eleven runs, and the reviews that found the bugs
were subagents that ran the code. CI now needs no API secret at all.

M1 outcome: PR #4.

## M2 — 2026-09-20

### Right

Five packages were written in parallel by subagents with fresh context, against
DESIGN.md alone, and wired together on the first try. The seams the design named held:
the proxy's request log, the Observer's history, the target contract and the launcher
met where §5 said they would.

### Wrong in the first draft

- The proxy completed a request's record after the client already had its response, so
  the envtest assertions raced it. Four local runs passed; CI caught it. `Log` now
  states the contract and the assertions wait.
- The collector, given only the managed kinds, is a no-op for the ordinary ownership
  shape: the children's owner is the primary CR, which `manages` does not list. It
  watches the primary kind too.
- Inferring envtest from a nil config would disable the collector on every run after the
  first, because §5.5 reuses one control plane. The mode is explicit.

### What fixed it

Each package was mutation-checked before it was committed: 24 for the Observer, 27 for
the proxy, 30 for the loader and launcher, 21 for the collector, 11 for the harness.
Every one was killed by a named test.

### Process notes

Five agents ran at once on non-overlapping directories, with one owning go.mod. The only
cross-package collision was a Snapshot type two packages would have defined; naming the
owner in a message cost one line. Three packages each build their own RESTMapper, and
two keep their own copy of the watched-kind list. Both are worth sharing in M3.

M2 outcome: PR #4.

## M3 — 2026-09-20

### Right

G1 to G6 are pure functions over the proxy log and the Observer history, so every
invariant has fixture tests that need no cluster. The bug matrix then runs the real
thing: one envtest run per seeded bug, regenerated by CI, and every bug trips the check
§9.1 names.

### Wrong in the first draft

- A settle wait that ended on a timer put the checkpoint before the controller had
  reacted, so a correct controller failed P1 after `DeleteManaged`.
- Thresholds were global, so B5 escaped G6 under controller-runtime's backoff.
- G2 covered only the managed objects, so B6, which churns the Widget's own status,
  escaped it.
- The emulated collector deleted ownerless objects, which is exactly the evidence B3 and
  B9 leave behind.
- Selecting a seeded bug would have needed ten target files, one per `--bug` value.
- The teardown boundary was in the code for G1, G2 and G6 and in no document. G4 and
  `always` properties lacked it, so the teardown's own delete read as a failure to
  converge, and one matrix row flapped between runs.

### What fixed it

D23 records the first five: a settle wait ends on quiescence, thresholds are per target,
G2 covers the primary CR, the collector ignores ownerless objects, and `--launch-arg`
appends to `launch.args`. D25 states the teardown boundary for every check.

### Process notes

The flapping row was the only symptom of a rule that lived in the code and in no
document. Per-check unit tests could not see it, because each check agreed with itself.
The duplication M2 flagged is still here: three packages each build their own RESTMapper.

M3 outcome: PR #4.

## M4 — 2026-09-20

### Right

cert-manager v1.21.2 runs unmodified, and nothing in the example is patched into it. The
negative control fails on cert-manager's own documented behaviour rather than on a seeded
bug: with `--enable-certificate-owner-ref=false` it leaves the issued Secret behind, and
G3 names that Secret.

### Wrong in the first draft

`make assets-path` echoed its own install recipe, so on a checkout without
`bin/setup-envtest` the quickstart captured two lines into `KUBEBUILDER_ASSETS` and
envtest could not start. CI would have hit it on the first cache miss. The recipe is
silent now.

### What an adopter supplies

An adopter fills in six keys, all of them in `examples/cert-manager/target.yaml`.

- `crds` names the CRDs the controller serves. It points at the release asset, because
  the in-tree copies are development-only.
- `sample` is one valid primary CR. A Certificate must name an identity, a rule only the
  webhook states (§8.3), so the schema alone cannot produce one.
- `fixtures` lists what the CR depends on. A Certificate needs an Issuer, and no schema
  can invent a valid `issuerRef`.
- `manages` lists the kinds the controller creates. Attribution needs nothing further.
- `ready` is a CEL expression. cert-manager keeps `observedGeneration` inside each
  condition, so readiness is a cross-field comparison.
- `launch` gives the binary and its flags.

No Go code was written against cert-manager.

### Findings

The example found two harness bugs that the toy target never showed.

- G1 and G2 were never evaluated for a target that converges. Their window closed
  `T_settle + T_stable` after the last op, which is past the teardown boundary (D25), so
  a passing run judged neither.
- G6 counted 409 Conflict. A conflict is optimistic concurrency working, and a healthy
  controller produces them.

### Process notes

Adopting a real controller cost two constraints in the target file. cert-manager binds its
healthz server to `0.0.0.0:9403` and hides the flag that moves it, so runs against this
target are sequential. `renewBefore` must be shorter than `duration`, a webhook-only rule,
so the target never mutates `renewBefore` and pins `duration` to an enum.

The embed check of §11 lives at the repository root, because it scans every Markdown file
and no package owns it.

### What fixed it

D26 anchors G1's and G2's quiet window to the end of the settle wait, so a target that
converges is judged at all. D27 stops G6 counting a 409 Conflict on a write. D28 records
that cert-manager's healthz port moves only under a flag upstream hides, so runs against
it stay sequential.

M4 outcome: `make test-example` and the embed check run on every pull request.

## M5 — 2026-09-21

### Right

The generator reads values out of the CRD's OpenAPI schema, and where the schema says too
little it refuses to guess rather than inventing a value: naming such a path in
`generate.mutate` is a configuration error, not a silent no-op. The shrinker keeps a
candidate only while it fails with the same check ID, so a shorter sequence that trips a
different check is reported as the different bug it is, and the minimized sequence is
re-run so the run directory's evidence is of the sequence botbox prints.

### Wrong in the first draft

- The M5 acceptance test was vacuous. It pinned seed 8675309 and asserted that with
  `--bug=2` the harness found a failure and shrank it, which it did. The same sequence
  failed identically with `--bug=0`, so the failure was the harness's and nothing in the
  test could tell the difference. The maintainer was told the acceptance was proved.
- A drawn sequence could end on a `restart` or a `noSettle` op. The teardown does not wait
  for convergence, so its quiet window then measured work still in flight. 26 of the first
  60 cert-manager seeds drew such a sequence, and each of the nine that ran failed G1 on
  the Issuer fixture's own status update, shrinking to the empty sequence.
- The first fix of that was incomplete. `checkpointed` skipped the settle after a `restart`
  whenever the next drawn op settled, so G5 compared the converged state before the restart
  against the state after the next spec change and blamed the restart for it. A second
  review sampled 200 toy seeds, found 16 of that shape with 8 failing a correct toy, and 5
  more that spent a restart with no converged snapshot before it.
- The shrinker briefly compared a candidate's violation statement as well as its ID,
  reasoned from the Runner's G4, whose statement carries no op index. The Engine's
  statements come from the invariants themselves, and G5, G1 and G2 all embed the op index,
  G1 and G2 a request count as well, so every candidate that removed an op before the
  failing one was rejected.
- A generated `deleteManaged` ended the invocation where its index resolved to nothing. A
  target that manages fewer objects than the sequence expected is behaving, not failing.
- The example's `spec.dnsNames` overlay admitted `plg-.example.test`. A DNS label may not
  start or end with a hyphen, and neither the CRD nor the API server says so, so §8.3 makes
  the pattern the target declaration's job.
- A Widget created at `spec.count: 0` went Ready 0 to 0, so its status merge patch carried
  only `observedGeneration` and never wrote `status.ready`. Its own `ready` expression then
  never held. About 13% of toy draws set count 0, and every one of them failed G4.

### What fixed it

The acceptance test now pins seed 2 and replays the minimized sequence against a toy with
no seeded bug, requiring it to pass. Both mutations of it fail loudly. That is the rule the
milestone earned: **a test that asserts the harness found something must assert in the same
test that the control finds nothing.** It is the bug matrix's B0 row applied to every
acceptance test, and it would have caught both of the misses above.

D33 gave generation the settle waits that leave the ops it drew judged, and
`Sequence.Validate` rejects a sequence without a terminal one, which also stops the shrink
pass proposing it. Generation then wraps every drawn `restart` in settle waits on both
sides, and the 13 seeds that had failed or gone unjudged all pass with no notes. The shape
is forbidden in generation and not in the format: `b0.json` and `b10.json` each follow a
restart with a change and no settle between, on purpose, and a hand-written sequence is
entitled to.

The shrinker compares the check ID alone again. `deleteManaged` skips and reports a note
(§7). The overlay spells out an RFC 1123 label. The toy patches its whole status rather
than a diff against a base.

### Process notes

Both of the large misses were caught by adversarial reviews rather than by the tests, and
the second review caught the first review's fix being incomplete. One review per change is
not enough when the change is the thing that decides whether the tests mean anything.

Reasoning about a value's shape from one of its two producers is what broke the shrinker.
`Violation.Statement` reaches the shrinker from the Runner and from the Engine, and only
the Runner's form matched the assumption.

`pkg/run` cannot import `pkg/generate`, because the generator produces a `run.Sequence` and
the dependency runs the other way, so the M5 acceptance test lives in `pkg/run`'s external
test package.

The example keeps its two hand-written sequences and gains generated runs. The fixed seeds
draw `deleteManaged` and `recreate`, which neither file did, and `reissue.json` is the only
`update` the tier runs. The tier runs both files, so the worked example §7 needs cannot rot.

`docs/bug-matrix.md` now marks G3 unjudged in five of eleven rows and G5 in one, where the
toy's 10s deletion window closes before the teardown reaches a verdict. Those cells used to
read as passes. It is the first time D31's notes are visible in the matrix.

Two runs against cert-manager cannot overlap, and only the quickstart's port guard keeps
them apart. A caller that drives `botbox` directly gets no warning: the second controller
dies on bind and botbox reports a harness error several ops later.

M5 outcome: `make test-example` draws its sequences, and a nightly workflow draws its own
seeds.

## M6 — 2026-09-21

### Right

The acceptance was proved with a throwaway sequence before any M6 code was written, and
the assumption behind it was wrong. G4 is the invariant that anchors on "the fault
stopped", so G4 was what the fault was expected to break. It breaks G1 instead: the toy's
backoff outlives the fault, and the create it finally lands falls in the quiet window the
teardown waits. That finding needs no seeded bug, because every controller that retries
takes time to recover.

Which invariant a fault breaks is the target's property, not the fault's. Running the
sequence settled it; reasoning about it had not.

### Wrong in the first draft

- The fault test said the fault refuses thirty creates. It refuses ten: the count trigger
  is never reached, and the teardown ends the fault. The same unchecked claim went into a
  commit message, a comment and a failure message before anyone read the request log.
- The report's replay command left out `--launch-arg` and `--kubeconfig`. A G3 found under
  `--bug=3` replayed green from its own report, so the one line a report exists to give a
  reader said the opposite of the truth.
- A rerun of the minimized sequence that found nothing left the report asserting the
  original failure beside recordings of a passing run, with the only warning on stderr.
- D35 said each excerpt is the first twenty and that nothing is dropped silently. The
  checks already bound their evidence at the twenty nearest the violation, so the report's
  own bound is a backstop, its totals never exceed twenty, and "the first N are below"
  named the wrong end.
- The checks flattened their evidence into a one-line summary and dropped the requests and
  versions behind it, so §5.7's "request log excerpt, object version timeline" had nothing
  to quote. The data existed and was discarded one layer before the report.
- A fault's duration halved toward a nanosecond in thirty-two steps, each step a cluster,
  and every successful weakening re-replayed the removal candidate it had just rejected.
- The acceptance test was built on the toy's recovery time, so its verdict came down to
  whether a retry landed inside a window. It passed on a 115 ms margin, and no retiming of
  the windows widened it.
- The G4 an expired settle wait raises carried no requests and no versions, so the one
  run M6's acceptance gates on would have written a report with no evidence to quote.
- One `fault` op left the rest of the run unjudged. The Runner treated a fault as active
  from the op that injected it until it dropped the spec, and only an op-index trigger
  made it drop one, so a `count` or `for` trigger — the shape the README documents — kept
  every later window excused. A fault matching a resource the target never touches did it
  too. B7 with an inert fault reported nothing, and neither did `b11-fault.json` with its
  trigger written as `{"count": 1}`. The exploratory review found it by writing the fault
  op the documentation shows.
- B11 was written up as a bug only a fault can reveal, twice: in §9.1, the matrix's own
  preamble and the acceptance test, and again after a `deleteManaged` op had revealed it.
  The belief outlives whatever removed the child, a scale-down included, so B11's matrix
  row is two spec changes. Two runs settled what two rounds of reasoning had not.

### What fixed it

D35 and the README say what the code does. `run.Violation` carries the evidence. The
replay command repeats the flags that selected the run. A report says when the recordings
beside it are of a run that found nothing, which is what D31 exists to require. Durations
halve to a floor, removal is asked once per position, and a candidate that changes nothing
is refused, which also stops the pass spinning.

A fault now excuses the target over the window the proxy applied it in, and a fault the
proxy never applied excuses nothing (D36). The proxy reports what it did with each fault,
and a spec keeps its progress across a `SetFaults` call, so dropping one fault no longer
restarts another's trigger.

B11 makes a fault's damage permanent. The toy notes a child as present the moment it asks
the API server for it, so a refused create leaves it one child short for good: no error, no
requeue, no watch event. The settle wait after the fault stops then expires however wide
any window is, and the acceptance turns on state rather than timing. A G4 the Runner raises
itself quotes the CR's history and the requests nearest the expiry, as a check's G4 does.

### Process notes

Two adversarial reviews ran over this work rather than one. The reviewer reading the diff
and mutating the code found the unpinned bound and the report quoting the wrong end of its
evidence. The reviewer driving the binary found the fault op that silences the invariants.
Neither would have found the other's.

Nine mutations of the M6 code outside `pkg/report` survived the first draft. The sharpest
was §10 M6's own acceptance: nothing tied a report to the minimized sequence, so the
report could have carried the sequence botbox drew and every test would have passed.
A milestone's acceptance sentence deserves a test that reads like it.

One of the tests written to close those holes passed for the wrong reason: it looked for an
object name that also appeared inside a request path, so the version table could vanish
unnoticed. Mutating each field separately caught it; running the test did not.

Two retimings of the acceptance test were tried and both were invalid. A one-second settle
breaks the toy's first reconcile before any fault exists. A wider stable makes convergence
impossible, because a settle wait needs `T_stable` of quiet inside `T_settle` — which is
issue #10, a target configured that way fails G4 on every op and botbox blames the
controller.

M6 outcome: a failing run writes a report that quotes its evidence, a fault shrinks toward
a shorter duration, and a fault makes the toy fail an invariant it otherwise passes.

## M7 — 2026-09-22

### Right

external-secrets v2.11.0 runs unmodified, and this adoption changed no Go code in botbox.
A spike measured the controller against §8's contract before anything was written
(`docs/spikes/2026-09-22-external-secrets-envtest.md`), so the target file was right the
first time. `make test-example-external-secrets` passes from a checkout holding no binary,
in 6m24s including the clone and a 16 s build, and in 6m19s warm. The spike measured 150 s
for that build on empty caches, and this machine's module and build caches already held
external-secrets' dependencies.

The negative control is again the controller's own documented behaviour rather than a
seeded bug. Under `spec.target.creationPolicy: Orphan` the Secret carries no
ownerReference, the collector of §5.8 has nothing to resolve, and G3 reports that `the
v1/Secret example-secret was still there 1m0s after the CR was deleted, orphaned`.

### Wrong in the first draft

The negative control's match was `G3 .*Secret example-secret`, which the renamed Secret
`example-secret-renamed` also satisfies. The control never renames, so nothing passed that
should not have. cert-manager's control carried the same loose match, and both now quote
the clause `pkg/invariant/g3.go` writes.

Both `verify-*-pin` targets checked the declared version and the asset digest and never
the commit, and the tag check lived in the build rule alone, which a warm CI cache skips.
A moved tag would have passed. Each verify target resolves the tag with `git ls-remote`
now.

The nightly matrix gave both examples one label and one issue title, so two legs failing
on the same night raced to file duplicates. The label and the title name the example.

### What an adopter supplies

The same six keys M4 listed, and each of them differed from cert-manager's.

- `crds` names the release asset, which here is a whole install manifest. envtest reads
  the file and keeps the 25 CustomResourceDefinitions, so nothing has to be extracted.
- `sample` is one valid primary CR, and its `refreshPolicy` is what makes the controller
  quiet enough to judge (D40).
- `fixtures` is a SecretStore on the `fake` provider, carrying two keys so that a sequence
  can switch `remoteRef.key`.
- `manages` names `v1/Secret` and not `v1/Event`. The controller leaves Events behind and
  posts two more on every start.
- `ready` is CEL over `status.syncedResourceVersion`, a `"<generation>-<hash>"` string,
  because an ExternalSecret carries no `observedGeneration` anywhere.
- `launch` gives the binary and its flags. There is no `--kubeconfig` flag: the controller
  reads the `$KUBECONFIG` the launcher exports, and passing the flag kills it.

No Go code was written against external-secrets.

### Findings

Nothing. The second adoption found no harness bug, where cert-manager's found two. Every
key the target needed was already in §8.1, and every check reached the verdict the spike
predicted.

What it did surface is design, not defect. §14's first open question has a real instance:
under `refreshPolicy: Periodic` the controller rewrites status every `refreshInterval`. A
10 s interval never lets the quiet window close, so G4 reports the settle wait expiring
before G2 ever counts the writes as churn. The answer taken is a target-side setting
rather than a G2 exemption list (D40), and the question stands for a controller that
offers no such setting. And a negative control need not be a launch flag: this one is a
sequence, because ownership is a field of the CR (D41).

### Process notes

The spike did the discovery, so the adoption was transcription, and its facts held: the
release asset hashed to the pinned digest, the tag named the pinned commit, the readiness
predicate drove every run, and the negative control printed the G3 line the spike quoted.

Naming `v1/Event` in `manages` fails on G5 before it reaches G3, because the two Events
the controller posts on start appear only after a `Restart` op. The spike predicted the
G3 failure and not which check fires first.

The generated runs are not vacuous. Widening the `spec.target.name` overlay to a pattern
the CRD rejects made seed 31 fail at admission on op 5, an `update` on that path, so
generation reaches the API server with mutated values on a seed the spike never drew.

cert-manager's fixed healthz port keeps its runs sequential and external-secrets' do not,
which is worth having in the repo: the first example's port guard could be read as
something botbox requires rather than something one controller forces.

M7 outcome: `make test-example-external-secrets` runs on every pull request, and one
nightly workflow draws seeds for both examples.
