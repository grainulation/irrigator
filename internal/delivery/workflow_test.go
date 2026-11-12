package delivery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func deliveryState(maps ...map[string]string) *model.State {
	s := &model.State{RunID: "test-run", Tasks: map[string]*model.TaskState{}}
	for i, m := range maps {
		id := string(rune('a' + i))
		raw, _ := json.Marshal(m)
		s.Tasks[id] = &model.TaskState{Status: "verified", Result: map[string]string{"files": string(raw)}}
	}
	return s
}

func TestDeliveryMergeAndResume(t *testing.T) {
	s := deliveryState(map[string]string{"index.html": "hello", "shared.js": "same"}, map[string]string{"nested/state.js": "state", "shared.js": "same"})
	dir := filepath.Join(t.TempDir(), "app")
	ids := []string{"a", "b"}
	if err := assembleDelivery(s, dir, ids, "files"); err != nil {
		t.Fatal(err)
	}
	if err := assembleDelivery(s, dir, ids, "files"); err != nil {
		t.Fatal("resume", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := assembleDelivery(s, dir, ids, "files"); err == nil {
		t.Fatal("accepted changed source")
	}
}

func TestDeliveryRejectsUnsafeAndConflictingMaps(t *testing.T) {
	for name, maps := range map[string][]map[string]string{
		"traversal": {{"../escape": "bad"}}, "absolute": {{"/tmp/escape": "bad"}},
		"reserved": {{".swarm-delivery.json": "bad"}}, "file_directory": {{"a": "one", "a/b": "two"}},
		"conflict": {{"index.html": "one"}, {"index.html": "two"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := deliveryState(maps...)
			ids := []string{"a"}
			if len(maps) > 1 {
				ids = append(ids, "b")
			}
			dir := filepath.Join(t.TempDir(), "app")
			if err := assembleDelivery(s, dir, ids, "files"); err == nil {
				t.Fatal("accepted unsafe map")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("published invalid output")
			}
		})
	}
}

func TestDeliveryRejectsSymlinkOnResume(t *testing.T) {
	s := deliveryState(map[string]string{"index.html": "hello"})
	base := t.TempDir()
	dir := filepath.Join(base, "app")
	if err := assembleDelivery(s, dir, []string{"a"}, "files"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "outside"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if err := assembleDelivery(s, dir, []string{"a"}, "files"); err == nil {
		t.Fatal("followed symlink")
	}
}
