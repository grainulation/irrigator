package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

func TestExecutionProfileCurrentNativeCLISize(t *testing.T) {
	// Claude 2.1.309 is 275,923,296 bytes; the old 256 MiB cap rejected it
	// before any operation existed. Sparse fixture avoids storing a binary copy.
	path, c, _ := executionFixture(t, "claude_cli")
	a := c.Adapters["adapter"]
	f, err := os.OpenFile(a.Command[0], os.O_RDWR, 0700)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = f.Truncate(275923296); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		t.Fatal(err)
	}
	a.ExecutableSHA256 = hex.EncodeToString(hash.Sum(nil))
	c.Adapters["adapter"] = a
	writeExecutionFixture(t, path, c)
	if _, err = ResolveExecutionProfile(path, ""); err != nil {
		t.Fatalf("current native CLI size rejected: %v", err)
	}
	if err = f.Truncate(512*1024*1024 + 1); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveExecutionProfile(path, ""); err == nil {
		t.Fatal("accepted executable above bounded512MiB limit")
	}
}
