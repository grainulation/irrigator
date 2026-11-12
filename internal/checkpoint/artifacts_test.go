package checkpoint

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/model"
)

func TestTypedArtifactRoundTrip(t *testing.T) {
	task := model.Task{Requires: []string{"files", "note"}, FileFields: []string{"files"}}
	source := "package sdk\n// quote: \"; slash: \\; unicode: λ\n"
	raw, _ := json.Marshal(map[string]any{"files": []map[string]string{{"path": "sdk.go", "content": source}}, "note": "done"})
	got, err := artifactstore.NormalizeModelOutput(task, raw)
	if err != nil {
		t.Fatal(err)
	}
	files, err := artifactstore.DecodeFileMap([]byte(got["files"]))
	if err != nil || files["sdk.go"] != source {
		t.Fatalf("lost source: %#v %v", files, err)
	}
	if err = artifactstore.Accept(task, got); err != nil {
		t.Fatal(err)
	}
	schema := artifactstore.ModelSchema(task)["properties"].(map[string]any)
	if schema["files"].(map[string]any)["minItems"] != 1 {
		t.Fatal("file schema must reject empty arrays")
	}
	if schema["files"].(map[string]any)["type"] != "array" {
		t.Fatal("files must be structurally encoded")
	}
}

func TestTypedArtifactRejectsMalformed(t *testing.T) {
	task := model.Task{Requires: []string{"files"}, FileFields: []string{"files"}}
	for _, raw := range []string{
		`{"files":"{broken"}`, `{"files":[]}`, `{"files":[{"path":"a","content":null}]}`,
		`{"files":[{"path":"a","content":"x","extra":true}]}`,
		`{"files":[{"path":"a","path":"b","content":"x"}]}`,
		`{"files":[{"path":"a","content":"x"},{"path":"./a","content":"x"}]}`,
		`{"files":[{"path":"../a","content":"x"}]}`,
		`{"files":[{"path":"a","content":"x"}],"files":[]}`,
		`{"files":[{"path":"a","content":"x"}]} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := artifactstore.NormalizeModelOutput(task, []byte(raw)); err == nil {
				t.Fatal("accepted malformed artifact")
			}
		})
	}
}

func TestFileMapAcceptanceAndDeliverySafety(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"a":"x","a":"y"}`, `{"a":"x","./a":"x"}`, `{"a":null}`, `{"a":5}`, `{"a":"x","a/b":"y"}`, `{"C:/a":"x"}`, `{"a/../b":"x"}`, `{"a\\b":"x"}`, `{"/a":"x"}`, `{".swarm-delivery.json":"x"}`} {
		if _, err := artifactstore.DecodeFileMap([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
		if err := artifactstore.Accept(model.Task{Requires: []string{"files"}, FileFields: []string{"files"}}, map[string]string{"files": raw}); err == nil {
			t.Errorf("verified %s", raw)
		}
	}
	if _, err := artifactstore.DecodeFileMap([]byte(strings.Repeat(" ", model.MaxOutputBytes+1))); err == nil {
		t.Fatal("unbounded artifact")
	}
}

func TestLegacyModelOutputAndTaskEncoding(t *testing.T) {
	task := model.Task{ID: "a", Prompt: "produce files", Requires: []string{"files"}}
	raw := []byte(`{"files":"legacy arbitrary string"}`)
	got, err := artifactstore.NormalizeModelOutput(task, raw)
	if err != nil || got["files"] != "legacy arbitrary string" {
		t.Fatalf("legacy changed: %v %v", got, err)
	}
	encoded, _ := json.Marshal(task)
	if strings.Contains(string(encoded), "file_fields") {
		t.Fatal("legacy plan hash changed")
	}
	if err := Validate(model.Plan{Tasks: []model.Task{task}}); err != nil {
		t.Fatal(err)
	}
	for _, fields := range [][]string{{"missing"}, {"files", "files"}} {
		task.FileFields = fields
		if err := Validate(model.Plan{Tasks: []model.Task{task}}); err == nil {
			t.Fatal("accepted invalid file field declaration")
		}
	}
}

func TestRevisionCannotRemoveFileContract(t *testing.T) {
	old := model.Task{ID: "a", Requires: []string{"files"}, FileFields: []string{"files"}}
	next := old
	next.FileFields = nil
	if err := CapabilityError(&model.State{}, old, next); err == nil {
		t.Fatal("revision weakened file contract")
	}
}
