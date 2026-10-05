# Real controllers

`examples/` holds targets for two real controllers. Each is built from its own source at a
pinned tag and runs unmodified. Each also carries a negative control: a configuration that
must fail.

## cert-manager

`examples/cert-manager/` drives [cert-manager](https://github.com/cert-manager/cert-manager)
v1.21.2. Nothing is patched into it: the CRDs are its release asset, pinned by sha256, and the
flags are its own. From a clean checkout, this script is the whole run:

<!-- embed: examples/cert-manager/quickstart.sh -->
```sh
#!/bin/sh
# Exercise cert-manager with botbox. It builds what it needs, so a clean checkout
# is enough. Arguments go to botbox: --seed picks the sequences it draws, and a
# later --runs wins over the one here.
# Exit codes: 0, all runs passed; 1, a check failed; 2, could not test.
set -eu
cd "$(dirname "$0")/../.."

# cert-manager binds its healthz server to port 9403, so its runs collide.
if ! command -v lsof >/dev/null; then
  echo "lsof is missing, so nothing checked whether port 9403 is free." >&2
elif lsof -nP -iTCP:9403 -sTCP:LISTEN >/dev/null; then
  echo "port 9403 is bound. cert-manager listens there, so its runs cannot overlap." >&2
  exit 2
fi

go build -o bin/botbox ./cmd/botbox || exit 2
make --no-print-directory cert-manager || exit 2

KUBEBUILDER_ASSETS="$(make --no-print-directory assets-path)" ./bin/botbox run \
  --target examples/cert-manager/target.yaml --runs 5 "$@"
```

The first invocation installs `setup-envtest`, downloads the control plane and builds
cert-manager, which takes a few minutes. The five runs then take about three minutes together.
Each applies the Issuer fixture to a fresh namespace, launches the controller behind the proxy,
and runs one drawn sequence. A fixed seed draws the same five sequences every time:

```sh
examples/cert-manager/quickstart.sh --seed 23
```

```
the deadline is 25m20s: these 5 runs can take 21m20s at the target's timeouts, and minimizing a failure gets the rest, at least 4m0s. --deadline sets another.
run 1: seed 23, generated
run 2: seed 24, generated
run 3: seed 25, generated
run 4: seed 26, generated
run 5: seed 27, generated
every run passed.
```

botbox derives the deadline from the `timeouts` your target declares, or their defaults. It
allows the longest the runs' waits can take, and at least 4 minutes to minimize a failure. A
correct controller finishes well inside it.

### The negative control

With `--enable-certificate-owner-ref=false`, which `--launch-arg` appends to `launch.args`,
cert-manager leaves the issued Secret behind, as upstream documents. The target declares
`v1/Secret` as managed, so G3 has to report it:

```sh
examples/cert-manager/quickstart.sh --seed 23 --runs 1 --deadline 5m --launch-arg --enable-certificate-owner-ref=false
```

```
run 1: seed 23, generated
run 1: G3 the v1/Secret example-tls was still there 1m0s (timeouts.delete) after example, the last CR it may belong to, was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-21T05:59:08.980624165Z; 1 version, the first v1/Secret example-tls
  the evidence is in botbox-out/20260921T055744Z-23/run-1
  the sequence is 1 op, in botbox-out/20260921T055744Z-23/run-1/sequence.json
```

Seed 23 draws a single op, so there is nothing to minimize. botbox cuts a longer sequence to the
ops the failure needs before it reports. Each removal it tries replays a whole run. A derived
deadline gives minimizing at least 4 minutes, and a longer `--deadline` gives it more.

`make test-example` runs this control. It fails unless the default configuration passes, the
control fails on G3 naming that Secret, and the control's evidence hides the Secret's private
key. It also runs each `examples/cert-manager/sequences/*.json` as written, so none can rot.
Drawn sequences set `spec.privateKey.rotationPolicy` only to `Always`, because under `Never`
cert-manager waits for a user once a later op changes the algorithm. `rotation-never.json` runs
`Never` instead. A nightly workflow draws its own seeds.

## external-secrets

`examples/external-secrets/` drives
[external-secrets](https://github.com/external-secrets/external-secrets) v2.11.0, pinned and
built from its own source the same way:

```sh
examples/external-secrets/quickstart.sh --seed 23
```

It shows three things cert-manager does not.

- Every port this controller binds is ephemeral, so two runs may overlap, and the quickstart
  needs no port guard.
- An ExternalSecret carries no `observedGeneration`. Its `ready` reads the generation from a
  version string instead: `status.syncedResourceVersion` is `"<generation>-<hash>"`, and the
  predicate matches its prefix.
- The negative control is a sequence rather than a flag. No flag makes the controller orphan
  the Secret it manages, but `spec.target.creationPolicy: Orphan` in the CR does.

`make test-example-external-secrets` runs the drawn sequences, then the pinned ones, then that
control. It fails unless the control reports G3 and its evidence hides the Secret's value:

```
run 1: seed 20260922, sequence examples/external-secrets/sequences/orphan.json
run 1: G3 the v1/Secret example-secret was still there 1m0s (timeouts.delete) after example, the last CR it may belong to, was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-22T16:43:05.836050746Z; 1 version, the first v1/Secret example-secret
  the evidence is in botbox-out/external-secrets-control/20260922T164150Z-20260922/run-1
```
