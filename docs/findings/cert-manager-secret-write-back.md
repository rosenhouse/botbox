# cert-manager v1.21.2 can give a recreated Certificate the deleted Certificate's private key

This draft is for botbox's maintainer to file at cert-manager. Nobody has searched
cert-manager's tracker for it. No agent may file it. The issue body follows the rule. Its
links are relative to this file, so they need botbox's URL before filing.

---

🤖 Created by Claude 🤖

## Summary

botbox, a black-box test harness for Kubernetes controllers, found this in cert-manager
v1.21.2 with `--enable-certificate-owner-ref=true`.

A user deletes a Certificate and at once creates one with the same name and `secretName`.
Sometimes the new Certificate's Secret then holds the deleted Certificate's private key. It
gets there in one of two ways:

- **Write-back.** The garbage collector deletes the Secret. The issuing controller then
  applies the data of the Secret it read before the delete, with the new Certificate as
  owner. The apply creates the Secret again, with a new UID and the old key.
- **Re-point.** The issuing controller's apply lands before the garbage collector's delete.
  The Secret then names a live owner, so the garbage collector keeps it.

What follows depends on the new Certificate's spec:

- If it matches the old spec, the new Certificate becomes Ready with the old key. This
  happens under `rotationPolicy: Always`, the default, as under `Never`.
- If it asks for another key algorithm under `Never`, the new Certificate stays
  `Ready=False` with reason `SecretMismatch`, waiting for a user.

So a user who recreates a Certificate to get a new key can keep the old one.

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
- Under `Never`, the key manager generates a key only when the Secret is gone or holds no
  key. Otherwise it reuses the stored key, or waits for a user when that key does not fit
  the spec (`pkg/controller/certificates/keymanager/keymanager_controller.go:248-272`).

kube-controller-manager's garbage collector reads the dependent, checks each owner, and
deletes it with UID and resourceVersion preconditions. It keeps a dependent that names a
live owner. See Kubernetes v1.37.0,
`pkg/controller/garbagecollector/garbagecollector.go:521-577` and `:654`, and
`operations.go:53-80`.

## Documented behaviour

- The help for `--enable-certificate-owner-ref`
  (`cmd/controller/app/options/options.go:181-183`): "When this flag is enabled, the secret
  will be automatically removed when the certificate resource is deleted."
- The comment where `ensureSecretData` finds no Secret (`secret_manager.go:42-43`): "Secret
  doesn't exist so we can't do anything. The Certificate will be marked for a re-issuance
  and the resulting Secret will be evaluated again."
- The doc of `rotationPolicy` (`pkg/apis/certmanager/v1/types_certificate.go:353-356`): "If
  set to `Never`, a private key will only be generated if one does not already exist in the
  target `spec.secretName`."

The write-back contradicts the first two. The Secret is removed, and cert-manager creates it
again from data it read before the removal. The new Certificate then finds a key that its
`secretName` should no longer hold.

The re-point may be intended. `CertificateOwnsSecret` decides which Certificate owns a
Secret by the `cert-manager.io/certificate-name` annotation, not by ownerReferences
(`internal/controller/certificates/certificates.go:32-46`). It still breaks the flag's
promise, because the Secret outlives the Certificate it was issued for.

## Reproduction

[`cert-manager-secret-write-back/main.go`](cert-manager-secret-write-back/main.go) needs a
cluster with cert-manager's CRDs and cert-manager running with
`--enable-certificate-owner-ref=true`. Each run makes a namespace and a self-signed Issuer,
then:

1. creates Certificate `example` (`secretName: example-tls`, the default RSA key, the
   `-policy` given) and waits for Ready;
2. records the Secret's UID and the sha256 of its `tls.key`;
3. deletes the Certificate and polls until it is gone;
4. at once creates `example` again with the `-algorithm` and `-policy` given;
5. waits 30 s for Ready, then reads the Secret's UID, controller owner and key hash.

A run keeps the key when the Secret still holds the old one. The UID tells the mechanism: a
new UID is a write-back, and the old UID owned by the new Certificate is a re-point. The
program never writes the Secret itself. From a botbox checkout:

```sh
go run ./docs/findings/cert-manager-secret-write-back -kubeconfig "$KUBECONFIG" -runs 1 -algorithm RSA -policy Always
```

cert-manager ran out of cluster with `--leader-elect=false`.

| cluster | garbage collector | new Certificate | runs | key kept | write-back | re-point |
| --- | --- | --- | --- | --- | --- | --- |
| kind v0.33.0, node v1.37.0 | kube-controller-manager | `Never`, ECDSA | 72 | 4 | 2 | 2 |
| kind v0.33.0, node v1.37.0 | kube-controller-manager | `Never`, RSA | 28 | 6 | 5 | 1 |
| kind v0.33.0, node v1.37.0 | kube-controller-manager | `Always`, RSA | 18 | 11 | 11 | 0 |
| envtest 1.37.0 | the program's `-collect` | `Never`, ECDSA | 10 | 8 | 0 | 8 |
| envtest 1.37.0, through botbox | botbox's | `Never`, ECDSA: [`sequence.json`](cert-manager-secret-write-back/sequence.json) | 8 | 3 | 3 | 0 |
| envtest 1.37.0, through botbox | botbox's | `Never`, ECDSA: the hunt's seed 1043 | 17 | 5 | 4 | 1 |

- With ECDSA, every run that kept the key ended `Ready=False` with reason `SecretMismatch`.
  With RSA, every one ended `Ready=True`. No run ended with the old Secret uncollected.
- On kind, every kept key came on the first recreate after cert-manager started: 21 of 35
  such runs kept it, and none of the 83 later runs did. Restart cert-manager before each run
  to see it.
- envtest runs no garbage collector. There, `-collect` starts once the old Certificate is
  deleted. It lists the Secret and the CertificateRequests that Certificate controls, and
  deletes each with UID and resourceVersion preconditions. A version of `-collect` that
  deleted the Secret before the create began kept the key in none of 20 runs. One that
  started with the create kept it in 9 of 10, all by re-point.
- botbox's garbage collector also deletes with UID and resourceVersion preconditions. Its
  proxy sits between cert-manager and the API server. Seed 1043's first two ops are the
  recreate above with other `dnsNames` and `duration`. The hunt run that found it kept the
  key by re-point.

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

On kind, with `-runs 1 -algorithm RSA -policy Always` and a fresh cert-manager for each
run, the stock and the patched controller took turns for 20 runs:

- The stock controller kept the key in 6 of 10 runs, all by write-back. One more run ended
  `Ready=True` with no Secret.
- The patched controller kept it in none of 10. In 8 of them it logged the Conflict above,
  and then generated a new key.

Against kube-apiserver 1.37.0, a forced apply of a Secret behaved like this:

- with the live Secret's UID, it updated the Secret;
- with the UID of a deleted Secret, it failed with that Conflict and created nothing;
- with the UID of a Secret that another of the same name had replaced, it failed with
  `metadata.uid: field is immutable`.

The patch does not stop the re-point. A fix for that would have cert-manager leave a
Secret alone while its controller ownerReference names another UID, so that the garbage
collector deletes it.

## Unknown

- Nobody has searched cert-manager's tracker for this.
- cert-manager may mean to adopt a Secret whose owner was deleted.
- On kind, only the first recreate after cert-manager started kept the key. The cause is
  unknown.
- No run put cert-manager in cluster or under leader election.
- With the patch, the tests of `pkg/controller/certificates/issuing/...` pass. No other
  cert-manager test ran against it.
