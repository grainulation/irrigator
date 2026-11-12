package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	dir      string
	bin      string
	keyPath  string
	pubPath  string
	artifact string
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// newFixture builds the repository's irrigator binary and an ephemeral key.
// No key material is read from or written to the repository.
func newFixture(t *testing.T) fixture {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("release platforms are darwin and linux")
	}
	// The verifier rejects symlinked path components, and macOS temp roots are symlinks.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{dir: dir, bin: filepath.Join(dir, "irrigator")}
	build := exec.Command("go", "build", "-trimpath", "-o", f.bin, "../..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build irrigator: %v\n%s", err, out)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.keyPath = filepath.Join(dir, "signing.key")
	f.pubPath = filepath.Join(dir, "trusted.pub")
	f.artifact = filepath.Join(dir, "irrigator_test.tar.gz")
	mustWrite(t, f.keyPath, priv)
	mustWrite(t, f.pubPath, pub)
	mustWrite(t, f.artifact, []byte("not a real archive, just bytes\n"))
	return f
}

func (f fixture) sign(t *testing.T, out string, sequence uint64, now time.Time) {
	t.Helper()
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	o := options{keyPath: f.keyPath, artifactPath: f.artifact, version: "1.2.3", goos: runtime.GOOS, goarch: runtime.GOARCH, sequence: sequence, validFor: time.Hour, outDir: out}
	if err := run(o, now); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) check(out, artifact string, installed uint64) (string, error) {
	cmd := exec.Command(f.bin, "update-check",
		"--manifest", filepath.Join(out, "release.json"), "--signature", filepath.Join(out, "release.sig"),
		"--artifact", artifact, "--public-key", f.pubPath, "--installed-sequence", strconv.FormatUint(installed, 10))
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func TestSignedReleaseVerifiesWithUpdateCheck(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(f.dir, "ok")
	f.sign(t, out, 5, time.Now())
	got, err := f.check(out, f.artifact, 4)
	if err != nil {
		t.Fatalf("update-check rejected a signed release: %v\n%s", err, got)
	}
	if !strings.Contains(got, `"verified"`) || !strings.Contains(got, `"installed": false`) && !strings.Contains(got, `"installed":false`) {
		t.Fatalf("unexpected update-check output: %s", got)
	}
	manifest, err := os.ReadFile(filepath.Join(out, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasSuffix(manifest, []byte("\n")) || !bytes.HasPrefix(manifest, []byte(`{"schema":"irrigator.update/v1","sequence":5,`)) {
		t.Fatalf("manifest is not canonical: %q", manifest)
	}
}

func TestSignedReleaseNegativeCases(t *testing.T) {
	f := newFixture(t)
	signed := filepath.Join(f.dir, "signed")
	f.sign(t, signed, 5, time.Now())

	t.Run("tampered artifact", func(t *testing.T) {
		tampered := filepath.Join(f.dir, "tampered.tar.gz")
		mustWrite(t, tampered, []byte("NOT a real archive, just bytes\n"))
		if out, err := f.check(signed, tampered, 4); err == nil {
			t.Fatalf("tampered artifact accepted: %s", out)
		}
	})
	t.Run("replayed sequence", func(t *testing.T) {
		for _, installed := range []uint64{5, 6} {
			if out, err := f.check(signed, f.artifact, installed); err == nil {
				t.Fatalf("sequence 5 accepted with installed sequence %d: %s", installed, out)
			}
		}
	})
	t.Run("tampered manifest", func(t *testing.T) {
		bad := filepath.Join(f.dir, "bad")
		if err := os.MkdirAll(bad, 0o755); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(filepath.Join(signed, "release.json"))
		sig, _ := os.ReadFile(filepath.Join(signed, "release.sig"))
		mustWrite(t, filepath.Join(bad, "release.json"), bytes.Replace(raw, []byte(`"sequence":5`), []byte(`"sequence":9`), 1))
		mustWrite(t, filepath.Join(bad, "release.sig"), sig)
		if out, err := f.check(bad, f.artifact, 4); err == nil {
			t.Fatalf("manifest with altered sequence accepted: %s", out)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		other := f
		other.pubPath = filepath.Join(f.dir, "other.pub")
		mustWrite(t, other.pubPath, pub)
		if out, err := other.check(signed, f.artifact, 4); err == nil {
			t.Fatalf("signature accepted under an unrelated key: %s", out)
		}
	})
	t.Run("expired", func(t *testing.T) {
		expired := filepath.Join(f.dir, "expired")
		f.sign(t, expired, 5, time.Now().Add(-48*time.Hour))
		if out, err := f.check(expired, f.artifact, 4); err == nil {
			t.Fatalf("expired manifest accepted: %s", out)
		}
	})
}

func TestRunRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := filepath.Join(dir, "k")
	art := filepath.Join(dir, "a")
	short := filepath.Join(dir, "short")
	mustWrite(t, key, priv)
	mustWrite(t, art, []byte("x"))
	mustWrite(t, short, priv[:32])
	good := options{keyPath: key, artifactPath: art, version: "1.0.0", goos: "linux", goarch: "arm64", sequence: 1, validFor: time.Hour, outDir: dir}
	if err := run(good, time.Now()); err != nil {
		t.Fatalf("baseline options rejected: %v", err)
	}
	absent := filepath.Join(dir, "absent")
	cases := map[string]func(*options){
		"missing key":        func(o *options) { o.keyPath = absent },
		"short key":          func(o *options) { o.keyPath = short },
		"windows":            func(o *options) { o.goos = "windows" },
		"bad version":        func(o *options) { o.version = "v1.0" },
		"zero sequence":      func(o *options) { o.sequence = 0 },
		"missing output dir": func(o *options) { o.outDir = absent },
		"directory artifact": func(o *options) { o.artifactPath = dir },
	}
	for name, mutate := range cases {
		o := good
		mutate(&o)
		if err := run(o, time.Now()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := os.Stat(absent); err == nil {
		t.Error("tool created a file or directory at a missing path")
	}
}
