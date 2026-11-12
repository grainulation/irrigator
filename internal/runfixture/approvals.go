package runfixture

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func ApprovalLegacyRun(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(model.Plan{Name: "approvals", Tasks: []model.Task{Task("a", 0)}}, root, "mock"); err != nil {
		t.Fatal(err)
	}
	return root
}

func ApprovalCapture(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func NotifyEnv(t *testing.T, server string) {
	t.Helper()
	t.Setenv("IRRIGATOR_NTFY_SERVER", server)
	t.Setenv("IRRIGATOR_NTFY_TOPIC", "")
	t.Setenv("IRRIGATOR_NTFY_TOKEN", "")
	t.Setenv("IRRIGATOR_APPROVALS_WEBHOOK", "")
}

const RunNotifyTopic = "run-notify-topic-0123456789"
