# cert-manager v1.21.2 can give a recreated Certificate the deleted Certificate's private key

This draft is for botbox's maintainer to file at cert-manager. The agents that wrote it may
not search cert-manager's tracker, so the maintainer searches it before filing. No agent may
file it. The issue body starts below the horizontal rule. Its links are relative to this
file, so they need botbox's URL before filing.

---

🤖 Created by Claude 🤖

## Summary

botbox, a black-box test harness for Kubernetes controllers, found this in cert-manager
v1.21.2 with `--enable-certificate-owner-ref=true`.

A client deletes a Certificate with the default background cascade. Within about 10 ms it
creates one with the same name and `secretName`. The new Certificate's Secret then often
holds the deleted Certificate's private key. It gets there in one of two ways:

- **Write-back.** The garbage collector deletes the Secret. The issuing controller then
  applies the data of the Secret still in its informer cache, with the new Certificate as
  owner. The apply creates the Secret again, with a new UID and the old key.
- **Re-point.** The issuing controller's apply lands before the garbage collector's delete.
  The Secret then names a live owner, so the garbage collector keeps it.

What follows depends on the new Certificate's spec:

- If the spec is unchanged, the new Certificate becomes Ready with the old key. Under
  `rotationPolicy: Always`, the default, cert-manager issues nothing, so the policy never
  applies, and the Secret keeps the deleted Certificate's certificate too.
- If a field such as `dnsNames` changes, cert-manager issues a new certificate. Under
  `Never` it signs the old key. Under `Always` it generates a new key.
- If the spec asks for another key algorithm under `Never`, the new Certificate stays
  `Ready=False` with reason `SecretMismatch`, waiting for a user.

So a user who recreates a Certificate to get a new key can keep the old one.

On an idle kind cluster the window is about 10 ms, too short for `kubectl replace --force`.
A foreground cascade (`kubectl delete --cascade=foreground`) closes it. The old Certificate
then goes only after the garbage collector has deleted its Secret.

## Code path

Paths are relative to cert-manager v1.21.2.

- While a Certificate is not `Issuing`, the issuing controller calls `ensureSecretData`
  (`pkg/controller/certificates/issuing/issuing_controller.go:198-206`).
- `ensureSecretData` reads the Secret from the informer's cache
  (`pkg/controller/certificates/issuing/secret_manager.go:40`).
- `SecretOwnerReferenceMismatch` finds no ownerReference with the new Certificate's UID
  (`internal/controller/certificates/policies/checks.go:756-790`). cert-manager logs
  `applying Secret data` with the message `unexpected Secret Owner Reference value on
  Secret --enable-certificate-owner-ref=true`, and calls `UpdateData` with the data it
  read (`secret_manager.go:98-99`).
- `UpdateData` server-side-applies that data with `Force: true` and one controller
  ownerReference to the new Certificate
  (`pkg/controller/certificates/issuing/internal/secret.go:108-127`). Its doc says "If the
  Secret resource does not exist, it will be created on Apply" (`internal/secret.go:92`).

Whether the new Certificate is issued depends on the trigger policy chain
(`internal/controller/certificates/policies/policies.go:71-84`):

- With an unchanged spec, the old Secret passes every check. No CertificateRequest belongs
  to the new Certificate, so `CurrentCertificateRequestMismatchesSpec` compares the Secret's
  certificate with the spec instead (`checks.go:214-223`). The readiness chain
  (`policies.go:89-102`) passes the same way, so the Certificate becomes Ready. Only
  `SecretDoesNotExist` can start an issuance, when the trigger controller runs while the
  Secret is gone.
- The key manager acts only while a Certificate is `Issuing`
  (`pkg/controller/certificates/keymanager/keymanager_controller.go:178-187`), so
  `rotationPolicy` never applies.
- When the spec changes, the trigger chain starts an issuance. Under `Never`, the key
  manager then reuses the stored key, or waits for a user when that key does not fit the
  spec (`keymanager_controller.go:248-281`).

kube-controller-manager's garbage collector reads the dependent, checks each owner, and
deletes it with UID and resourceVersion preconditions. After a Conflict, it retries without
the resourceVersion only if the ownerReferences did not change. It keeps a dependent that
names a live owner. See Kubernetes v1.37.0,
`pkg/controller/garbagecollector/garbagecollector.go:521-577` and `:654`, and
`operations.go:53-80`.

## Documented behaviour

