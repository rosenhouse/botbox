# Spike: cert-manager as a black-box target under envtest

Date: 2026-09-20. Decisions it fed: D3, D5, D8, D9, D10, D17 in `DESIGN.md` §15.

## Question

Can an unmodified cert-manager controller run against a bare envtest API server, with
no webhook installed, and behave the way the harness expects: converge, stay quiet,
survive a crash, and clean up?

## Setup

| Component | Version | How obtained |
|---|---|---|
| cert-manager controller | v1.21.2 | shallow `git clone --depth 1 --branch v1.21.2`, then `go build` in `cmd/controller` |
| cert-manager CRDs | v1.21.2 | release asset `cert-manager.crds.yaml` (997,110 bytes, 6 CRDs) |
| envtest control plane | Kubernetes 1.37.0 | `setup-envtest use 1.37.0 -p path` |
| Go | 1.26.8 | downloaded by `GOTOOLCHAIN=auto`; cert-manager's `go.mod` says `go 1.26.0` |

Controller flags: `--kubeconfig=<file> --leader-elect=false
--enable-certificate-owner-ref=<true|false> --metrics-listen-address=127.0.0.1:19402 --v=2`.
The kubeconfig came from envtest's `AddUser`, so this is the same hand-off the Binary
launcher will do (through the proxy, later).

The driver program is `cert-manager-envtest/main.go` next to this file. It creates a
namespace, a self-signed `Issuer` (the fixture), and a `Certificate`; waits for
`Ready=True` with the condition's `observedGeneration` equal to `metadata.generation`;
watches resourceVersions for 20 s; adds a DNS name; SIGKILLs and re-execs the controller;
deletes the Certificate; deletes the namespace.

## Results

Two runs, identical except for the owner-reference flag.

| Observation | owner-ref=true | owner-ref=false |
|---|---|---|
| envtest up | 3.2 s | 3.4 s |
| Certificate Ready after controller start | 2.28 s | 2.28 s |
| Ready again after `dnsNames` change | 0.26 s | 0.26 s |
| resourceVersion changes over 20 s of stable spec | 0 | 0 |
| After SIGKILL + re-exec: revision, Secret, CertificateRequest | unchanged | unchanged |
| Issued Secret `c1-tls` ownerReferences | 1 | 0 |
| Issued Secret label `controller.cert-manager.io/fao` | `true` | `true` |
| CertificateRequest ownerReferences | 1 | 1 |
| 10 s after deleting the Certificate | Secret and CertificateRequest still present | same |
| 5 s after deleting the namespace | `Terminating` | `Terminating` |

CertificateRequests are named `<certificate>-<revision>` (`c1-1`, then `c1-2`), and
cert-manager deleted the superseded one itself. The temporary private-key Secret that
cert-manager creates with `generateName` was already gone by the time the Certificate was
Ready.

## What this means for the design

- **It works.** cert-manager needs no webhook, no leader election and no cluster-scoped
  namespace to issue self-signed certificates under envtest. Convergence is fast enough
  that the §6 defaults (`T_settle` 30 s) leave a wide margin.
- **No garbage collection, no namespace finalization.** envtest has no
  kube-controller-manager. Objects with ownerReferences outlive their owner, and namespaces
  never leave `Terminating`. G3 and the namespace-per-run cleanup in the first design draft
  could not work as written. Hence GC emulation and self-cleanup (D5).
- **Readiness needs CEL.** The Certificate has no top-level `status.observedGeneration`;
  the field lives inside each condition. The predicate is
  `status.conditions.exists(c, c.type == "Ready" && c.status == "True" && c.observedGeneration == metadata.generation)` (D3).
- **The negative control is real.** With the owner-ref flag off the Secret is retained by
  design. Declaring Secrets as managed makes that a G3 finding, which gives CI a failure
  to assert on (D17).
- **Fixtures are required.** A Certificate cannot be created without an Issuer to point
  at, and no schema can invent a valid `issuerRef` (D8).

## Gotchas for the M4 implementer

- `cmd/controller` is a nested Go module named
  `github.com/cert-manager/cert-manager/controller-binary` with
  `replace github.com/cert-manager/cert-manager => ../../` and no tag of its own.
  `go install ...@v1.21.2` and `go mod download` both refuse it. Clone and build (D10).
  Measured here: 2 s to clone, 91 s to build cold including module downloads, 0 s warm.
- `deploy/crds/*.yaml` inside the Go module are marked "for reference, development and
  testing purposes only" by their README. Use the release asset instead.
- v1.21.2 has `--metrics-listen-address` but no `--healthz-listen-address`; the first run
  failed on the unknown flag.
- The Claude Code web sandbox reaches `proxy.golang.org` and `github.com` but not
  `quay.io`, so the controller image is not an option for local runs.

## Rerun

```sh
git clone --depth 1 --branch v1.21.2 https://github.com/cert-manager/cert-manager.git /tmp/cm
(cd /tmp/cm/cmd/controller && GOTOOLCHAIN=auto go build -o /tmp/cert-manager-controller .)
curl -sSL -o /tmp/cert-manager.crds.yaml \
  https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.crds.yaml
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 -p path)"
cd docs/spikes/cert-manager-envtest && go run . /tmp/cert-manager.crds.yaml /tmp/cert-manager-controller true
```
