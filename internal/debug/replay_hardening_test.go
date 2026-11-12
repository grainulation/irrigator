package debug

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func replayHardeningRun(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(model.Plan{Name: "p", Tasks: []model.Task{runfixture.Task("a", 0)}}, root, "mock"); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReplayEventKindIsAClosedSet(t *testing.T) {
	for _, kind := range []string{"started", "failed", "cancel_requested", "outcome_uncertain", "legacy_cost_unknown", "turn_verified", "event"} {
		if got := replayEventKind(kind); got != kind {
			t.Errorf("known kind %q mapped to %q", kind, got)
		}
	}
	for _, kind := range []string{"db_password_is_hunter2_sentinelleak", "k_" + strings.Repeat("deadbeef", 8), "", "Started", "started "} {
		if got := replayEventKind(kind); got != "event" {
			t.Errorf("unknown kind %q mapped to %q, want the generic \"event\"", kind, got)
		}
	}
}

func TestReplayExportCollapsesHostileEventKinds(t *testing.T) {
	root := replayHardeningRun(t)
	hostile := []string{"db_password_is_hunter2_sentinelleak", "k_" + strings.Repeat("deadbeef", 8)}
	if err := checkpoint.Update(root, func(s *model.State) error {
		for _, kind := range hostile {
			s.Events = append(s.Events, model.Event{Time: time.Now(), Kind: kind, Task: "a"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := BuildReplayFixtureBytes(runfixture.Read(t, root), "hostile-kinds")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hostile {
		if bytes.Contains(raw, []byte(h)) {
			t.Fatalf("worker-written event kind %q reached the fixture", h)
		}
	}
	if _, err := DecodeReplayFixture(raw); err != nil {
		t.Fatal(err)
	}
}

func TestReplayValidatorRejectsFreeTextEventKind(t *testing.T) {
	root := replayHardeningRun(t)
	raw, err := BuildReplayFixtureBytes(runfixture.Read(t, root), "validator")
	if err != nil {
		t.Fatal(err)
	}
	fx, err := DecodeReplayFixture(raw)
	if err != nil {
		t.Fatal(err)
	}
	fx.Events = append(fx.Events, ReplayEvent{Kind: "db_password_is_hunter2_sentinelleak", Task: fx.Tasks[0].ID})
	bad, err := json.Marshal(fx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReplayFixture(bad); err == nil {
		t.Fatal("a fixture carrying a free-text event kind must be rejected")
	}
}

func TestReplayDecodeRejectsTrailingData(t *testing.T) {
	root := replayHardeningRun(t)
	raw, err := BuildReplayFixtureBytes(runfixture.Read(t, root), "trailing")
	if err != nil {
		t.Fatal(err)
	}
	for _, tail := range []string{"]", "}", " {}", "\n[1]"} {
		if _, err := DecodeReplayFixture(append(append([]byte{}, raw...), tail...)); err == nil {
			t.Errorf("trailing %q after a valid fixture was accepted", tail)
		}
	}
}

func TestReplayExportRefusesOutInsideRunDir(t *testing.T) {
	root := replayHardeningRun(t)
	out := filepath.Join(root, "fixture.json")
	var buf bytes.Buffer
	code, err := debugFixtureRun([]string{"export", root, "--out", out}, &buf)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "outside the run directory") {
		t.Fatalf("code %d err %v", code, err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("a file was created inside the run directory")
	}
	// A symlinked subdirectory of the run directory must not smuggle the write out.
	elsewhere := t.TempDir()
	link := filepath.Join(root, "export")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	code, err = debugFixtureRun([]string{"export", root, "--out", filepath.Join(link, "x.json")}, &buf)
	if code == 0 && err == nil {
		// Resolving the link lands outside the run directory, which is allowed;
		// the file must then be where the symlink points and nowhere in the run dir.
		if _, statErr := os.Stat(filepath.Join(elsewhere, "x.json")); statErr != nil {
			t.Fatalf("export reported success but wrote nothing at the resolved target: %v", statErr)
		}
	}
}

func TestReplayExportRefusesNonRegularState(t *testing.T) {
	root := replayHardeningRun(t)
	state := filepath.Join(root, "state.json")
	real := filepath.Join(t.TempDir(), "real.json")
	if err := os.Rename(state, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, state); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "fx.json")
	var buf bytes.Buffer
	code, err := debugFixtureRun([]string{"export", root, "--out", out}, &buf)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("code %d err %v", code, err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("a fixture was written from a symlinked state.json")
	}
}

// The gate must notice a fixture that moves between the dev and held-out sets or
// disappears: every committed fixture is pinned to its set by name.
func TestReplayCommittedFixtureSetsArePinned(t *testing.T) {
	want := map[string][]string{
		"dev": {
			"cancelled-running-task", "failed-provider-auth", "failed-validation-blocks-dependent",
			"managed-mock-failed-validation", "spawn-waiting-parent-failed-child",
			"turn-failed-helper-blocks-waited-continuation", "uncertain-killed-worker",
			"uncertain-provider-timeout", "verified-cost-unknown",
		},
		"heldout": {
			"cancelled-in-flight-provider", "cancelled-pending-subtree", "failed-mid-dag-partial-success",
			"failed-provider-error-with-cost", "failed-provider-session-budget",
			"spawn-nested-waiting-cancelled-child", "uncertain-malformed-response",
		},
	}
	for set, names := range want {
		files, err := filepath.Glob(filepath.Join(runfixture.ReplayDir(), set, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range files {
			got = append(got, strings.TrimSuffix(filepath.Base(f), ".json"))
		}
		sort.Strings(got)
		sort.Strings(names)
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("%s set changed:\n got  %v\n want %v\n(update this pin deliberately when adding or moving a fixture)", set, got, names)
		}
	}
}

func TestReplayDuplicateJSONKeysAreRejected(t *testing.T) {
	bad := map[string]string{
		"top level":                    `{"schema":"a","schema":"b"}`,
		"nested object":                `{"tasks":[{"id":"t1","status":"x","status":"y"}]}`,
		"inside an array element":      `{"events":[{"kind":"a"},{"kind":"a","kind":"b"}]}`,
		"deeply nested":                `{"a":{"b":{"c":1,"c":2}}}`,
		"case folded":                  `{"Schema":1,"schema":2}`,
		"unicode simple fold (long s)": `{"ſchema":1,"schema":2}`,
		"kelvin sign":                  `{"K":1,"k":2}`,
		"escaped equivalent":           `{"schema":1,"schema":2}`,
	}
	for name, doc := range bad {
		if err := rejectDuplicateJSONKeys([]byte(doc)); err == nil {
			t.Errorf("%s: duplicate key accepted: %s", name, doc)
		}
	}
	good := map[string]string{
		"same key in different objects": `{"a":{"k":1},"b":{"k":2},"c":[{"k":1},{"k":2}]}`,
		"key-like text in a string":     `{"a":"\"b\":1,\"b\":2","b":3}`,
		"repeated scalars in an array":  `[1,2,"a","a",null,true]`,
		"empty object":                  `{}`,
		"nested empty containers":       `{"a":{},"b":[],"c":[{}],"d":[[]]}`,
	}
	for name, doc := range good {
		if err := rejectDuplicateJSONKeys([]byte(doc)); err != nil {
			t.Errorf("%s: valid document rejected: %v", name, err)
		}
	}
}

func TestReplayDuplicateKeyErrorDoesNotEchoContent(t *testing.T) {
	err := rejectDuplicateJSONKeys([]byte(`{"hunter2_sentinel":1,"hunter2_sentinel":2}`))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error must report an offset only, got %v", err)
	}
}

func TestReplayDuplicateKeyWalkerBoundsAndMalformedInput(t *testing.T) {
	deep := strings.Repeat(`{"a":`, replayMaxJSONDepth+2) + "1" + strings.Repeat("}", replayMaxJSONDepth+2)
	if err := rejectDuplicateJSONKeys([]byte(deep)); err == nil {
		t.Error("nesting beyond the bound was accepted")
	}
	for _, doc := range []string{`{"a":`, `{"a":1,}`, `{"a" 1}`, `[1,2`} {
		if err := rejectDuplicateJSONKeys([]byte(doc)); err == nil {
			t.Errorf("malformed %q accepted", doc)
		}
	}
}

func TestDecodeReplayFixtureRejectsDuplicateKeys(t *testing.T) {
	root := replayHardeningRun(t)
	raw, err := BuildReplayFixtureBytes(runfixture.Read(t, root), "dup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReplayFixture(raw); err != nil {
		t.Fatal(err)
	}
	// "scenario" already exists at the top level; a second copy must be refused
	// even though the decoder would silently keep the last one.
	dup := bytes.Replace(raw, []byte("{"), []byte(`{"scenario":"hijacked",`), 1)
	if _, err := DecodeReplayFixture(dup); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate top-level key in a fixture: %v", err)
	}
}

func TestReplayDuplicateKeyWalkerIsLinearOnLargeInput(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"tasks":[`)
	for i := 0; b.Len() < 1<<20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"t1","status":"verified","depends_on":["t2"],"attempt":1}`)
	}
	b.WriteString(`]}`)
	if err := rejectDuplicateJSONKeys([]byte(b.String())); err != nil {
		t.Fatalf("a valid ~1 MiB document was rejected: %v", err)
	}
}

func BenchmarkReplayDuplicateKeyWalk(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`{"tasks":[`)
	for i := 0; sb.Len() < 1<<20; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":"t1","status":"verified","depends_on":["t2"],"attempt":1}`)
	}
	sb.WriteString(`]}`)
	raw := []byte(sb.String())
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := rejectDuplicateJSONKeys(raw); err != nil {
			b.Fatal(err)
		}
	}
}

// A relative RUN_DIR is the common invocation; the --out-inside-run check must
// hold for it too (an unresolved relative path used to make the comparison fail
// open).
func TestReplayExportGuardHoldsForRelativeRunDir(t *testing.T) {
	base := t.TempDir()
	if err := checkpoint.Initialize(model.Plan{Name: "p", Tasks: []model.Task{runfixture.Task("a", 0)}}, filepath.Join(base, "run"), "mock"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	var buf bytes.Buffer
	for _, out := range []string{"run/fx.json", "./run/fx.json", "run/../run/fx.json"} {
		code, err := debugFixtureRun([]string{"export", "run", "--out", out}, &buf)
		if code != 1 || err == nil || !strings.Contains(err.Error(), "outside the run directory") {
			t.Errorf("--out %s: code %d err %v", out, code, err)
		}
		if _, statErr := os.Stat(filepath.Join(base, "run", "fx.json")); statErr == nil {
			t.Fatalf("--out %s wrote inside the run directory", out)
		}
	}
	if code, err := debugFixtureRun([]string{"export", "run", "--out", "fx.json"}, &buf); code != 0 || err != nil {
		t.Fatalf("an --out beside the run directory must work: %d %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(base, "fx.json")); err != nil {
		t.Fatal(err)
	}
}

// `irrigator debug fixture` must keep meaning "diagnose the run directory named
// fixture"; only the explicit `fixture export` sequence selects the verb.
func TestDebugFixtureNameStillMeansRunDirectory(t *testing.T) {
	base := t.TempDir()
	if err := checkpoint.Initialize(model.Plan{Name: "p", Tasks: []model.Task{runfixture.Task("a", 0)}}, filepath.Join(base, "fixture"), "mock"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	var out bytes.Buffer
	code, err := DebugRun([]string{"fixture", "--json"}, &out)
	if code != 0 || err != nil || !json.Valid(out.Bytes()) {
		t.Fatalf("debug fixture --json on a run directory named fixture: %d %v %q", code, err, out.String())
	}
	out.Reset()
	if code, err := DebugRun([]string{"fixture", "export", "--help"}, &out); code != 0 || err != nil || !strings.Contains(out.String(), "experimental") {
		t.Fatalf("the export verb must still be reachable: %d %v", code, err)
	}
}
