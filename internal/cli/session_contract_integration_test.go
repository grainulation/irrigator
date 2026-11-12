//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
	"github.com/grainulation/irrigator/internal/session"
)

func TestSessionControllerOwnershipPreventsPublication(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	reg := filepath.Join(t.TempDir(), "current.json")
	old := session.ActiveSessionRegistry
	session.ActiveSessionRegistry = reg
	defer func() { session.ActiveSessionRegistry = old }()
	lock, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lock)
	if _, err := runops.Run(root, model.Options{Workers: 1}); err == nil {
		t.Fatal("second controller accepted")
	}
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Fatal("non-owner published registry")
	}
}
