package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The controller's busy scan uses ProbeLease instead of FileLock(path, true). It
// must classify every lease the way FileLock did: held, free, or an error.
func TestProbeLeaseAgreesWithFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease")

	held, err := ProbeLease(path)
	if err != nil || held {
		t.Fatalf("absent lease: held=%v err=%v", held, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("probe should create the lease file as FileLock does: %v", err)
	}

	lock, err := FileLock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	held, err = ProbeLease(path)
	if err != nil || !held {
		t.Fatalf("lease held by FileLock: held=%v err=%v", held, err)
	}
	if _, e := FileLock(path, true); !errors.Is(e, syscall.EWOULDBLOCK) {
		t.Fatalf("FileLock disagrees that the lease is held: %v", e)
	}
	Unlock(lock)

	held, err = ProbeLease(path)
	if err != nil || held {
		t.Fatalf("released lease: held=%v err=%v", held, err)
	}
	// A probe must never leave the lease taken.
	again, err := FileLock(path, true)
	if err != nil {
		t.Fatalf("probe left the lease locked: %v", err)
	}
	Unlock(again)

	missing := filepath.Join(t.TempDir(), "no-such-dir", "lease")
	if _, err = ProbeLease(missing); err == nil {
		t.Fatal("a lease in a missing directory must be an error, as with FileLock")
	}
	if _, e := FileLock(missing, true); e == nil {
		t.Fatal("FileLock accepted a missing directory")
	}
}
