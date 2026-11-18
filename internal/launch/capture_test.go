package launch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func TestCaptureProcessHelper(t *testing.T) {
	mode := os.Getenv("IRRIGATOR_CAPTURE_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "stdout-overflow":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("o", model.MaxOutputBytes+1024)))
	case "stderr-overflow":
		_, _ = os.Stderr.Write([]byte(strings.Repeat("e", 1024*1024+1024)))
	case "complete":
		_, _ = os.Stdout.Write([]byte("hello"))
		_, _ = os.Stderr.Write([]byte("diagnostic"))
	}
	os.Exit(0)
}

func TestProcessCaptureRecordsCompleteAndOverflowWithoutChangingOutcome(t *testing.T) {
	for _, mode := range []string{"complete", "stdout-overflow", "stderr-overflow"} {
		t.Run(mode, func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "1")
			cmd := exec.Command(os.Args[0], "-test.run=^TestCaptureProcessHelper$")
			cmd.Env = append(os.Environ(), "IRRIGATOR_CAPTURE_HELPER="+mode)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := runDirectProcess(ctx, cmd, prefix, []byte("{}"))
			if mode == "stdout-overflow" && err == nil {
				t.Fatal("stdout overflow no longer fails execution")
			}
			if mode != "stdout-overflow" && err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(prefix + ".capture.json")
			if err != nil {
				t.Fatal(err)
			}
			var capture struct {
				Schema  string                   `json:"schema"`
				Streams map[string]captureStream `json:"streams"`
			}
			if err := json.Unmarshal(raw, &capture); err != nil || capture.Schema != "irrigator.process-capture/v1" {
				t.Fatalf("invalid sidecar: %s %v", raw, err)
			}
			if !capture.Streams["stdout"].Complete || !capture.Streams["stderr"].Complete || capture.Streams["stdout"].Truncated != (mode == "stdout-overflow") || capture.Streams["stderr"].Truncated != (mode == "stderr-overflow") {
				t.Fatalf("capture metadata mismatch: %+v", capture)
			}
			if len(capture.Streams["stdout"].SHA256) != 64 || len(capture.Streams["stderr"].SHA256) != 64 {
				t.Fatal("capture not bound to source digest")
			}
		})
	}
}

func TestCaptureSidecarNeverOverwritesPriorAttemptEvidence(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "1")
	for _, suffix := range []string{".stdout.json", ".stderr.log"} {
		if err := os.WriteFile(prefix+suffix, []byte("a"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writer := &cappedWriter{}
	if err := writeProcessCapture(prefix, writer, writer, false); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(prefix + ".capture.json")
	if !strings.Contains(string(first), `"complete":false`) {
		t.Fatal("unfinished capture marked complete")
	}
	if err := writeProcessCapture(prefix, writer, writer, true); !os.IsExist(err) {
		t.Fatalf("earlier metadata could be replaced: %v", err)
	}
	second, _ := os.ReadFile(prefix + ".capture.json")
	if string(first) != string(second) {
		t.Fatal("historical capture metadata changed")
	}
}
