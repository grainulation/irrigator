package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func updateFixture(t *testing.T) (updateManifest, []byte, ed25519.PublicKey, ed25519.PrivateKey, time.Time) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("signed fixture artifact")
	digest := sha256.Sum256(artifact)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	m := updateManifest{Schema: updateManifestSchema, Sequence: 2, Version: "1.0.1", OS: runtime.GOOS, Arch: runtime.GOARCH, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), ArtifactSHA256: hex.EncodeToString(digest[:]), ArtifactBytes: int64(len(artifact))}
	return m, artifact, pub, priv, now
}
func signedUpdate(t *testing.T, m updateManifest, priv ed25519.PrivateKey) ([]byte, []byte) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data, ed25519.Sign(priv, data)
}
func TestUpdateVerificationPolicy(t *testing.T) {
	m, _, pub, priv, now := updateFixture(t)
	data, sig := signedUpdate(t, m, priv)
	if _, err := verifyUpdateManifest(data, sig, pub, 1, now, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*updateManifest){
		"schema":        func(m *updateManifest) { m.Schema = "other" },
		"rollback":      func(m *updateManifest) { m.Sequence = 1 },
		"oldsequence":   func(m *updateManifest) { m.Sequence = 0 },
		"platform":      func(m *updateManifest) { m.OS = "wrong" },
		"arch":          func(m *updateManifest) { m.Arch = "wrong" },
		"expired":       func(m *updateManifest) { m.ExpiresAt = now.Format(time.RFC3339) },
		"offsetexpiry":  func(m *updateManifest) { m.ExpiresAt = "2026-10-23T13:00:00+00:00" },
		"digest":        func(m *updateManifest) { m.ArtifactSHA256 = strings.Repeat("A", 64) },
		"negative-size": func(m *updateManifest) { m.ArtifactBytes = -1 },
		"oversized":     func(m *updateManifest) { m.ArtifactBytes = updateArtifactLimit + 1 },
		"version":       func(m *updateManifest) { m.Version = "../release" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := m
			mutate(&candidate)
			data, sig := signedUpdate(t, candidate, priv)
			if _, err := verifyUpdateManifest(data, sig, pub, 1, now, runtime.GOOS, runtime.GOARCH); err == nil {
				t.Fatal("invalid update accepted")
			}
		})
	}
	badSig := append([]byte{}, sig...)
	badSig[0] ^= 1
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, pair := range []struct{ sig, key []byte }{{badSig, pub}, {sig, other}, {nil, pub}, {sig, pub[:31]}} {
		if _, err := verifyUpdateManifest(data, pair.sig, pair.key, 1, now, runtime.GOOS, runtime.GOARCH); err == nil {
			t.Fatal("invalid authentication accepted")
		}
	}
}
func TestUpdateManifestAmbiguityRejected(t *testing.T) {
	m, _, pub, priv, now := updateFixture(t)
	canonical, _ := signedUpdate(t, m, priv)
	cases := []string{
		string(canonical[:len(canonical)-1]) + `,"sequence":3}`,
		strings.Replace(string(canonical), `"sequence"`, `"Sequence"`, 1),
		strings.Replace(string(canonical), `"sequence":2`, `"sequence":null`, 1),
		string(canonical[:len(canonical)-1]) + `,"public_key":"attacker"}`,
		string(canonical) + "{}", string(canonical) + "\n", "null", "[]",
		strings.Replace(string(canonical), `"sequence":2`, `"sequence":2.0`, 1),
	}
	for _, raw := range cases {
		data := []byte(raw)
		sig := ed25519.Sign(priv, data)
		if _, err := verifyUpdateManifest(data, sig, pub, 1, now, runtime.GOOS, runtime.GOARCH); err == nil {
			t.Fatalf("ambiguous manifest accepted %s", raw)
		}
	}
}
func TestUpdateArtifactAndUnsafeFiles(t *testing.T) {
	m, artifact, _, _, _ := updateFixture(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "artifact")
	if err = os.WriteFile(name, artifact, 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyUpdateArtifact(name, m); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte("tampered fixture bytes"), append(artifact, 'x'), artifact[:len(artifact)-1]} {
		os.WriteFile(name, data, 0600)
		if err = verifyUpdateArtifact(name, m); err == nil {
			t.Fatal("altered artifact accepted")
		}
	}
	os.WriteFile(name, artifact, 0600)
	link := filepath.Join(dir, "link")
	if err = os.Symlink(name, link); err != nil {
		t.Fatal(err)
	}
	if _, err = readUpdateFile(link, 4096); err == nil {
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(dir, "fifo")
	if err = syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readUpdateFile(fifo, 4096); err == nil {
		t.Fatal("fifo accepted")
	}
	ancestor := filepath.Join(dir, "alias")
	os.Symlink(dir, ancestor)
	if _, err = readUpdateFile(filepath.Join(ancestor, "artifact"), 4096); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
}
func TestUpdateCheckCLIRequiresTrustedSequence(t *testing.T) {
	code, err := UpdateCheckCLI([]string{"--manifest", "x", "--signature", "y", "--artifact", "z", "--public-key", "k"})
	if code != 2 || err == nil {
		t.Fatal("missing installed sequence accepted")
	}
	m, artifact, pub, priv, _ := updateFixture(t)
	m.ExpiresAt = time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	data, sig := signedUpdate(t, m, priv)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"manifest": data, "signature": sig, "key": pub, "artifact": artifact} {
		if err = os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"--manifest", filepath.Join(dir, "manifest"), "--signature", filepath.Join(dir, "signature"), "--artifact", filepath.Join(dir, "artifact"), "--public-key", filepath.Join(dir, "key"), "--installed-sequence", "1"}
	if code, err = UpdateCheckCLI(args); code != 0 || err != nil {
		t.Fatalf("valid CLI rejected %d %v", code, err)
	}
	if code, err = UpdateCheckCLI(append(args, "extra")); code == 0 || err == nil {
		t.Fatal("extra positional accepted")
	}
}
