package launch

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestStopProcessGroupWithToleratesTransientEPERM(t *testing.T) {
	var probes int
	kill := func(pid int, sig syscall.Signal) error {
		if pid != -42 {
			t.Fatalf("pid %d, want the group -42", pid)
		}
		if sig == syscall.SIGKILL {
			return syscall.EPERM // reported while the dying group is reaped
		}
		probes++
		if probes < 3 {
			return syscall.EPERM
		}
		return syscall.ESRCH
	}
	if err := stopProcessGroupWith(kill, 42, time.Second); err != nil {
		t.Fatalf("a group that disappears after transient EPERM must be confirmed stopped: %v", err)
	}
	if probes != 3 {
		t.Fatalf("probes %d, want 3: confirmation needs a positive ESRCH", probes)
	}
}

func TestStopProcessGroupWithNeverTreatsPersistentEPERMAsStopped(t *testing.T) {
	var probes int
	kill := func(_ int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			return syscall.EPERM
		}
		if sig != 0 {
			t.Fatalf("probe used signal %d, want 0", sig)
		}
		probes++
		return syscall.EPERM
	}
	err := stopProcessGroupWith(kill, 42, 20*time.Millisecond)
	if !errors.Is(err, ErrProcessCleanup) {
		t.Fatalf("persistent EPERM must remain an unconfirmed cleanup, got %v", err)
	}
	if probes == 0 {
		t.Fatal("EPERM from the kill must reach the probe loop, not end the wait early")
	}
}

func TestStopProcessGroupWithUndeliveredSignalStaysUnconfirmed(t *testing.T) {
	var probes int
	kill := func(_ int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			return syscall.EPERM // the signal was not delivered
		}
		probes++
		return nil // the group is still visible and signalable
	}
	err := stopProcessGroupWith(kill, 42, 20*time.Millisecond)
	if !errors.Is(err, ErrProcessCleanup) {
		t.Fatalf("a still-visible group must never be confirmed stopped, got %v", err)
	}
	if probes == 0 {
		t.Fatal("the group must have been probed")
	}
}

func TestStopProcessGroupWithOtherKillErrorsFailImmediately(t *testing.T) {
	var calls int
	kill := func(int, syscall.Signal) error { calls++; return syscall.EINVAL }
	if err := stopProcessGroupWith(kill, 42, time.Second); !errors.Is(err, ErrProcessCleanup) {
		t.Fatalf("got %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls %d: a non-EPERM kill error must not enter the probe loop", calls)
	}
}

func TestStopProcessGroupWithAlreadyGoneIsConfirmed(t *testing.T) {
	kill := func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := stopProcessGroupWith(kill, 42, time.Second); err != nil {
		t.Fatal(err)
	}
}