- The help for `--enable-certificate-owner-ref`
  (`cmd/controller/app/options/options.go:181-183`): "When this flag is enabled, the secret
  will be automatically removed when the certificate resource is deleted."
- The doc of `rotationPolicy` (`pkg/apis/certmanager/v1/types_certificate.go:353-356`): "If
  set to `Never`, a private key will only be generated if one does not already exist in the
  target `spec.secretName`."

Under the default background cascade, the garbage collector does delete the Secret. The
write-back then creates it again from cert-manager's cache, which breaks the flag's promise.
The comment where `ensureSecretData` finds no Secret (`secret_manager.go:42-43`) states the
intent: "Secret doesn't exist so we can't do anything." The stale cache never reaches that
branch, because the lister still returns the deleted Secret.

The re-point may count as the race a background cascade leaves open.
`CertificateOwnsSecret` decides which Certificate owns a Secret by the
`cert-manager.io/certificate-name` annotation, not by ownerReferences
(`internal/controller/certificates/certificates.go:32-46`). Still, the Secret outlives the
Certificate it was issued for.

## Reproduction

[`cert-manager-secret-write-back/main.go`](cert-manager-secret-write-back/main.go) needs a
cluster with cert-manager's CRDs and cert-manager running with
`--enable-certificate-owner-ref=true`. Each run makes a namespace and a self-signed Issuer.
Then:

1. It creates Certificate `example` with `secretName: example-tls`, DNS name
   `example.test`, the default RSA key and the `-policy` given, and waits for Ready.
2. It records the Secret's UID and the sha256 of its `tls.key` and `tls.crt`.
3. It deletes the Certificate with the `-propagation` given, `Background` by default, and
   polls every 10 ms until the Certificate is gone.
4. It waits for `-pause`, none by default. Then it creates `example` again with the
   `-algorithm`, `-policy` and `-dns` given. Its client has no rate limit, so the create
   follows within milliseconds.
5. It waits up to 30 s for Ready and up to 30 s more for the Secret. Then it reads the
   Secret's UID, controller owner and hashes.

A run keeps the key when the Secret still holds the old one. The UID tells the mechanism: a
new UID is a write-back, and the old UID owned by the new Certificate is a re-point.
Without `-collect`, the program never writes the Secret. From a botbox checkout:

```sh
go run ./docs/findings/cert-manager-secret-write-back -kubeconfig "$KUBECONFIG" -runs 10 -algorithm RSA -policy Always
```

The cluster was kind v0.33.0 with node v1.37.0. cert-manager ran out of cluster with
`--leader-elect=false`. Each batch of 10 runs had one cert-manager process and one
reproducer process, and the batches took turns.

| flags beside `-runs 10` | runs | key kept | write-back | re-point |
| --- | --- | --- | --- | --- |
| `-policy Always -algorithm RSA` | 30 | 17 | 16 | 1 |
| `-policy Never -algorithm RSA` | 30 | 17 | 15 | 2 |
| `-policy Never -algorithm ECDSA` | 30 | 19 | 10 | 9 |
| `-policy Never -algorithm RSA -dns other.test` | 20 | 7 | 6 | 1 |
| `-policy Always -algorithm RSA -dns other.test` | 20 | 0 | 0 | 0 |
| `-policy Always -algorithm RSA -propagation Foreground` | 20 | 0 | 0 | 0 |

- With the same spec under `Always`, every kept key came with the deleted Certificate's
  `tls.crt`, byte for byte, and `Ready=True`. Under `Never`, 14 of 17 did, and 3 got a new
  `tls.crt` for the old key.
- With another DNS name under `Never`, every kept key came with a new `tls.crt` and
  `Ready=True`.
- With ECDSA under `Never`, every kept key ended `Ready=False` with reason
  `SecretMismatch`.
- Under the foreground cascade, cert-manager never logged `applying Secret data`.
- In the four rows that kept keys, the first run of a batch kept it in 7 of 11, and later
  runs in 53 of 99.
- No run ended with the old Secret uncollected.

The window is short. With `-policy Always -algorithm RSA`, these pauses before the create
kept the key in:

- none: 40 of 70 runs, the first row and the batches interleaved with the pauses below;
- 5 ms: 3 of 20;
- 10 ms: 1 of 20;
- 20 ms, 50 ms, 100 ms and 200 ms: none of 20 each.

