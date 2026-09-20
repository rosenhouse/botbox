---
name: add-invariant
description: Add a new generic invariant (G-series) to botbox: design row, pure-function implementation, fixture tests, toy-target bug that trips it, and bug-matrix update. Use when asked to add, split, or redefine an invariant.
---

# Add a generic invariant

A generic invariant (`G7` and later) is a pure check applied to every target, defined
in DESIGN.md §6 and implemented in `pkg/invariant`. Follow this procedure.

1. **Design first.** Add or change the row in DESIGN.md §6, in the same PR as the
   code. State the invariant's window and its stability requirement, the way G1–G6
   do. Note the change under a "Design change" heading in the PR description
   (§11, §12).

2. **Implement as a pure function.** Add it to `pkg/invariant` as a pure function over
   the Proxy request log, the Observer state history, and the target declaration
   (§5.6). Give it a per-target configurable window with a sensible default.

3. **Add fixture tests.** Write unit-tier tests (no API server) from recorded
   `requests.jsonl` and `objects.jsonl` fixtures. Include at least one fixture that
   passes and one that fails the invariant.

4. **Add or reuse a seeded bug.** Check whether an existing bug in
   `targets/toy-widget` (§9.1) already trips the new invariant. If none does, add one:
   a new row in the §9.1 bug catalog, a new case behind the `--bug` flag, and a
   regenerated `docs/bug-matrix.md` with a non-empty row for the new ID (§9.1, the
   M3/M6 acceptance criteria).

5. **Never retry or sleep to make it pass.** Flakiness is a harness bug, not the
   target's. If the invariant is unstable, tune its window instead of adding a retry
   or a sleep (§5.6, §11).

6. **Name the new ID in the PR description**, alongside the milestone and any other
   invariant or property IDs the PR touches (§11).
