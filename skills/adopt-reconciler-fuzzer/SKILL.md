---
name: adopt-reconciler-fuzzer
description: Sets up reconciler-fuzzer for a kubebuilder or controller-runtime Kubernetes controller. Writes target.yaml and a sample custom resource, builds the controller, runs reconciler-fuzzer until it tests the controller, and reports what it finds. Use when the user wants to fuzz their operator or controller, or to adopt or try reconciler-fuzzer in their repository.
---

# Adopt reconciler-fuzzer

reconciler-fuzzer runs a controller against envtest, acts on its custom resources (CRs), and
checks that it converges, goes quiet and cleans up. This skill writes the target for a
kubebuilder project and runs it until reconciler-fuzzer tests the controller.

Never edit the controller's source without the user's consent.

Copy this checklist and track it:

```
- [ ] 1. Check that the controller fits
- [ ] 2. Install reconciler-fuzzer and find its docs
- [ ] 3. Choose a directory
- [ ] 4. Build the controller
- [ ] 5. Write target.yaml and the sample
- [ ] 6. Run until reconciler-fuzzer tests the controller
- [ ] 7. Report
```

## 1. Check that the controller fits

Read `PROJECT`, `cmd/main.go`, `api/`, `internal/controller/` and `config/`. Stop and tell
the user when one of these holds:

- The primary kind is cluster-scoped, as `namespaced: false` in `PROJECT` says.
- The controller creates a cluster-scoped object, or an object in another namespace.
- The controller waits for a Pod, a Deployment, a Job or a PersistentVolumeClaim to become
  ready. envtest runs no Pod. Offer a kind cluster instead, as
  `<module>/docs/targets.md#against-a-cluster` says.
- The CRD serves several versions through a conversion webhook. No webhook runs.

One target.yaml tests one primary kind. If `PROJECT` lists several kinds with controllers,
ask which to start with.

## 2. Install reconciler-fuzzer and find its docs

If `reconciler-fuzzer` is not on `PATH` or in `$(go env GOPATH)/bin`, install it:

```sh
GOTOOLCHAIN=auto go install github.com/rosenhouse/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
```

`go install` writes to `$(go env GOPATH)/bin`. If that is not on `PATH`, prefix each command
with `PATH="$(go env GOPATH)/bin:$PATH"`. Then print the directory of the module it was built
from:

```sh
echo "$(go env GOMODCACHE)/github.com/rosenhouse/reconciler-fuzzer@$(reconciler-fuzzer version | cut -d' ' -f2)"
```

Below, `<module>` stands for that directory. If it is missing, run
`go mod download github.com/rosenhouse/reconciler-fuzzer@<version>`. If the version is
`(devel)`, reconciler-fuzzer was built from a clone, and `<module>` is that clone.

The docs there match the installed binary. Read them rather than guessing:

- `<module>/docs/reference.md` lists every key of target.yaml, with its default.
- `<module>/docs/targets.md` says how to choose each value.
- `<module>/docs/failures.md` says what each failure means.

Follow `<module>/README.md#install` to install the envtest control plane. Shell variables do
not last from one command to the next, so note the path in `KUBEBUILDER_ASSETS` and set it on
each `reconciler-fuzzer` command.

## 3. Choose a directory

Ask the user where the files go, and propose `test/reconciler-fuzzer/`. Add
`reconciler-fuzzer-out/` to `.gitignore`.

## 4. Build the controller

```sh
make manifests
go build -o bin/manager ./cmd
```

`make manifests` writes the CRDs under `config/crd/bases/` and `config/rbac/role.yaml`. A
kubebuilder v3 project keeps `main.go` at its root, so build `.` there. Rebuild after each
change to the controller.

## 5. Write target.yaml and the sample

This is the target of a project named guestbook. Its Guestbook, in group
`webapp.example.com`, owns ConfigMaps. reconciler-fuzzer reads `crds`, `sample`, `rbac` and
`fixtures` relative to target.yaml, and `launch.binary` relative to the repository root.

```yaml
name: guestbook
crds:
  - ../../config/crd/bases/
primary: webapp.example.com/v1/Guestbook
sample: guestbook.yaml
manages:
  - v1/ConfigMap
rbac:
  - ../../config/rbac/role.yaml
ready: >-
  has(status.conditions) && status.conditions.exists(c, c.type == "Ready"
  && c.status == "True" && has(c.observedGeneration)
  && c.observedGeneration == metadata.generation)
launch:
  binary: bin/manager
  args:
    - --leader-elect=false
    - --health-probe-bind-address=0
    - --metrics-bind-address=0
  env:
    ENABLE_WEBHOOKS: "false"
```

