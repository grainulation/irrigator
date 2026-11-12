package checkpoint

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/model"
)

func TestSpawnModelSchemaRequiresTypedChildren(t *testing.T) {
	task := model.Task{Requires: []string{"answer", "children"}, Spawn: &model.SpawnPolicy{Field: "children", MaxChildren: 3}}
	schema := artifactstore.ModelSchema(task)
	props := schema["properties"].(map[string]any)
	children := props["children"].(map[string]any)
	if children["type"] != "object" {
		t.Fatal("children remain unenforced string")
	}
	tasks := children["properties"].(map[string]any)["tasks"].(map[string]any)
	if tasks["type"] != "array" || tasks["maxItems"] != 3 {
		t.Fatal("missing child bounds")
	}
	defs := schema["$defs"].(map[string]any)
	child := defs["Task"].(map[string]any)
	if !reflect.DeepEqual(child["required"], []string{"id", "prompt", "requires"}) {
		t.Fatal("missing required child fields")
	}
	if !reflect.DeepEqual(defs["SpawnPolicy"].(map[string]any)["required"], []string{"field"}) {
		t.Fatal("nested spawn is untyped")
	}
	raw, err := json.Marshal(schema)
	if err != nil || !json.Valid(raw) {
		t.Fatal("invalid model schema", err)
	}
}

func TestTypedSpawnNormalizesAndRetainsLegacyStrings(t *testing.T) {
	parent := model.Task{Requires: []string{"answer", "children"}, Spawn: &model.SpawnPolicy{Field: "children"}}
	proposal := `{"tasks":[{"id":"child","prompt":"work","requires":["answer"]}]}`
	out, err := artifactstore.NormalizeModelOutput(parent, json.RawMessage(`{"answer":"paid result","children":`+proposal+`}`))
	if err != nil || out["children"] != proposal {
		t.Fatalf("typed output not preserved: %v %v", out, err)
	}
	legacy, _ := json.Marshal(map[string]string{"answer": "paid result", "children": "  " + proposal + "\n"})
	out, err = artifactstore.NormalizeModelOutput(parent, legacy)
	if err != nil || out["children"] != "  "+proposal+"\n" {
		t.Fatalf("legacy bytes changed: %v %v", out, err)
	}
}

func TestSpawnModelSchemaUsesOnlyStrictWireKeywords(t *testing.T) {
	schema := artifactstore.ModelSchema(model.Task{Requires: []string{"x-note", "children"}, Spawn: &model.SpawnPolicy{Field: "children"}})
	allowed := map[string]bool{}
	for _, key := range []string{"type", "properties", "required", "additionalProperties", "$defs", "$ref", "anyOf", "items", "minItems", "maxItems", "minLength", "minimum", "maximum", "description", "format", "contentEncoding"} {
		allowed[key] = true
	}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		for key, value := range node {
			if !allowed[key] {
				t.Errorf("unsupported model schema keyword %q", key)
			}
			switch key {
			case "properties", "$defs":
				for _, child := range value.(map[string]any) {
					walk(child.(map[string]any))
				}
			case "items", "additionalProperties":
				if child, ok := value.(map[string]any); ok {
					walk(child)
				}
			case "anyOf":
				for _, child := range value.([]any) {
					walk(child.(map[string]any))
				}
			}
		}
	}
	walk(schema)
	if schema["properties"].(map[string]any)["x-note"] == nil {
		t.Fatal("stripped user output field")
	}
	editor, err := planJSONSchema()
	if err != nil || !strings.Contains(string(editor), "x-go-omitempty") {
		t.Fatal("wire filtering changed editor schema")
	}
}
