package dashboard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestStatusExecutorUnreadableRecordIsUnknown(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := os.WriteFile(filepath.Join(root, executor.ExecutorRecordName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RunExecutor(root); got != "unknown" {
		t.Fatalf("runExecutor = %q", got)
	}
}
