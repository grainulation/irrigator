package checkpoint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
)

func TestPlanSchemaSnapshot(t *testing.T) {
	got, err := planJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../docs/plan.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("schema drift: regenerate with irrigator schema > docs/plan.schema.json")
	}
}

// checkGeneratedShape interprets only the keywords emitted by our generator.
// It is a fixture check, deliberately not a general JSON Schema validator.
func checkGeneratedShape(s map[string]any, v any, defs map[string]any) error {
	if ref, ok := s["$ref"].(string); ok {
		return checkGeneratedShape(defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any), v, defs)
	}
	if branches, ok := s["anyOf"].([]any); ok {
		for _, branch := range branches {
			if checkGeneratedShape(branch.(map[string]any), v, defs) == nil {
				return nil
			}
		}
		return fmt.Errorf("no matching nullable branch")
	}
	if values, ok := s["enum"].([]any); ok {
		found := false
		for _, value := range values {
			if reflect.DeepEqual(value, v) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("invalid enum")
		}
	}
	switch s["type"] {
	case nil:
		return nil
	case "null":
		if v != nil {
			return fmt.Errorf("expected null")
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected bool")
		}
	case "integer", "number":
		n, ok := v.(float64)
		if !ok {
			return fmt.Errorf("expected number")
		}
		if s["type"] == "integer" && n != math.Trunc(n) {
			return fmt.Errorf("expected integer")
		}
		if min, ok := s["minimum"].(float64); ok && n < min {
			return fmt.Errorf("below minimum")
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fmt.Errorf("expected array")
		}
		for _, item := range a {
			if err := checkGeneratedShape(s["items"].(map[string]any), item, defs); err != nil {
				return err
			}
		}
	case "object":
		o, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		props, _ := s["properties"].(map[string]any)
		for k, value := range o {
			child, known := props[k]
			if !known {
				switch extra := s["additionalProperties"].(type) {
				case bool:
					if !extra {
						return fmt.Errorf("unknown field %s", k)
					}
					continue
				case map[string]any:
					child = extra
				default:
					continue
				}
			}
			if err := checkGeneratedShape(child.(map[string]any), value, defs); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	return nil
}
func decodedPlanSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := planJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func TestPlanSchemaExampleStructures(t *testing.T) {
	s := decodedPlanSchema(t)
	defs := s["$defs"].(map[string]any)
	paths, err := filepath.Glob("../../examples/*.json")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if _, plan := v["tasks"]; !plan {
			continue
		}
		if err := checkGeneratedShape(s, v, defs); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no plan fixtures checked")
	}
	for _, raw := range []string{`{"tasks":"not-array"}`, `{"tasks":[{"id":5}]}`, `{"providers":{"x":{"command":[false]}}}`, `{"api_version":"swarm/v999"}`, `{"unknown":true}`} {
		var v any
		json.Unmarshal([]byte(raw), &v)
		if checkGeneratedShape(s, v, defs) == nil {
			t.Fatalf("accepted malformed structure %s", raw)
		}
	}
}
func TestPlanSchemaRecursiveAndOmissionMetadata(t *testing.T) {
	s := decodedPlanSchema(t)
	defs := s["$defs"].(map[string]any)
	p := defs["Plan"].(map[string]any)["properties"].(map[string]any)
	if p["api_version"].(map[string]any)["x-go-omitempty"] != true || p["tasks"].(map[string]any)["x-go-omitempty"] != false {
		t.Fatal("JSON omission metadata lost")
	}
	if _, ok := defs["ArtifactContract"]; !ok {
		t.Fatal("recursive artifact contracts absent")
	}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if _, exists := defs[strings.TrimPrefix(ref, "#/$defs/")]; !exists {
					t.Fatalf("unresolved ref %s", ref)
				}
			}
			for _, child := range x {
				visit(child)
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	visit(s)
	b := &artifactstore.PlanSchemaBuilder{Defs: map[string]any{}}
	raw := b.Schema(reflect.TypeOf(json.RawMessage{}))
	if _, typed := raw["type"]; typed {
		t.Fatal("raw JSON incorrectly modeled as byte array")
	}
}