Fill each key from the project:

- `primary` joins the CRD's `spec.group`, its storage version and its kind.
- `manages` lists each kind the controller creates. Look for `Owns(`, `SetControllerReference`,
  `Create(` and `CreateOrUpdate(` in the controller, and for the `create` verb in `role.yaml`.
  Write a core kind as `v1/Kind`, and any other as `group/version/Kind`.
- `ready` depends on how the reconciler writes status:
  - If it sets `status.observedGeneration`, leave `ready` out. The default compares it with
    `metadata.generation`.
  - If it sets a condition with `ObservedGeneration`, use the expression above with that
    condition's type.
  - Otherwise, write a `ready` that holds once the controller has finished. Tell the user that
    this `ready` cannot tell a stale status from a current one.
- `launch.args` holds kubebuilder's flags. `0` turns off the health probe and metrics
  servers, so no port collides. Drop each flag that `main.go` does not define. Set
  `ENABLE_WEBHOOKS` only where `main.go` reads it. reconciler-fuzzer sets `KUBECONFIG` for
  the controller.
- `fixtures` lists files of objects the controller reads and does not create, such as a
  Secret the spec names. Write each beside target.yaml, with no `metadata.namespace`. See
  `<module>/docs/targets.md#fixtures`.

Leave out `timeouts`, `thresholds`, `properties` and `generate`. Their defaults suit a first
run.

Copy the sample from `config/samples/`. Drop `metadata.namespace`, and replace each `TODO`
with a spec the controller accepts.

No webhook runs. Look for `Default(` under `internal/webhook/`. Set each field it defaults in
the sample. Generation drops optional fields, so also make each such field required. An
overlay's `required` replaces the CRD's list, so keep the CRD's required fields in it:

```yaml
generate:
  overlay:
    spec: {required: [message, pages]}
```

## 6. Run until reconciler-fuzzer tests the controller

Run this from the repository root. A run that fails takes 10 minutes or more, because
reconciler-fuzzer then minimizes its sequence, so give the command a timeout of at least 20
minutes:

```sh
KUBEBUILDER_ASSETS=<path> reconciler-fuzzer run --target test/reconciler-fuzzer/target.yaml --seed 1 --runs 3
```

Act on the exit code.

**Exit 2**: reconciler-fuzzer could not test the controller, and its message says why.
`<module>/docs/failures.md#when-reconciler-fuzzer-exits-2` lists the usual causes.
`target.log` in the run's directory holds the controller's output. Fix target.yaml or the
sample, and run again. If only a change to the controller would fix it, stop and ask the user.

**Exit 1**: a check failed. Read `report.md` in the run's directory, then decide whether
target.yaml misdescribes the controller. These failures can mean it does:

| Check | What target.yaml got wrong | Fix |
|---|---|---|
| G4 | `ready` reads a field or condition the controller never writes. | Fix `ready`. |
| G4 | Generation drew a spec value that the controller rejects, or that a webhook would. | Narrow it, as `<module>/docs/targets.md#generated-values` says. |
| G1, G2 | The controller requeues with `RequeueAfter` by design. | Ask the user, then set `thresholds.quiet` as `<module>/docs/targets.md#thresholds` says. |
| G5 | The controller stamps a field when it starts. | Name it under `equalIgnore`, as `<module>/docs/targets.md#equalignore` says. |
| G7 | The controller leaves a deleted kind deleted, or recreates it under a new name, by design. | List the kind under `notRecreated`. |

Every other failure is a finding, such as G3 for a child without an ownerReference. Never
hide a finding to make a check pass. Do not drop a kind from `manages`, raise a threshold or
narrow generation unless the controller's design calls for it. Ask the user when in doubt.

On a finding, stop. Give the user the check, the line reconciler-fuzzer printed, the path of
`report.md`, and the replay command that `report.md` gives, with `KUBEBUILDER_ASSETS` set.

**Exit 0**: every run passed.

If five runs in a row make no progress, stop and report what you tried.

## 7. Report

Tell the user:

- which files you wrote, and the command that runs them
- how the last invocation ended, with each finding
- what you assumed, such as the `ready` you chose and the values in the sample
- what to do next: pin sequences as `<module>/README.md#pin-sequences` says, and run in CI as
  `<module>/docs/ci.md` says
