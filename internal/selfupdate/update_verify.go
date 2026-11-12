package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
)

const updateManifestSchema = "irrigator.update/v1"
const updateArtifactLimit int64 = 512 * 1024 * 1024

type updateManifest struct {
	Schema         string `json:"schema"`
	Sequence       uint64 `json:"sequence"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	ExpiresAt      string `json:"expires_at"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	ArtifactBytes  int64  `json:"artifact_bytes"`
}

// Signatures cover exactly json.Marshal(UpdateManifest), without a final newline.
// Requiring these bytes avoids multiple encodings, ignored fields, and aliases.
func parseUpdateManifest(data []byte) (updateManifest, error) {
	var m updateManifest
	if len(data) == 0 || len(data) > 4096 {
		return m, errors.New("update manifest exceeds bounds")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return m, errors.New("update manifest must be an object")
	}
	allowed := map[string]bool{"schema": true, "sequence": true, "version": true, "os": true, "arch": true, "expires_at": true, "artifact_sha256": true, "artifact_bytes": true}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			return m, errors.New("unknown, duplicate or aliased update field")
		}
		seen[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil || bytes.Equal(value, []byte("null")) {
			return m, errors.New("invalid update field")
		}
	}
	if _, err = d.Token(); err != nil || len(seen) != len(allowed) {
		return m, errors.New("missing update fields")
	}
	if d.Decode(new(any)) != io.EOF {
		return m, errors.New("trailing update data")
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	canonical, err := json.Marshal(m)
	if err != nil || !bytes.Equal(canonical, data) {
		return m, errors.New("update manifest is not canonical")
	}
	return m, nil
}

func verifyUpdateManifest(data, signature, publicKey []byte, installed uint64, now time.Time, osName, arch string) (updateManifest, error) {
	m, err := parseUpdateManifest(data)
	if err != nil {
		return m, err
	}
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(publicKey), data, signature) {
		return m, errors.New("update signature rejected")
	}
	if m.Schema != updateManifestSchema {
		return m, errors.New("unsupported update schema")
	}
	if m.Sequence <= installed {
		return m, errors.New("update sequence is not newer than installed release")
	}
	if len(m.Version) > 80 || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`).MatchString(m.Version) {
		return m, errors.New("invalid update version")
	}
	if m.OS != osName || m.Arch != arch {
		return m, errors.New("update platform mismatch")
	}
	expires, err := time.Parse(time.RFC3339, m.ExpiresAt)
	if err != nil || m.ExpiresAt != expires.UTC().Format(time.RFC3339) || !expires.After(now) {
		return m, errors.New("update manifest expired or expiry invalid")
	}
	digest, err := hex.DecodeString(m.ArtifactSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != m.ArtifactSHA256 {
		return m, errors.New("invalid artifact digest")
	}
	if m.ArtifactBytes <= 0 || m.ArtifactBytes > updateArtifactLimit {
		return m, errors.New("artifact size exceeds bounds")
	}
	return m, nil
}

func openUpdateFile(name string, limit int64) (*os.File, error) {
	if name == "" {
		return nil, errors.New("update input path is required")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	f, err := launch.OpenContextFile(absolute, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() < 0 || st.Size() > limit {
		f.Close()
		return nil, errors.New("update input must be a bounded regular file")
	}
	return f, nil
}
func readUpdateFile(name string, limit int64) ([]byte, error) {
	f, err := openUpdateFile(name, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("update input read exceeds bounds")
	}
	return b, nil
}
func verifyUpdateArtifact(name string, m updateManifest) error {
	f, err := openUpdateFile(name, updateArtifactLimit)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, m.ArtifactBytes+1))
	if err != nil || n != m.ArtifactBytes || hex.EncodeToString(hash.Sum(nil)) != m.ArtifactSHA256 {
		return errors.New("artifact digest or size mismatch")
	}
	return nil
}

// Offline validation only. The key and installed sequence must come from a trusted
// release configuration, never from the downloaded manifest. No install occurs.
func UpdateCheckCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("update-check", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "canonical signed update manifest")
	signature := fs.String("signature", "", "raw 64-byte Ed25519 signature")
	artifact := fs.String("artifact", "", "local artifact to verify")
	publicKey := fs.String("public-key", "", "independently pinned raw 32-byte Ed25519 public key")
	installed := fs.Uint64("installed-sequence", 0, "trusted installed release sequence (required)")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	hasSequence := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "installed-sequence" {
			hasSequence = true
		}
	})
	if fs.NArg() != 0 || !hasSequence || *manifest == "" || *signature == "" || *artifact == "" || *publicKey == "" {
		return 2, errors.New("manifest, signature, artifact, public-key and installed-sequence are required")
	}
	data, err := readUpdateFile(*manifest, 4096)
	if err != nil {
		return 1, err
	}
	sig, err := readUpdateFile(*signature, ed25519.SignatureSize)
	if err != nil {
		return 1, err
	}
	key, err := readUpdateFile(*publicKey, ed25519.PublicKeySize)
	if err != nil {
		return 1, err
	}
	m, err := verifyUpdateManifest(data, sig, key, *installed, time.Now(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return 1, err
	}
	if err = verifyUpdateArtifact(*artifact, m); err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(map[string]any{"verified": true, "installed": false, "sequence": m.Sequence, "version": m.Version, "artifact_sha256": m.ArtifactSHA256, "scope": fmt.Sprintf("offline artifact verification for %s/%s", m.OS, m.Arch)})
}
