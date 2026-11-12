//go:build integration

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/debug"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func replayFailedRun(t *testing.T) string {
	t.Helper()
	bad := replayTask1("a")
	bad.Equals = map[string]string{"answer": runfixture.SentinelEquals}
	root := replayMockRoot(t, bad, replayTask1("b", "a"), replayTask1("c"))
	_, done := clitest.Start(t, root)
	replayWait(t, done)
	return root
}

func exportVerb(args ...string) (int, string, error) {
	var out bytes.Buffer
	code, err := debug.DebugRun(append([]string{"fixture", "export"}, args...), &out)
	return code, out.String(), err
}

func TestDebugFixtureExportWritesRedactedReplayableFile(t *testing.T) {
	root := replayFailedRun(t)
	before, _ := os.ReadFile(filepath.Join(root, "state.json"))
	out := filepath.Join(t.TempDir(), "fx.json")
	code, msg, err := exportVerb(root, "--out", out, "--scenario", "mined-run")
	if code != 0 || err != nil {
		t.Fatalf("export: %d %v", code, err)
	}
	if !strings.Contains(msg, "experimental") {
		t.Fatalf("output must say experimental: %q", msg)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fixture mode: %v %v", info, err)
	}
	raw, _ := os.ReadFile(out)
	if hit := runfixture.ReplayContainsSentinel(raw); hit != "" {
		t.Fatalf("export leaked %s", hit)
	}
	if hits := debug.ReplaySecretScan(raw); len(hits) > 0 {
		t.Fatalf("export matched %v", hits)
	}
	fx, err := debug.DecodeReplayFixture(raw)
	if err != nil || fx.Scenario != "mined-run" || fx.Schema != debug.ReplaySchema {
		t.Fatalf("decode: %v %+v", err, fx)
	}
	// The point of the verb: a mined real run replays through the executor path.
	if diffs := replayRoundTrip(t, fx); len(diffs) > 0 {
		t.Fatalf("mined run did not replay:\n  %s", strings.Join(diffs, "\n  "))
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("export modified the run")
	}
}

func TestDebugFixtureExportRefusesOverwriteAndSymlink(t *testing.T) {
	root := replayFailedRun(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "fx.json")
	if err := os.WriteFile(out, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, err := exportVerb(root, "--out", out); code != 1 || err == nil {
		t.Fatalf("overwrite not refused: %d %v", code, err)
	}
	if got, _ := os.ReadFile(out); string(got) != "keep me" {
		t.Fatal("existing file was modified")
	}
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if code, _, err := exportVerb(root, "--out", link); code != 1 || err == nil {
		t.Fatalf("symlink --out not refused: %d %v", code, err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("wrote through a symlink")
	}
}

func TestDebugFixtureExportFailsClosedWhenRedactionRejects(t *testing.T) {
	root := replayFailedRun(t)
	s := runfixture.Read(t, root)
	// In-memory poison: a role outside the closed set stands in for any value
	// the allowlist must reject. The exporter must return an error and no bytes.
	s.Tasks["c"].Spec.Turn = &model.TurnIdentity{AgentID: "c", Role: "SENTINEL_ROLE_LEAK", Index: 1}
	data, err := debug.BuildReplayFixtureBytes(s, "poisoned")
	if err == nil || data != nil || !strings.Contains(err.Error(), "redaction check") {
		t.Fatalf("expected redaction rejection, got %v / %q", err, data)
	}
	if strings.Contains(err.Error(), "SENTINEL_ROLE_LEAK") {
		t.Fatal("error echoed the rejected value")
	}
	// A serialised-bytes scan hit also rejects: scenario names pass the
	// allowlist, so hand the scanner a home path directly.
	if hits := debug.ReplaySecretScan([]byte(`{"x":"` + runfixture.SentinelHomePath() + `"}`)); len(hits) == 0 {
		t.Fatal("secret scan missed an absolute home path")
	}
	// Through the verb: a checkpoint whose backend is outside the closed set is refused and leaves no file.
	if err := checkpoint.Update(root, func(st *model.State) error { st.Backend = "SENTINEL_BACKEND_LEAK"; return nil }); err != nil {
		t.Skip("checkpoint rejects the poisoned backend before the exporter sees it:", err)
	}
	out := filepath.Join(t.TempDir(), "fx.json")
	code, _, verr := exportVerb(root, "--out", out)
	if code != 1 || verr == nil || strings.Contains(verr.Error(), "SENTINEL_BACKEND_LEAK") {
		t.Fatalf("expected closed failure without echo: %d %v", code, verr)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a rejected export left a file behind")
	}
}

func TestDebugFixtureExportUsageErrors(t *testing.T) {
	root := replayFailedRun(t)
	out := filepath.Join(t.TempDir(), "fx.json")
	for name, args := range map[string][]string{
		"no out":           {root},
		"no run":           {"--out", out},
		"bad scenario":     {root, "--out", out, "--scenario", "Has Spaces"},
		"unknown flag":     {root, "--out", out, "--bogus"},
		"two runs":         {root, root, "--out", out},
		"scenario as path": {root, "--out", out, "--scenario", "../../etc"},
	} {
		if code, _, err := exportVerb(args...); code != 2 || err == nil {
			t.Errorf("%s: want usage error 2, got %d %v", name, code, err)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("usage error created a file")
	}
	if code, _, err := exportVerb(filepath.Join(t.TempDir(), "missing"), "--out", out); code != 1 || err == nil {
		t.Fatalf("missing run: %d %v", code, err)
	}
	if code, _, err := exportVerb(root, "--out", filepath.Join(t.TempDir(), "no-such-dir", "fx.json")); code != 1 || err == nil {
		t.Fatalf("unwritable --out: %d %v", code, err)
	}
	if code, _ := debug.DebugRun([]string{"fixture", "import"}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("unknown fixture subcommand: %d", code)
	}
}
