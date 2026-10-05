# botbox

botbox finds bugs in a Kubernetes controller. It runs your controller on your machine against a
real kube-apiserver and etcd, which envtest provides, behind a proxy that records each request.
It creates, updates and deletes your custom resources (CRs), restarts your controller, and
deletes objects your controller manages. Then it checks that your controller converges, goes
quiet, cleans up and recreates what it lost, and that properties you declare hold. botbox
shrinks a failing sequence of ops to the ops the failure needs, and writes a report.

botbox needs no test code. One `target.yaml` describes your controller. botbox does not replace
your unit tests.

## What botbox cannot test yet

botbox runs your controller on your machine, not in a Pod. Your controller cannot reach a Pod
or a Service from there, and no admission or conversion webhook of yours runs. So write in
your `sample` CR what your webhooks would add, and keep your `primary` kind and your controller
on the version your CRD stores. [docs/targets.md](docs/targets.md#generated-values) shows how
to keep generated CRs within what your webhooks would admit.

botbox generates sequences only for a primary kind your `crds` define. For a built-in kind,
such as a Service, it runs only the [sequences you write](docs/targets.md#sequences-you-write).
[target.yaml](#write-targetyaml) names the sample, the primary kind and the CRDs.

- botbox tests namespaced kinds only. It refuses a cluster-scoped primary, managed kind or
  fixture when it loads the target, and exits 2. For a kind that neither Kubernetes nor your
  CRDs define, it asks the cluster, and refuses a cluster-scoped one before the first run. It
  watches only the namespace it creates for each run. So it passes a controller that leaks a
  child in another namespace, and it cannot supply an object your controller reads from
  another namespace
  ([#38](https://github.com/rosenhouse/botbox/issues/38)).
- botbox does not test your controller's RBAC. Its proxy sends your controller's requests with
  botbox's own credentials, which are admin on envtest, so a rule your Role lacks goes
  unnoticed ([#45](https://github.com/rosenhouse/botbox/issues/45)).
- The sequences botbox generates inject no faults. Only a sequence you write carries one
  ([#47](https://github.com/rosenhouse/botbox/issues/47)).

## Install

```sh
go install github.com/rosenhouse/botbox/cmd/botbox@latest
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.1
export PATH="$(go env GOPATH)/bin:$PATH"
index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 --index $index -p path)"
```

`go install` puts both tools in `$(go env GOPATH)/bin`, or in `$GOBIN` where you set it.
`setup-envtest` downloads etcd and kube-apiserver, and `KUBEBUILDER_ASSETS` tells botbox where
they are. On Linux it keeps them under `~/.local/share/kubebuilder-envtest`, and `--bin-dir`
names another directory, as the [CI recipe](#running-in-ci) does. A new shell needs the last
three lines again.

botbox has no release yet, so `@latest` installs main as it is now. Name a commit in its place
to install the same botbox every time, as [CI](#running-in-ci) should. In a clone of the
repository, `go install ./cmd/botbox` installs the botbox that its README describes.

Building botbox takes Go 1.26.0 or later. A `go` command from Go 1.21 on downloads that Go
itself, unless `GOTOOLCHAIN=local` is set, as in many CI images. Then `go install` fails with
`requires go >= 1.26.0`. Install a newer Go, or run the commands with `GOTOOLCHAIN=auto`.

`botbox --help` lists the commands and the exit codes. `botbox run --help` lists each flag with
its default.

## A first run and a first find

Every find this README shows is planted: a bug seeded into the toy controller in this
repository. Clone [the repository](https://github.com/rosenhouse/botbox), and run this in the
clone. The first build takes a minute or two, and the three runs then take about half a
minute:

```sh
go build -o bin/toy-widget ./targets/toy-widget
botbox run --target targets/toy-widget/target.yaml --seed 1 --runs 3
```

```
the deadline is 11m16s: these 3 runs can take 7m16s at the target's timeouts, and minimizing a failure gets the rest, at least 4m0s. --deadline sets another.
run 1: seed 1, generated
run 2: seed 2, generated
run 3: seed 3, generated
every run passed.
```

Each run creates a namespace, starts the toy controller and applies a sequence of ops that its
seed draws. The toy is correct, so every run passes.

Now seed bug B3, which leaves a ConfigMap without an ownerReference. `--launch-arg`, on `run`
or `replay`, adds an argument to the toy's command line. This replays a sequence that creates a
Widget and deletes it:

```sh
botbox replay --target targets/toy-widget/target.yaml --launch-arg --bug=3 targets/toy-widget/sequences/b3.json
```

```
the deadline is 2m12s: this run can take that long at the target's timeouts. --deadline sets another.
run 1: seed 20260920, sequence targets/toy-widget/sequences/b3.json
run 1: G3 the v1/ConfigMap widget-0 was still there 10s (timeouts.delete) after widget, the last CR it may belong to, was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-30T19:50:43.395112151Z; 1 version, the first v1/ConfigMap widget-0
  the evidence is in botbox-out/20260930T195026Z-20260920/run-1
```

botbox exits 1. The first indented line says when G3 failed, how many object versions the
report quotes, and whose version comes first: here one version, of the ConfigMap `widget-0`.
`report.md` in the evidence directory says what failed. The toy converges, so nothing looks
wrong until the Widget is deleted and its ConfigMap stays. envtest runs no garbage collector,
so an envtest suite catches this only if it asserts each ownerReference itself. botbox
emulates the collector, and G3 judges every object of every kind your target manages, in
every run, with no test code of yours.

## Your own controller

### Write target.yaml

A target is one YAML file. This is the toy's:

<!-- embed: targets/toy-widget/target.yaml -->
```yaml
# botbox reads crds, sample and fixtures relative to this file, and
# launch.binary relative to the directory it runs in.
name: toy-widget
version: dev                       # Reports print it.
crds:                              # botbox installs the CRDs in these files and directories.
  - crds/
primary: toy.botbox/v1/Widget      # botbox creates, changes and deletes CRs of this kind.
sample: widget.yaml                # Each drawn sequence creates a variant of this CR first.
manages:                           # The controller creates objects of these kinds.
  - v1/ConfigMap
rbac:
  - rbac/role.yaml
launch:
  binary: bin/toy-widget
  args:
    - --kubeconfig=$KUBECONFIG     # botbox writes a kubeconfig for its proxy.
    - --label-from=widget-config
    - --bug=0                      # A later --bug wins, so --launch-arg --bug=3 seeds B3.
  env:
    WATCH_NAMESPACE: $NAMESPACE    # botbox replaces $NAMESPACE with each run's namespace.
fixtures:                          # The controller reads these objects and owns none.
  - config.yaml
ready: >-                          # This CEL holds once the controller has converged.
  has(status.observedGeneration) && status.observedGeneration == metadata.generation
  && has(status.ready) && status.ready == spec.count
properties:                        # These checks are your own.
  - id: P1
    description: status.ready never exceeds the number of ConfigMaps present.
    # A property also runs where no CR exists, so the CEL guards status.ready.
    cel: '!has(status.ready) || status.ready <= managed.filter(o, o.kind == "ConfigMap").size()'
timeouts:                          # The toy converges in milliseconds.
  settle: 5s
  stable: 2s
  delete: 10s
thresholds:
  # G6 fails a controller that repeats a failing request more than errloop
  # times. controller-runtime's backoff repeats one 10 times in a 5s settle.
  errloop: 5
```

Write yours in this order:

1. `name` names the target, and every sequence for it carries that name. botbox installs the
   CRDs in `crds`, and creates, updates and deletes CRs of the `primary` kind. `sample` holds
   one valid CR with a `metadata.name`. Each drawn sequence first creates it, with drawn values
   in some spec fields.
2. `manages` lists every kind your controller creates, as `group/version/Kind`, or `v1/Kind`
   for the core group. botbox judges your controller by the objects of these kinds.
3. `launch.binary` names your controller's executable. botbox sets `KUBECONFIG` for it. In
   `launch.args` and `launch.env`, it replaces `$KUBECONFIG` with the kubeconfig's path and
   `$NAMESPACE` with the run's namespace. Turn leader election off, and give each port your
   controller binds a free one, such as `127.0.0.1:0`.
4. `ready` is CEL over the CR's `metadata`, `spec` and `status`. Leave it out, and botbox uses
   `has(status.observedGeneration) && status.observedGeneration == metadata.generation`.
   Guard each optional field with `has()`. A CR whose `Ready` condition carries
   `observedGeneration` can use this:

   ```yaml
   ready: >-
     has(status.conditions) && status.conditions.exists(c, c.type == "Ready"
     && c.status == "True" && has(c.observedGeneration)
     && c.observedGeneration == metadata.generation)
   ```
5. `properties` are optional checks of your own. A property runs once for each CR, and its
   CEL reads that CR's `metadata`, `spec` and `status`. `managed` holds every object of your
   `manages` kinds that this CR owns, plus every object that no CR owns. It leaves out other
   CRs' objects. A property reads each object whole, such as `o.metadata.name` or `o.data`. A
   property also runs where no CR exists, as after the last delete, with empty `metadata`,
   `spec` and `status`. Guard what it reads there with `has()`, as P1 does.
6. `timeouts` and `thresholds` default to what suits most controllers. The toy is fast, so it
   shortens its timeouts. Its 5s `settle` holds only 10 backoff repeats, so it also lowers
   `errloop` below the default of 10.

[docs/reference.md](docs/reference.md) lists every key with its default.
[docs/targets.md](docs/targets.md) says how to choose the values: for a slow controller, one
that resyncs on a timer, one that reads objects it does not own, and more.

By default, botbox starts envtest, which runs no Pod and never moves the status of a
Deployment, a Job or a PersistentVolumeClaim. If your controller waits on one, run botbox
[against a cluster](docs/targets.md#against-a-cluster), such as kind.

### Run it

Run botbox where `launch.binary`'s path resolves, such as your repository root:

```sh
botbox run --target target.yaml --runs 5
```

Each run draws a sequence of ops from `create`, `update`, `delete`, `recreate`, `settle`,
`restart` and `deleteManaged`, and from the [fixture ops](docs/targets.md#fixtures) where
`generate.fixtures` names a fixture. A drawn `create` adds a second or a third CR beside your
sample. Without `--seed`, botbox draws a seed and prints it. `--seed` draws the same sequences
again.

### Pin sequences

Drawn sequences hold few `deleteManaged` ops, and few updates after a CR has settled. So a few
drawn runs can pass a controller that does not watch a kind it manages, or one that ignores a
spec change. Pin a sequence for each:

- For each managed kind, create your sample and delete its first object of that kind with
  `deleteManaged`. G7 requires your controller to recreate it.
- For each property, create your sample and update a spec field that changes what the
  property reads.

Each of these ops settles before the next begins. This sequence does both for the toy, whose
P1 reads the ConfigMaps that `spec.count` sets:

```json
{"seed": 1, "target": "toy-widget", "ops": [
  {"i": 0, "t": "create", "obj": {"apiVersion": "toy.botbox/v1", "kind": "Widget",
    "metadata": {"name": "widget"}, "spec": {"count": 3}}},
  {"i": 1, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
  {"i": 2, "t": "update", "patch": {"spec": {"count": 2}}}]}
```

`i` numbers the ops from 0. A sequence's `seed` draws only the requests that a fault's
`match.fraction` picks, so any number serves here.
`botbox replay --target target.yaml sequences/pin.json` runs one such file, and
`botbox run --target target.yaml sequences/*.json` runs several, as written.
[docs/targets.md](docs/targets.md#sequences-you-write) says how to write one, with faults too.

### Real controllers

`examples/` holds targets for two real controllers, cert-manager and external-secrets, each
built from its own source. [docs/examples.md](docs/examples.md) runs them, with a negative
control for each.

## Reading a failure

botbox exits 0 when every run passes, 1 when a check fails, and 2 when it could not test your
controller. On a failure, it prints the check's ID, what the check saw and the run's evidence
directory, `botbox-out/<timestamp>-<seed>/run-<n>/`. Start with `report.md` there. It says what
failed, quotes what the check read, such as requests or object versions, and gives the command
that replays it.

Before it reports, botbox minimizes a failing drawn sequence: it removes each op the failure
does not need. Each removal it tries replays a whole run, so this can take several minutes.
botbox says so as soon as the run fails, as in
`run 1: G3 failed, and minimizing its 12 ops can take minutes.` `sequence.json` in the
evidence directory holds the minimized sequence. Where the deadline or an interrupt cut
minimizing short, it holds the drawn sequence, and `sequence.shrunk.json` beside it holds any
smaller failing one that botbox found. Where your controller fails with no CR at all, the
minimized sequence lacks even the `create`. Each run ends by deleting every CR and checking
once more, so G3 or a property can fail a sequence with no `delete`. `summary.json`, one
directory up, holds each run's sequence as drawn.

| Check | Usual cause |
|---|---|
| G1 | Your controller keeps making requests after it converged, as a resync timer does. |
| G2 | Your controller keeps rewriting an object, such as a timestamp in a status. |
| G3 | A child lacks an ownerReference to its CR, or a finalizer stays on the CR. |
| G4 | Your controller did not converge within `timeouts.settle`. It is slow, it never stops writing, or it waits on a Pod, which [envtest never runs](docs/targets.md#against-a-cluster). Or `ready` reads a field the CR lacks. |
| G5 | A restart changed converged state: a field your controller stamps at startup, or a change it missed until then. |
| G6 | Your controller repeats one failing request. |
| G7 | Your controller missed the deletion. It does not watch the object's kind, as with a missing `Owns()`, or the object lacks the controller ownerReference that `Owns()` follows. |

A property's ID, such as P1, names a check of your own. The report quotes the managed objects'
metadata where it failed, and says when no CR existed. `objects.jsonl` in the evidence directory
holds every object whole. From there, this prints each version of the ConfigMap `widget-0`'s
data:

```sh
jq -c 'select(.name == "widget-0") | .object.data' objects.jsonl
```

`requests.jsonl` holds only your controller's requests. On envtest, `collector.jsonl` holds each
delete that botbox's garbage collector tried, so you can tell its deletes from your controller's.

[docs/failures.md](docs/failures.md) says what each file and message means.

### When botbox exits 2

botbox could not test your controller, and the message says why.
[docs/failures.md](docs/failures.md#when-botbox-exits-2) lists the usual causes and what to change.

## Running in CI

Copy this workflow into `.github/workflows/`, and replace its build and `target.yaml` with your
own. Set `BOTBOX_VERSION` to a commit of main, because `@latest` tracks main. botbox's steps
need no go.mod, no cluster and no registry.

<!-- embed: examples/ci/github-actions.yml -->
```yaml
name: botbox

on:
  pull_request:
  schedule:
    - cron: '17 6 * * *'

env:
  BOTBOX_VERSION: <commit>   # Set this to a commit of main.
  SETUP_ENVTEST_VERSION: v0.25.1
  ENVTEST_K8S_VERSION: 1.37.0
  ENVTEST_INDEX_URL: https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml

permissions:
  contents: read

jobs:
  botbox:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
          cache: false   # true needs a go.sum
      - id: tools
        uses: actions/cache/restore@v4
        with:
          path: |
            ~/go/bin
            bin/envtest
          key: ${{ runner.os }}-${{ runner.arch }}-botbox-${{ env.BOTBOX_VERSION }}-${{ env.SETUP_ENVTEST_VERSION }}-${{ env.ENVTEST_K8S_VERSION }}
      - if: steps.tools.outputs.cache-hit != 'true'
        run: |
          go install "github.com/rosenhouse/botbox/cmd/botbox@$BOTBOX_VERSION"
          go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@$SETUP_ENVTEST_VERSION"
      - run: |
          assets=$(setup-envtest use "$ENVTEST_K8S_VERSION" --index "$ENVTEST_INDEX_URL" --bin-dir bin/envtest -p path)
          echo "KUBEBUILDER_ASSETS=$assets" >>"$GITHUB_ENV"
      # Saving before botbox runs fills the cache even when botbox fails.
      - if: steps.tools.outputs.cache-hit != 'true'
        uses: actions/cache/save@v4
        with:
          path: |
            ~/go/bin
            bin/envtest
          key: ${{ steps.tools.outputs.cache-primary-key }}
      - run: go build -o bin/controller ./cmd/controller   # Build what launch.binary names.
      # exec lets a cancel's signal reach botbox, which bash does not pass on.
      - if: github.event_name == 'pull_request'
        run: exec botbox run --target target.yaml --seed 23 --runs 5 --deadline 10m --out botbox-out
      - if: github.event_name == 'schedule'
        run: exec botbox run --target target.yaml --runs 20 --deadline 30m --out botbox-out
      - if: always()
        run: cat botbox-out/*/summary.md >>"$GITHUB_STEP_SUMMARY" || true
      # gh run download <run-id> --name botbox-out --dir botbox-out restores
      # the paths that the replay command in report.md names.
      - if: always()
        uses: actions/upload-artifact@v4
        with:
          name: botbox-out
          path: botbox-out/
```

The job caches botbox and setup-envtest in `~/go/bin`, and the control plane in `bin/envtest`,
under a key of their versions. A Go repository may instead read Go's version from its go.mod
and turn setup-go's cache on.

A pull request runs fixed seeds, so its runs repeat from one commit to the next. The nightly run
draws fresh seeds. GitHub tells only whoever last edited the schedule when a nightly run fails.
botbox's own [nightly.yml](.github/workflows/nightly.yml) reads `summary.json` to tell a find
(a check failed, exit 1) from an error (exit 2 or no output) and files an issue under the
matching label. A seed names a sequence only for one build of botbox and one `target.yaml`, so
upgrading botbox, or editing your CRD or `target.yaml`, can draw other sequences. To tell
whether a failure comes from the change under review, replay its `sequence.json` against the
base branch's controller.

Add a step that runs your pinned sequences, such as
`exec botbox run --target target.yaml --deadline 10m --out botbox-out sequences/*.json`.

`--deadline` caps each botbox invocation, and botbox stops within seconds of it. When the
deadline ends a run, or stops botbox before its last run, botbox exits 2 rather than reporting
a find. When it ends minimizing, botbox still exits 1 and reports the smallest failing
sequence it found. Without `--deadline`, botbox derives a deadline from the longest the runs'
waits can take at your target's timeouts, plus 4m to minimize a failure. That is the worst
case, and a fast controller finishes far sooner. To size a deadline, time the pull request's
command once and add at least 4m to minimize. Or shorten `timeouts`, as the toy does.

The job uploads `botbox-out/`, which holds each run's sequence and a failing run's evidence.
Anyone who can read the repository can download it, and botbox hides only the values of a
Secret's `data` and annotations. Download it with the command in the workflow's last comment,
and build your controller. The replay command in `report.md` then runs as written from the
repository root. On GitLab or Jenkins, `--junit botbox.xml` writes the runs as JUnit XML for
`artifacts:reports:junit` or the `junit` step.

### Keep botbox out of your go.mod

`go install` leaves your module alone. Requiring botbox in your operator's go.mod, with
`go get -tool` or by importing one of its packages, raises your module to botbox's versions:

```
go: upgraded go 1.23.0 => 1.26.0
go: upgraded k8s.io/api v0.32.0 => v0.37.0
go: upgraded sigs.k8s.io/controller-runtime v0.20.0 => v0.25.1
```

botbox's Go packages make no compatibility promise yet. To pin botbox in your repository, give
it a module of its own. Run this from your repository root:

<!-- embed: examples/tools-module.sh -->
```sh
mkdir -p tools/botbox
cd tools/botbox
go mod init example.com/operator/tools/botbox
go mod edit -go=1.26.0
GOWORK=off go get -tool github.com/rosenhouse/botbox/cmd/botbox@latest
cd ../..
GOWORK=off go -C tools/botbox build -o ../../bin/botbox github.com/rosenhouse/botbox/cmd/botbox
bin/botbox version
```

`go mod edit -go` comes before `go get -tool`, so that a `go` before 1.24, which lacks `-tool`,
switches to a newer Go first. `GOWORK=off` leaves out a go.work of yours, whose go line
`go get` would raise. `tools/botbox/go.mod` then pins botbox. Your own go.mod and go.work, and
any package of yours in `tools/`, stay as they were. Under `GOTOOLCHAIN=local`, a `go` before
1.24 stops this recipe with `flag provided but not defined: -tool`. Run `bin/botbox` as the last
line does, not `go -C tools/botbox tool botbox`, which runs botbox in `tools/botbox/`, where
your `launch.binary` does not resolve.

In the CI workflow, build `bin/botbox` with the recipe's `go -C tools/botbox build` line, in a
step of its own after checkout, because the cache holds no `bin/botbox`. Install only
setup-envtest in the cached step, drop `BOTBOX_VERSION` from the workflow and its cache key,
and run `bin/botbox`. setup-go's cache, with `cache: true` and
`cache-dependency-path: tools/botbox/go.sum`, keeps that build's downloads.

### From go test

A Go test can run botbox and fail on what it finds. This one runs it on the toy controller:

<!-- embed: targets/toy-widget/botbox_test.go -->
```go
//go:build botbox

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestBotbox(t *testing.T) {
	botbox := exec.Command("bin/botbox", "run", "--target", "targets/toy-widget/target.yaml",
		"--seed", "1", "--runs", "3")
	if deadline, ok := t.Deadline(); ok {
		// Stop botbox before go test's -timeout, which would leave its control plane running.
		left := time.Until(deadline).Truncate(time.Second) - 30*time.Second
		if left <= 0 {
			t.Fatal("go test's -timeout leaves botbox no time")
		}
		botbox.Args = append(botbox.Args, "--deadline", left.String())
	}
	botbox.Dir = "../.." // launch.binary is relative to the repository root.
	botbox.Stdout, botbox.Stderr = os.Stdout, os.Stderr
	if err := botbox.Run(); err != nil {
		t.Fatalf("botbox: %v", err)
	}
}
```

Build `bin/botbox` with the [tools module](#keep-botbox-out-of-your-gomod), build your
controller and set `KUBEBUILDER_ASSETS`. Then run `go test -count=1 -tags botbox ./...`.
`go test` cannot see a change to your controller or `target.yaml`, so `-count=1` stops it from
reusing a cached pass. The build tag keeps the test out of a plain `go test ./...`. Raise
`go test`'s `-timeout` for more runs, because the test stops botbox before it.

botbox has no Go API to call instead.

## Invariants

Seven generic invariants apply to every target. [DESIGN.md](DESIGN.md#6-generic-invariants) states them exactly, with their windows, thresholds and attribution rules.

| ID | Name | Checks |
|---|---|---|
| G1 | Bounded reconciliation | Under an unchanged spec, one quiet window holds no more requests than `thresholds.quiet` allows, zero by default. |
| G2 | No churn | Once converged, the target stops changing the CRs and the objects it manages. |
| G3 | Clean deletion | Deleting a CR removes everything it manages and clears its finalizers. |
| G4 | Convergence | `ready` holds on every CR within `timeouts.settle` of every spec change, `updateFixture` or return of a deleted fixture, and again once a fault stops or the controller is back from a `restart`. A controller waiting to restart, or not yet back, has not converged. |
| G5 | Restart-stable | Restarting the target does not change converged state. |
| G6 | No error loop | The target does not repeat one failing request more than `thresholds.errloop` times. |
| G7 | Self-healing | An object `deleteManaged` deletes exists again, by kind and name, once the run settles. |

[docs/bug-matrix.md](docs/bug-matrix.md) shows which check catches each bug seeded into the toy controller of [DESIGN.md](DESIGN.md#9-toy-target-widget), and CI regenerates it from real runs. Each bug's sequence also runs against the toy with no bug, and CI fails if a check fires there.

## Development and internals

- `make setup` installs the envtest control plane, and `make help` lists every target.
- `make test`, `make test-envtest`, `make test-example` and `make test-example-external-secrets` are the tiers CI runs on every PR.
- `make test-kind` runs the toy through `--kubeconfig` against a kind cluster that it creates and deletes. It needs Docker. The nightly workflow runs the same runs with `make test-kind-runs`.
- `make hunt-cert-manager` and `make hunt-external-secrets` hunt for bugs in the pinned controllers
  for `HUNT_MINUTES` (default 120). Each runs the families in `examples/<example>/sequences/hunt/`,
  then up to `HUNT_RUNS` (default 1000) seeds from `HUNT_SEED` (default 1000) on. It keeps each
  failing run's evidence in `botbox-out/hunt-<example>/`. A run that fails is a candidate to
  triage, not yet a bug: see [DESIGN.md §11](DESIGN.md#11-repo-conventions). No pull request
  runs a hunt.
- A block after `<!-- embed: path -->` holds that file byte for byte, and `make test` enforces it.
- [DESIGN.md](DESIGN.md) is the governing design. Code and docs must not contradict it.
- [docs/journal.md](docs/journal.md) and [docs/spikes/](docs/spikes/) hold the milestone journal and the experiments behind DESIGN.md §15.

## License

Apache-2.0. See [LICENSE](LICENSE).
