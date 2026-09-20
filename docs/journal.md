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