`kubectl replace --force` (kubectl v1.37.0) deletes, reads the Certificate gone, and creates
it 35 to 55 ms after the delete. With the first row's Certificate, it kept the key in none
of 20 tries.

## How botbox found it

botbox ran cert-manager against envtest, which has no garbage collector. botbox's own
collector deletes dependents with UID and resourceVersion preconditions, within 1 s of the
owner's deletion. Hunt seed 1043 recreates a `Never` Certificate with ECDSA and other
`dnsNames` and `duration`. It kept the key in 5 of 17 replays: 4 by write-back and 1 by
re-point. [`sequence.json`](cert-manager-secret-write-back/sequence.json), the recreate in
the table's ECDSA row, kept it in 3 of 8 botbox runs, all by write-back.

On a cluster without a garbage collector, the program's `-collect` deletes what the old
Certificate controls, as kube-controller-manager would. On envtest with
`-policy Always -algorithm RSA`, it kept the key in 7 of 10 runs.

## Possible fix

`ensureSecretData` can hand `UpdateData` the UID of the Secret it read at
`secret_manager.go:40`, and `UpdateData` can put that UID in the apply. An apply that names
a UID and finds no object fails with Conflict: "uid mismatch: the provided object specified
uid %s, and no existing object was found" (k8s.io/apiserver v0.37.0,
`pkg/endpoints/handlers/patch.go:612-618`). The controller's retry then reads that the
Secret is gone. Issuance (`issuing_controller.go:464`) passes no UID, so it still creates a
missing Secret.

```diff
--- a/pkg/controller/certificates/issuing/internal/secret.go
+++ b/pkg/controller/certificates/issuing/internal/secret.go
@@ -25,6 +25,7 @@
 	corev1 "k8s.io/api/core/v1"
 	apierrors "k8s.io/apimachinery/pkg/api/errors"
 	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
+	"k8s.io/apimachinery/pkg/types"
 	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
 	applymetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
 	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
@@ -66,6 +67,9 @@
 	PrivateKey, Certificate, CA         []byte // #nosec G117 -- holds runtime certificate material; not a hardcoded secret
 	CertificateName                     string
 	IssuerName, IssuerKind, IssuerGroup string
+	// UID, if set, is the UID of the Secret the data was read from. The apply
+	// then fails rather than create a Secret that has since been deleted.
+	UID types.UID
 }
 
 // NewSecretsManager returns a new SecretsManager. Setting
@@ -122,6 +126,10 @@
 		})
 	}
 
+	if data.UID != "" {
+		applyCnf = applyCnf.WithUID(data.UID)
+	}
+
 	log.V(logf.DebugLevel).Info("applying secret")
 
 	_, err = s.secretClient.Secrets(secret.Namespace).Apply(ctx, applyCnf, applyOpts)
--- a/pkg/controller/certificates/issuing/secret_manager.go
+++ b/pkg/controller/certificates/issuing/secret_manager.go
@@ -73,6 +73,7 @@
 		IssuerName:      secret.Annotations[cmapi.IssuerNameAnnotationKey],
 		IssuerKind:      secret.Annotations[cmapi.IssuerKindAnnotationKey],
 		IssuerGroup:     secret.Annotations[cmapi.IssuerGroupAnnotationKey],
+		UID:             secret.UID,
 	}
 
 	// Check whether the Certificate's Secret has correct output format and
```

A build of v1.21.2 with this patch took turns with the stock batches above, with
`-policy Always -algorithm RSA`. It kept the key in none of 20 runs, where the stock build
kept it in 17 of 30. In 12 of the 20 it logged the Conflict above, and then generated a new
key.

Against kube-apiserver 1.37.0, a forced apply of a Secret behaved like this:

- with the live Secret's UID, it updated the Secret;
- with the UID of a deleted Secret, it failed with that Conflict and created nothing;
- with the UID of a Secret that another of the same name had replaced, it failed with
  `metadata.uid: field is immutable`.

The patch does not stop the re-point. A fix for that would have cert-manager leave a
Secret alone while its controller ownerReference names another UID, so that the garbage
collector deletes it.

## Unknown

- cert-manager may mean to adopt a Secret whose owner was deleted.
- Every run used an idle cluster. A garbage collector that lags, as on a busy cluster, may
  widen the window. No run measured that.
- No run put cert-manager in cluster or under leader election.
- Only the tests of `pkg/controller/certificates/issuing/...` ran against the patch, and
  they pass.
