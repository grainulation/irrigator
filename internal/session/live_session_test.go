package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func TestPublishSessionMonotonicAndSameRunNoop(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, ".irrigator", "current-session.json")
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, step := range []struct {
		root, id string
		gen      int
	}{{a, "A", 1}, {a, "A", 1}, {b, "B", 2}, {a, "A", 3}} {
		if err := PublishSession(reg, step.root, step.id); err != nil {
			t.Fatal(err)
		}
		r, err := readSessionRegistry(reg)
		if err != nil || r.Generation != step.gen || r.RunID != step.id {
			t.Fatalf("got %+v %v want %v", r, err, step)
		}
	}
}

func TestRegistryMissingMalformedUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	if _, err := ReadRegistryAny(path); !os.IsNotExist(err) {
		t.Fatalf("missing registry must read as not-exist, got %v", err)
	}
	_ = os.WriteFile(path, []byte(`{"version":1,"extra":1}`), 0600)
	if _, err := ReadRegistryAny(path); err == nil {
		t.Fatal("malformed registry accepted")
	}
}

func TestPanelEventsAllowlistAndIDs(t *testing.T) {
	s := &model.State{RunID: "R", Events: []model.Event{{Time: time.Now(), Kind: "created", Task: "", Detail: nil}, {Time: time.Now(), Kind: "secret_kind", Task: "x", Detail: "private"}, {Time: time.Now(), Kind: "complete", Task: "", Detail: nil}}}
	ev := PanelEvents(s)
	if len(ev) != 2 || ev[0].ID != "R:1" || ev[1].ID != "R:3" {
		t.Fatalf("%+v", ev)
	}
}
