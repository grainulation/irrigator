package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDoctorReportWithClaudeOnPath(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "claude")
	if err := os.WriteFile(exePath, []byte(`#!/bin/sh
printf '{"loggedIn":true}\n'
exit 0
`), 0o755); err != nil {
		t.Fatalf("failed to create temp executable: %v", err)
	}
	t.Setenv("PATH", dir)

	report := DoctorReport()

	if ready, ok := report["ready"].(bool); !ok || !ready {
		t.Errorf("expected ready=true, got %v", report["ready"])
	}
	if got, ok := report["claude_path"].(string); !ok || got != exePath {
		t.Errorf("expected claude_path=%q, got %v", exePath, report["claude_path"])
	}
	if report["claude_installed"] != true || report["haiku_auth"] != "authenticated" {
		t.Fatal("doctor must distinguish CLI discovery from authentication")
	}
	for _, key := range []string{"os", "arch", "go_version"} {
		if _, ok := report[key].(string); !ok {
			t.Errorf("expected string value for key %q, got %v", key, report[key])
		}
	}
}

func TestDoctorReportWithEmptyPath(t *testing.T) {
	t.Setenv("PATH", "")

	report := DoctorReport()

	if ready, ok := report["ready"].(bool); !ok || !ready || report["local_ready"] != true {
		t.Errorf("local execution must be ready without Claude, got %v", report)
	}
	if got, ok := report["claude_path"].(string); !ok || got != "" {
		t.Errorf("expected empty claude_path, got %v", report["claude_path"])
	}
	if report["claude_installed"] != false || report["haiku_auth"] != "not_installed" {
		t.Fatal("doctor misreported model authentication")
	}
}

func TestDoctorAuthFailuresAreNotReadiness(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"signed_out", `printf '{"loggedIn":false}'; exit 1`, "not_authenticated"},
		{"malformed", `printf 'private account diagnostic'; exit 1`, "provider_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "claude")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.body+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			r := DoctorReport()
			if r["haiku_auth"] != tc.want || r["model_ready"] != false {
				t.Fatal(r)
			}
			for _, mode := range r["modes"].([]string) {
				if mode == "haiku" {
					t.Fatal("unavailable backend advertised")
				}
			}
		})
	}
}

func TestDoctorAuthStatusTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec /bin/sleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if got := claudeAuthStatus(path, 30*time.Millisecond); got != "timeout" {
		t.Fatal(got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("auth probe did not honor deadline")
	}
}

func TestDoctorReportAgyVersionAndStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte("#!/bin/sh\necho '1.5.2'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	r := DoctorReport()
	if r["agy_status"] != "too_old" || r["agy_version"] != "1.5.2" {
		t.Fatalf("expected agy_status=too_old and agy_version=1.5.2, got %+v", r)
	}
	agents, ok := r["agents"].([]agentStatus)
	if !ok {
		t.Fatalf("expected agents slice in doctorReport, got %T", r["agents"])
	}
	var agy *agentStatus
	for _, a := range agents {
		if a.ID == "agy" {
			agy = &a
			break
		}
	}
	if agy == nil || agy.Status != "too_old" || agy.Available != false {
		t.Fatalf("expected agy in doctorReport agents with too_old and available=false, got %+v", agy)
	}
}
