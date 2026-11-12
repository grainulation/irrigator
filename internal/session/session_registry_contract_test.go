package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRegistryExistingRecentResumeHonorsActiveCapacity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	old, id := runfixture.V2Run(t, dir, "old")
	v2Complete(t, old)
	if err := PublishSession(path, old, id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < registryMaxActive; i++ {
		r, rid := runfixture.V2Run(t, dir, "active"+strconv.Itoa(i))
		if err := PublishSession(path, r, rid); err != nil {
			t.Fatal(err)
		}
	}
	if err := PublishSession(path, old, id); !errors.Is(err, errRegistryActiveFull) {
		t.Fatalf("existing recent admission bypassed cap: %v", err)
	}
	reg, err := ReadRegistryAny(path)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Overflow.ActiveRejected != 1 || len(reg.Entries) != registryMaxActive+1 {
		t.Fatalf("lost entries or overflow: %+v", reg)
	}
}

func TestRegistryPerRunEventTailIsBoundedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	root, id := runfixture.V2Run(t, dir, "events")
	if err := checkpoint.Update(root, func(s *model.State) error {
		for i := 0; i < 80; i++ {
			s.Event("started", "a", map[string]any{"secret": "DO-NOT-EXPOSE"})
		}
		s.Event("private_kind", "a", "DO-NOT-EXPOSE")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e := registryEntry{RunID: id, RunPath: root, StartedAt: time.Now()}
	sum := summarizeRegisteredRun(e, true)
	if len(sum.Events) != 64 || sum.EventsFloor != sum.Events[0].Seq || sum.EventsWatermark != sum.EventsTotal {
		t.Fatalf("bad tail: %+v", sum)
	}
	if sum.Events[len(sum.Events)-1].Seq != sum.EventsWatermark-1 {
		t.Fatal("filtered events changed durable sequence positions")
	}
	b, _ := json.Marshal(sum)
	if strings.Contains(string(b), "DO-NOT-EXPOSE") || strings.Contains(string(b), "SECRET-PROMPT-MARKER") {
		t.Fatal("private data exposed")
	}
	if len(summarizeRegisteredRun(e, false).Events) != 0 {
		t.Fatal("capacity probe unnecessarily projects events")
	}
}
