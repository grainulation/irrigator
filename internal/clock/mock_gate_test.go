package clock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitMockGate(t *testing.T) {
	t.Setenv(MockGateEnv, "")
	if err := WaitMockGate(context.Background(), "a"); err != nil {
		t.Fatalf("unset gate must not block: %v", err)
	}
	dir := t.TempDir()
	t.Setenv(MockGateEnv, dir)
	if err := WaitMockGate(context.Background(), "free"); err != nil {
		t.Fatalf("a task without a hold must not block: %v", err)
	}
	hold := filepath.Join(dir, "a.hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- WaitMockGate(context.Background(), "a") }()
	select {
	case err := <-done:
		t.Fatalf("returned while held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitMockGate(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}
