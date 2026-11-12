package selfupdate

import (
	"crypto/ed25519"
	"math"
	"strings"
	"testing"
)

func TestUpdateVerifySignedNoncanonicalAndMissingFieldsRejected(t *testing.T) {
	m, _, pub, priv, now := updateFixture(t)
	data, _ := signedUpdate(t, m, priv)
	cases := []string{
		strings.Replace(string(data), `"schema"`, `"\u0073chema"`, 1),
		strings.Replace(string(data), `"sequence":2,`, "", 1),
		strings.Replace(string(data), `"artifact_bytes":23`, `"artifact_bytes":0`, 1),
		strings.Replace(string(data), `"version":"1.0.1"`, `"version":"1.0.1","Version":"9.9.9"`, 1),
	}
	// Construct zero-sized artifact explicitly instead of depending on fixture length.
	zero := m
	zero.ArtifactBytes = 0
	b, _ := signedUpdate(t, zero, priv)
	cases[2] = string(b)
	for _, raw := range cases {
		sig := ed25519.Sign(priv, []byte(raw))
		if _, e := verifyUpdateManifest([]byte(raw), sig, pub, 1, now, m.OS, m.Arch); e == nil {
			t.Fatalf("signed malformed manifest accepted: %s", raw)
		}
	}
}
func TestUpdateVerifySequenceCannotWrapAndSignatureBindsArtifact(t *testing.T) {
	m, _, pub, priv, now := updateFixture(t)
	m.Sequence = math.MaxUint64
	data, sig := signedUpdate(t, m, priv)
	if _, e := verifyUpdateManifest(data, sig, pub, math.MaxUint64, now, m.OS, m.Arch); e == nil {
		t.Fatal("max sequence rollback/wrap accepted")
	}
	changed := strings.Replace(string(data), m.ArtifactSHA256, strings.Repeat("0", 64), 1)
	if _, e := verifyUpdateManifest([]byte(changed), sig, pub, 1, now, m.OS, m.Arch); e == nil {
		t.Fatal("artifact substitution escaped signature binding")
	}
	if _, e := verifyUpdateManifest(data, sig, pub, 1, now.AddDate(0, 0, 1), m.OS, m.Arch); e == nil {
		t.Fatal("expired manifest accepted")
	}
}
