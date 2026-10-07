# Spike: a model of a dependency's controller under envtest

Date: 2026-10-07. An adopter's controller creates child CRs of another project's kind and
waits for that project's controller to mark them Ready. Under envtest nothing moves them,
so [G4](../checks.md#g4-convergence) fails. The adoption skill sends an adopter to kind
for a kind that runs Pods, and names no third-party kind. The adopter asked whether a
model of the other controller, drawn from its CRD and docs, could stand in for it, and be
told to misbehave.

## Questions

1. Is there prior art?
2. Does a model change the approach of `DESIGN.md`?
3. Where does a model plug in?

## Prior art

Six papers bound the problem.

- [Welder](https://xudongs.com/pub/sosp26_welder.pdf) (SOSP '26) states the contract this
  needs: a controller is verified against a rely condition and a depend condition, where
  the depend condition is the lower controller's eventual stability, "inspired by
  rely-guarantee reasoning". It verifies Rust controllers. No testing tool instantiates it.
- [Anvil](https://www.usenix.org/system/files/osdi24-sun-xudong.pdf) (OSDI '24) hand-models
  the garbage collector, the StatefulSet controller and the DaemonSet controller, and
  needs a fairness assumption: a peer that keeps changing the object "can adversarially
  keep letting the target controller lose the race". A verifier assumes that away. A
  tester can inject it.
- [Kivi](https://www.usenix.org/system/files/atc24-liu-bingzhe.pdf) (ATC '24)
  model-checks hand-written controller models, "most controllers are modeled based on the
  Kubernetes source code", and states the limit: "If a cluster includes unmodeled
  features, our model may not provide accurate results."
- [Sieve](https://www.usenix.org/system/files/osdi22-sun.pdf) (OSDI '22) and
  [Acto](https://cs.cornell.edu/~legunsen/pubs/GuETAlActoSOSP23.pdf) (SOSP '23) run kind
  with every built-in controller, and model nothing. Acto's future work is "to test
  interdependent operators together".
- [Oat](https://www.usenix.org/system/files/nsdi26-gu.pdf) (NSDI '26) proposes "management
  interfaces as mocks": a state machine with "a collection of bad states", kept honest by
  property-based comparison of the model and the real component.
- An [ISSTA '24 study](https://gaoyu-cn.github.io/paper/2024-issta-operatorbugs.pdf) counts
  4% of operator bugs as involving built-in controllers, and its example is this shape: an
  operator racing the StatefulSet controller on a Pod deletion, blocked forever.

Nothing derives a model from a CRD. Practice has the mechanism without the derivation.

- [KWOK Stages](https://kwok.sigs.k8s.io/docs/user/stages-configuration/) model a
  resource's lifecycle declaratively, for any resource since
  [v0.5.0](https://github.com/kubernetes-sigs/kwok/releases/tag/v0.5.0): a selector of
  CEL expressions, a delay with jitter, a weight for random branching, and steps that patch
  a subresource, emit an event, add or remove a finalizer, delete the object, or apply a
  child object from a template. The shipped
  [PVC stage](https://github.com/kubernetes-sigs/kwok/blob/main/kustomize/stage/volume/fast/pvc-provision.yaml)
  is a fake provisioner for a built-in kind, and the chaos stages take their delay, weight
  and failure reason from annotations on the object.
- [`kwokctl create cluster --runtime binary`](https://kwok.sigs.k8s.io/docs/generated/kwokctl_create_cluster/)
  runs etcd, kube-apiserver, kube-controller-manager, kube-scheduler and kwok as host
  processes, downloading the Kubernetes binaries from `dl.k8s.io` or taking paths to
  installed ones. D57 rejected a controller manager beside envtest: `setup-envtest` ships
  none, and no Pod runs without a kubelet. kwokctl runs both as host processes, and
  [its docs](https://kwok.sigs.k8s.io/docs/examples/scale-with-custom-resource/) show a
  Deployment at `3/3` seconds after two fake nodes join. Not tried here.
- [Crossplane provider-nop](https://github.com/crossplane-contrib/provider-nop) is the
  model of the knobs: `conditionAfter` lists timed condition transitions, and
  `deleteAfter` and `deleteError` model slow and stuck deletion. It fakes only its own kind.
- Test suites write dependency status by hand: the kubebuilder
  [CronJob tutorial](https://book.kubebuilder.io/cronjob-tutorial/writing-tests) sets a
  Job's status, and openstack-k8s-operators'
  [lib-common](https://github.com/openstack-k8s-operators/lib-common/blob/main/modules/common/test/helpers/deployment.go)
  ships `SimulateDeploymentReplicaReady` and `SimulateJobSuccess`.
- Three catalogues say what Ready means: [KEP-1623](https://github.com/kubernetes/enhancements/blob/master/keps/sig-api-machinery/1623-standardize-conditions/README.md)
  conditions, [kstatus](https://github.com/kubernetes-sigs/cli-utils/blob/master/pkg/kstatus/README.md)
  for built-ins and conventional CRDs, and Argo CD's
  [`resource_customizations`](https://github.com/argoproj/argo-cd/tree/master/resource_customizations),
  294 health scripts for 123 groups, each with real status fixtures. These give a model
  its happy path.
- Running kube-controller-manager under envtest is
  [not planned](https://github.com/kubernetes-sigs/controller-runtime/issues/3083).

## Setup

| Component | Version | How obtained |
|---|---|---|
| KWOK stage controller | v0.8.0 | `sigs.k8s.io/kwok/pkg/kwok/controllers` as a Go library |
| envtest control plane | Kubernetes 1.37.0 | `setup-envtest use 1.37.0 -p path` |
| Go | 1.26.0 | this repository's toolchain |

The driver program is `kwok-stage-envtest/main.go` next to this file. It installs a `Bar`
CRD with a status subresource and starts one stage controller over `bars` with two
stages. The first marks a Bar Ready for its generation 200 ms after the Bar lags. The
second marks it Ready False while an annotation says `degraded`. The program creates a
Bar, changes its spec, adds the annotation, removes it, reads the resourceVersion twice
10 s apart, and deletes the Bar. The first stage:

```yaml
spec:
  resourceRef: {apiGroup: example.com/v1, kind: Bar}
  selector:
    matchExpressions:
    - cel: {expression: '!has(self.metadata.deletionTimestamp)'}
    - cel: {expression: '!has(self.metadata.annotations) || !("spike.example.com/mode" in self.metadata.annotations)'}
    - cel: {expression: '!has(self.status) || !has(self.status.conditions) || !self.status.conditions.exists(c, c.type == "Ready" && c.status == "True" && c.observedGeneration == self.metadata.generation)'}
  delay: {durationMilliseconds: 200}
  steps:
  - patch:
      subresource: status
      root: status
      type: merge
      template: |
        observedGeneration: {{ .metadata.generation }}
        conditions:
        - {type: Ready, status: "True", observedGeneration: {{ .metadata.generation }}, reason: Modelled}
```

## Results

Three runs agreed within 20 ms.

| Observation | Value |
|---|---|
| Ready True for generation 1 after the create | 231 ms |
| Ready True for generation 2 after a spec change | 215 ms |
| Ready False for generation 2 after the `degraded` annotation | 215 ms |
| Ready True for generation 2 after the annotation's removal | 212 ms |
| resourceVersion changes between two reads 10 s apart | 0 |
| Bar gone after its delete | within 2 ms; the stages add no finalizer |

The spike ran one deviation, a degraded mode switched by an annotation. It ran no timed
or random one, and bounded none, as a fault's `until` does.

The library's import closure holds no controller-runtime. kwok pins `k8s.io/*` 0.36.1
and builds against the root module's 0.37.0 pins. A stage that patches with
`type: strategic` reads the CRD's OpenAPI v3 document, which the API server publishes a
few hundred milliseconds after the install. A `merge` stage reads none, and the spike
passes no schema.

## What it means for the design

The thesis of §1 holds. The target stays unmodified and is observed only through the API
server. A model is another writer on the API server, as the garbage-collector emulation
of D5 is. Three points move.

- §2 says reconciler-fuzzer is not a mocking framework. A model mocks another controller at the
  API server, not the target's client. The non-goal needs that sentence.
- D57 moves in two ways. kwokctl supplies the controller manager and the fake kubelet
  that D57 found missing, for the workload kinds. A stage model covers a kind no cluster
  runs a controller for.
- Attribution (§6) holds as written. A model writes status on objects the target
  created, which stay the target's for G3, G5 and G7. G2 counts only writes the proxy
  saw, so a model's writes are not churn. A settle wait counts them as changes, so a
  model must go quiet once it converges, as the spike's does.

The cost is in deviations. A model told to flap, or to stay NotReady, breaks G4 for a
correct target unless the window it reaches into is unjudged and the target is owed
recovery time after it, as §6 "Recovery from faults" gives a fault. A deviation should
reuse the fault's `until` triggers and window bookkeeping, not grow a parallel set.

The checking half exists today. A property binds `managed`, so a target can already
declare that a Ready CR owns only Ready children.

A dependency plugs in here:

| Concern | Where | Pattern to copy |
|---|---|---|
| Declaration | `internal/target`, `docs/reference.md` | `fixtures`, `manages`, `ready` |
| Running the model | `internal/run/run.go` starts the collector per run | `internal/cluster/collector.go` |
| Its log | `writeRecordings` in `internal/run/run.go` | `collector.jsonl` |
| Deviation ops | `internal/run/sequence.go`, the `apply` switch in `internal/run/runner.go`, `internal/generate/faults.go` | the `fault` op |
| Excusing a deviation | fault windows in `internal/run/runner.go` and `internal/invariant/window.go` | §6 "Recovery from faults" |
| Contract checks | `properties` today, a generic check later | P1 of both examples |
| The skill | step 2 of `skills/adopt-reconciler-fuzzer/SKILL.md`, or a second skill | `.claude/skills/add-invariant/SKILL.md` |

The declaration might read:

```yaml
dependencies:
  - kind: example.com/v1/Bar
    ready: >-                      # Bar's contract: the model writes it, properties read it
      has(status.conditions) && status.conditions.exists(c, c.type == "Ready"
      && c.status == "True" && c.observedGeneration == metadata.generation)
    after: 200ms
    finalizer: example.com/bar     # optional: the model holds a deletion this long
```

Deviations are generic, so the harness ships them and generation draws them as it draws
faults: `stuck`, `slow`, `flap`, `stale` (Ready for an old generation) and `degraded`
(Ready False with a reason), each bounded by an `until`. A model for a kind that fills
status fields the target reads, such as an address, needs a template, which a stage has.

## Open questions

1. Embed kwok's stage controller, or write a smaller one? The spike shows embedding works
   and costs no controller-runtime import. Its templates and CEL selectors are more than
   the sketch needs, and its versions lag the root module's by one minor.
2. Does a kwokctl cluster pass `--kubeconfig` as kind does? The run waits for the default
   ServiceAccount and `kube-root-ca.crt`, which kube-controller-manager adds. Not tried.
3. How is a model kept honest? Oat's answer is to run the same sequences against the real
   controller on kind, and compare. A nightly tier could do that.

## Rerun

```sh
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 -p path)"
cd docs/spikes/kwok-stage-envtest && go run .
```
