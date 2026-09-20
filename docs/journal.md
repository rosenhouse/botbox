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
