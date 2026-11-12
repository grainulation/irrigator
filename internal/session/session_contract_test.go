package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestSessionFollowSwitchAndFailClosed(t *testing.T) {
	a, b := runfixture.New(t, runfixture.Task("same", 0)), runfixture.New(t, runfixture.Task("same", 0))
	for root, note := range map[string]string{a: "A public", b: "B public"} {
		if err := checkpoint.Update(root, func(s *model.State) error {
			s.Tasks["same"].Result = map[string]string{"findings": note, "secret": "private-marker"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	aid, bid := runfixture.Read(t, a).RunID, runfixture.Read(t, b).RunID
	reg := filepath.Join(t.TempDir(), "session.json")
	// current resolves the registry's CURRENT entry through the live registry
	// reader and summarizes it with the shared session summary projection. It
	// reports (summary, true) only when the current run is readable and matches.
	current := func() (panelSessionSummary, int, bool) {
		t.Helper()
		r, err := ReadRegistryAny(reg)
		if err != nil {
			return panelSessionSummary{}, 0, false
		}
		i := IndexOfRun(r.Entries, r.CurrentRunID)
		if i < 0 {
			return panelSessionSummary{}, 0, false
		}
		sum := summarizeRegisteredRun(r.Entries[i], true)
		return sum, r.Generation, sum.Available
	}
	if err := PublishSession(reg, a, aid); err != nil {
		t.Fatal(err)
	}
	if sum, _, ok := current(); !ok || sum.RunID != aid {
		t.Fatalf("A: %+v", sum)
	}
	if err := PublishSession(reg, b, bid); err != nil {
		t.Fatal(err)
	}
	sum, gen, ok := current()
	if !ok || sum.RunID != bid || gen != 2 {
		t.Fatalf("B: %+v gen=%d", sum, gen)
	}
	if raw, _ := json.Marshal(sum); strings.Contains(string(raw), "private-marker") || strings.Contains(string(raw), "A public") || strings.Contains(string(raw), "B public") {
		t.Fatalf("result fields leaked into the summary: %s", raw)
	}
	validRegistry, err := os.ReadFile(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, broken := range []string{"missing", "malformed", "wrong_run", "missing_target"} {
		// Each fault starts from the same valid fixture. Publication refuses
		// to overwrite malformed registries rather than silently repairing them.
		if err := os.WriteFile(reg, validRegistry, 0600); err != nil {
			t.Fatal(err)
		}
		if err := PublishSession(reg, b, bid); err != nil {
			t.Fatal(err)
		}
		switch broken {
		case "missing":
			os.Remove(reg)
		case "malformed":
			os.WriteFile(reg, []byte(`{"version":999}`), 0600)
		case "wrong_run":
			PublishSession(reg, b, "wrong-id")
		case "missing_target":
			PublishSession(reg, filepath.Join(t.TempDir(), "absent"), "missing")
		}
		if sum, _, ok := current(); ok || sum.RunID == bid {
			t.Fatalf("%s retained old current: %+v", broken, sum)
		}
	}
}

func TestSessionRegistryStrictAndNoSymlink(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "current.json")
	if err := PublishSession(reg, dir, "run1"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "link.json")
	if err := os.Symlink(reg, target); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionRegistry(target); err == nil {
		t.Fatal("symlink registry accepted")
	}
	raw, err := os.ReadFile(reg)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	json.Unmarshal(raw, &v)
	v["private_extra"] = "secret"
	data, _ := json.Marshal(v)
	os.WriteFile(reg, data, 0600)
	if _, err := readSessionRegistry(reg); err == nil {
		t.Fatal("unknown registry schema accepted")
	}
}

func TestSessionPublicEventsIncludeRealWorkerLifecycle(t *testing.T) {
	s := &model.State{RunID: "run1"}
	for _, kind := range []string{"started", "produced", "verified", "expanded", "graph_verified"} {
		s.Events = append(s.Events, model.Event{Kind: kind, Task: "parent__child", Detail: map[string]string{"secret": "never publish"}})
	}
	events := PanelEvents(s)
	if len(events) != 5 {
		t.Fatalf("real worker/graph events missing: %+v", events)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "never publish") || strings.Contains(string(raw), "secret") {
		t.Fatal("event private detail leaked")
	}
}
