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
The CRD schema rejected every invalid count server-side. Mutation checks caught every
seeded-bug branch and every review fix.

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

### What fixed it

§9.1 rows B2, B3 and B6 say what the bugs do; D22 moves P1 to checkpoints. The CLAUDE.md
review personas ran as subagents; the exploratory tester drove the binary against envtest
with kubectl and found the 409s.

### Process notes

The Claude Review workflow cannot run without a repository API secret, so PRs #3 and #4
needed a human merge. Routines created from a session get no GitHub tools, so the
continuation ran in the session itself.

M1 outcome: PR #4.
