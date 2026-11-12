package executor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestExecutorRegistryKnowsLocalAndHerdr(t *testing.T) {
	if got := strings.Join(KnownExecutors(), ","); got != "herdr,local,tmux" {
		t.Fatalf("known executors = %s", got)
	}
	err := ValidateExecutorName("zellij", "--executor")
	if err == nil || !strings.Contains(err.Error(), "herdr, local, tmux") || !strings.Contains(err.Error(), `"zellij"`) {
		t.Fatalf("unknown name must list the known executors, got %v", err)
	}
	for name, ctor := range workerExecutors {
		if name == "herdr" {
			continue
		}
		ex, err := ctor()
		if err != nil || ex.name() != name {
			t.Errorf("%s: %v %v", name, ex, err)
		}
	}
}

func TestExecutorSelectionPrecedence(t *testing.T) {
	runfixture.FakeHc(t, "0.1.19")
	for _, tc := range []struct {
		name                string
		record, env, flag   string
		want                string
		wantOverrideMessage bool
	}{
		{name: "default is local", want: "local"},
		{name: "env selects", env: "herdr", want: "herdr"},
		{name: "record beats env", record: "local", env: "herdr", want: "local"},
		{name: "flag beats record", record: "local", flag: "herdr", want: "herdr", wantOverrideMessage: true},
		{name: "flag beats env", env: "herdr", flag: "local", want: "local"},
		{name: "flag equal to record is silent", record: "herdr", flag: "herdr", want: "herdr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IRRIGATOR_EXECUTOR", tc.env)
			root := t.TempDir()
			if tc.record != "" {
				if err := WriteExecutorRecord(root, tc.record); err != nil {
					t.Fatal(err)
				}
			}
			var ex workerExecutor
			var err error
			stderr := captureStderr(t, func() { ex, err = ResolveWorkerExecutor(root, tc.flag) })
			if err != nil || ex.name() != tc.want {
				t.Fatalf("got %v, %v; want %s", ex, err, tc.want)
			}
			if tc.wantOverrideMessage != strings.Contains(stderr, "overrides the run's recorded executor") {
				t.Errorf("override message mismatch: %q", stderr)
			}
			if tc.wantOverrideMessage && strings.Count(stderr, "\n") != 1 {
				t.Errorf("want exactly one line, got %q", stderr)
			}
			if rec, _ := ReadExecutorRecord(root); rec != tc.want {
				t.Errorf("recorded %q, want %q", rec, tc.want)
			}
		})
	}
}

func TestExecutorUnknownNamesFail(t *testing.T) {
	root := t.TempDir()
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	if _, err := ResolveWorkerExecutor(root, "nope"); err == nil || !strings.Contains(err.Error(), "herdr, local") {
		t.Errorf("flag: %v", err)
	}
	t.Setenv("IRRIGATOR_EXECUTOR", "nope")
	if _, err := ResolveWorkerExecutor(root, ""); err == nil || !strings.Contains(err.Error(), "IRRIGATOR_EXECUTOR") || !strings.Contains(err.Error(), "herdr, local") {
		t.Errorf("env: %v", err)
	}
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	if err := os.WriteFile(filepath.Join(root, ExecutorRecordName), []byte(`{"executor":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorkerExecutor(root, ""); err == nil || !strings.Contains(err.Error(), "recorded executor") {
		t.Errorf("record: %v", err)
	}
}

func TestExecutorLegacyRunWithoutRecordDefaultsToLocal(t *testing.T) {
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	root := runfixture.New(t, runfixture.Task("a", 0))
	if rec, err := ReadExecutorRecord(root); err != nil || rec != "" {
		t.Fatalf("legacy run: %q %v", rec, err)
	}
	name, _, err := chooseExecutor(root, "")
	if err != nil || name != "local" {
		t.Fatalf("legacy run chose %q: %v", name, err)
	}
}

func TestExecutorRecordedAtCreationAndKept(t *testing.T) {
	runfixture.FakeHc(t, "0.1.19")
	root := t.TempDir()
	t.Setenv("IRRIGATOR_EXECUTOR", "herdr")
	if err := RecordExecutorChoice(root, ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRRIGATOR_EXECUTOR", "local")
	if err := RecordExecutorChoice(root, ""); err != nil {
		t.Fatal(err)
	}
	if rec, _ := ReadExecutorRecord(root); rec != "herdr" {
		t.Fatalf("record changed to %q; an existing record must stay", rec)
	}
	if err := RecordExecutorChoice(filepath.Join(t.TempDir(), "x"), "nope"); err == nil {
		t.Fatal("unknown executor recorded")
	}
}

func TestHerdrVersionParsing(t *testing.T) {
	for _, tc := range []struct {
		out  string
		ok   bool
		want string
	}{
		{"hc 0.1.19\n", true, "0.1.19"},
		{"hc 0.1.19", true, "0.1.19"},
		{"hc 0.2.0", true, "0.2.0"},
		{"hc v1.0.3 (abc)", true, "1.0.3"},
		{"hc dev", false, ""},
		{"", false, ""},
		{"other 0.1.19", false, ""},
	} {
		v, _, ok := parseHcVersion(tc.out)
		if ok != tc.ok || v != tc.want {
			t.Errorf("%q: %q %v", tc.out, v, ok)
		}
	}
	for version, ok := range map[string]bool{"0.1.18": false, "0.1.19": true, "0.1.20": true, "0.2.0": true, "1.0.0": true, "0.0.99": false} {
		runfixture.FakeHc(t, version)
		if got := herdrStatus().Status == herdrOK; got != ok {
			t.Errorf("version %s accepted=%v want %v", version, got, ok)
		}
	}
	runfixture.FakeHc(t, "garbage")
	if st := herdrStatus(); st.Status != herdrUnknown {
		t.Errorf("garbage version: %+v", st)
	}
}

func TestDoctorExecutorsReport(t *testing.T) {
	byName := func() map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, e := range DoctorExecutors() {
			out[e["name"].(string)] = e
		}
		return out
	}
	t.Setenv("PATH", t.TempDir())
	got := byName()
	if got["local"]["available"] != true || got["herdr"]["status"] != herdrNotInstalled || got["herdr"]["available"] != false {
		t.Fatalf("no hc: %v", got)
	}
	if !strings.Contains(fmt.Sprint(got["herdr"]["summary"]), "not installed") {
		t.Errorf("summary: %v", got["herdr"]["summary"])
	}
	runfixture.FakeHc(t, "0.1.18")
	got = byName()
	if got["herdr"]["status"] != herdrTooOld || !strings.Contains(fmt.Sprint(got["herdr"]["summary"]), "too old") {
		t.Fatalf("old hc: %v", got["herdr"])
	}
	runfixture.FakeHc(t, "0.1.19")
	got = byName()
	if got["herdr"]["status"] != herdrOK || got["herdr"]["version"] != "0.1.19" || got["herdr"]["hc_path"] == "" || got["herdr"]["summary"] != "OK" {
		t.Fatalf("good hc: %v", got["herdr"])
	}
}
