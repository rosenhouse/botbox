# Journal

This file records one entry per milestone: what the agents got right, what they got
wrong, and which prompt, skill, or convention change fixed it (DESIGN.md §12). It is a
first-class deliverable of the repo, not a changelog.

## M0 — 2026-09-20

This entry covers the design-decision session that preceded the scaffold, not the
scaffold PR itself.

### Right

The thesis held up. The six generic invariants held up too. A spike ran cert-manager
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

Decisions D1 through D21 in DESIGN.md §15. The new §5.8 (test cluster) and §8.3
(webhook-only rules). The network-assumptions convention in §11. The autonomy rules in
§12: the agent merges once CI is green, amends the design in the same PR, starts a
fresh session every hour, and hands bounded work to subagents with fresh context and
cheaper models.

### Process notes

The maintainer answered eight decision questions in one sitting. The sandbox could
reach `proxy.golang.org` and `github.com` but not `quay.io`. That gap drove the
"clone and build" rule for external targets.

M0 outcome: (filled in when the M0 PR merges)
