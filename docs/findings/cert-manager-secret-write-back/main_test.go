package main

import "testing"

func TestMechanismTellsHowTheOldKeySurvived(t *testing.T) {
	old := secret{UID: "old-secret", Owner: "old-certificate", Key: "rsa"}
	for _, c := range []struct {
		after secret
		want  string
	}{
		{secret{UID: "new-secret", Owner: "new-certificate", Key: "ecdsa"}, "new key"},
		{secret{UID: "new-secret", Owner: "new-certificate", Key: "rsa"}, "write-back"},
		{secret{UID: "old-secret", Owner: "new-certificate", Key: "rsa"}, "re-point"},
		{secret{UID: "old-secret", Owner: "old-certificate", Key: "rsa"}, "not collected"},
		{secret{}, "no Secret"},
	} {
		if got := mechanism(old, c.after, "new-certificate"); got != c.want {
			t.Errorf("mechanism(%+v, %+v) = %q, want %q", old, c.after, got, c.want)
		}
	}
}
