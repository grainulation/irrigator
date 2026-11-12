package artifactstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"reflect"
	"strings"

	"github.com/grainulation/irrigator/internal/model"
)

// File and spawn fields use typed schemas on the model wire. The checkpoint and
// worker protocol carry canonical JSON strings.
func ModelSchema(task model.Task) map[string]any {
	properties := map[string]any{}
	for _, key := range task.Requires {
		properties[key] = map[string]any{"type": "string"}
		if contract, ok := task.ArtifactContracts[key]; ok {
			raw, _ := json.Marshal(contract)
			properties[key] = map[string]any{"type": "string", "description": "Canonical compact JSON text (sorted object keys) satisfying this bounded Irrigator contract: " + string(raw)}
		}
	}
	for _, key := range task.FileFields {
		properties[key] = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"path", "content"}, "additionalProperties": false}}
	}
	schema := map[string]any{"type": "object", "properties": properties, "required": task.Requires, "additionalProperties": false}
	if task.Spawn != nil {
		// Reuse the runtime Task schema so optional child fields cannot drift.
		// Admission still enforces capabilities, graph limits and dependencies.
		b := &PlanSchemaBuilder{Defs: map[string]any{}}
		child := b.Schema(reflect.TypeOf(model.Task{}))
		taskSchema := b.Defs["Task"].(map[string]any)
		taskSchema["required"] = []string{"id", "prompt", "requires"}
		childProps := taskSchema["properties"].(map[string]any)
		childProps["id"] = map[string]any{"type": "string", "minLength": 1}
		childProps["prompt"] = map[string]any{"type": "string", "minLength": 1}
		childProps["requires"] = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1}}
		spawn := b.Defs["SpawnPolicy"].(map[string]any)
		spawn["required"] = []string{"field"}
		spawn["properties"].(map[string]any)["field"] = map[string]any{"type": "string", "minLength": 1}
		limit := task.Spawn.MaxChildren
		if limit == 0 {
			limit = 8
		}
		spawn["properties"].(map[string]any)["max_children"] = map[string]any{"type": "integer", "minimum": 0, "maximum": limit}
		properties[task.Spawn.Field] = map[string]any{"type": "object", "properties": map[string]any{"tasks": map[string]any{"type": "array", "maxItems": limit, "items": child}}, "required": []string{"tasks"}, "additionalProperties": false}
		schema["$defs"] = b.Defs
	}
	stripModelSchemaAnnotations(schema)
	return schema
}

// Request-aware constraints are derived from the admitted snapshot. They help
// models produce valid proposals; runtime admission remains authoritative.
func ModelSchemaForRequest(req model.WorkRequest) map[string]any {
	schema := ModelSchema(req.Task)
	props := schema["properties"].(map[string]any)
	bounds := req.Boundaries
	if req.Task.Spawn != nil && bounds != nil {
		tasks := props[req.Task.Spawn.Field].(map[string]any)["properties"].(map[string]any)["tasks"].(map[string]any)
		tasks["maxItems"] = bounds.SpawnChildrenRemaining
		defs := schema["$defs"].(map[string]any)
		childProps := defs["Task"].(map[string]any)["properties"].(map[string]any)
		childProps["id"] = map[string]any{"type": "string", "minLength": 1, "pattern": "^[a-zA-Z0-9_-]+$", "description": "Unique local ID; do not include __ or dots. Irrigator adds the parent namespace."}
		if bounds.RemainingDepth <= 1 {
			childProps["spawn"] = map[string]any{"type": "null", "description": "The proposed children are at the depth limit and must be leaves."}
		}
	}
	if req.Task.Turn != nil && TurnContains(req.Task.Requires, TurnRequestField) {
		b := &PlanSchemaBuilder{Defs: map[string]any{}}
		child := b.Schema(reflect.TypeOf(model.Task{}))
		def := b.Defs["Task"].(map[string]any)
		def["required"] = []string{"id", "prompt", "requires"}
		childProps := def["properties"].(map[string]any)
		childProps["id"] = map[string]any{"type": "string", "minLength": 1, "pattern": "^[a-zA-Z0-9_-]+$"}
		childProps["prompt"] = map[string]any{"type": "string", "minLength": 1}
		childProps["requires"] = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1}}
		for _, key := range []string{"turn", "parent", "spawn", "depends_on", "input_fields", "profile", "provider", "effort"} {
			delete(childProps, key)
		}
		helpers := 64
		canContinue := req.Task.Turn.Role == "main"
		if bounds != nil {
			helpers = bounds.HelpersRemaining
			canContinue = canContinue && bounds.RemainingTurns > 0 && bounds.TaskSlotsRemaining > 0
		}
		if req.Task.Turn.Role != "main" {
			helpers = 0
		}
		requestProps := map[string]any{"helpers": map[string]any{"type": "array", "maxItems": helpers, "items": child}, "wait_for_helpers": map[string]any{"type": "boolean", "enum": []bool{false}}}
		if canContinue {
			requestProps["continue"] = child
		}
		variants := []any{map[string]any{"type": "object", "properties": requestProps, "additionalProperties": false}}
		if canContinue {
			waitProps := map[string]any{"continue": child, "helpers": map[string]any{"type": "array", "maxItems": helpers, "items": child}, "wait_for_helpers": map[string]any{"type": "boolean", "enum": []bool{true}}}
			if bounds != nil {
				waitProps["helpers"].(map[string]any)["maxItems"] = min(helpers, max(0, bounds.TaskSlotsRemaining-1))
			}
			variants = append(variants, map[string]any{"type": "object", "properties": waitProps, "required": []string{"continue", "wait_for_helpers"}, "additionalProperties": false})
		}
		turnSchema := variants[0].(map[string]any)
		if len(variants) > 1 {
			turnSchema = map[string]any{"anyOf": variants}
		}
		turnSchema["description"] = "Always return a JSON object for turn_request. Return {} to stop; never omit its value or encode it as a string. Helpers are one-shot. Waiting requires a permitted next turn."
		props[TurnRequestField] = turnSchema
		schema["$defs"] = b.Defs
	}
	stripModelSchemaAnnotations(schema)
	return schema
}

// Editor-only annotations are not accepted by strict provider JSON Schema
// validators. Walk schema positions, not property names: an output field named
// "x-note" is user data and must remain in properties and required.
func stripModelSchemaAnnotations(schema map[string]any) {
	for key, value := range schema {
		if strings.HasPrefix(key, "x-") {
			delete(schema, key)
			continue
		}
		switch key {
		case "properties", "$defs", "definitions", "patternProperties":
			if named, ok := value.(map[string]any); ok {
				for _, child := range named {
					if node, ok := child.(map[string]any); ok {
						stripModelSchemaAnnotations(node)
					}
				}
			}
		case "items", "additionalProperties", "not", "if", "then", "else":
			if child, ok := value.(map[string]any); ok {
				stripModelSchemaAnnotations(child)
			}
		case "anyOf", "allOf", "oneOf", "prefixItems":
			if children, ok := value.([]any); ok {
				for _, child := range children {
					if node, ok := child.(map[string]any); ok {
						stripModelSchemaAnnotations(node)
					}
				}
			}
		}
	}
}

func decodeUniqueObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) > model.MaxOutputBytes {
		return nil, fmt.Errorf("artifact exceeds %d bytes", model.MaxOutputBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	result := map[string]json.RawMessage{}
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("invalid object key")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	if _, err = dec.Token(); err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return result, nil
}

func NormalizeModelOutput(task model.Task, raw json.RawMessage) (map[string]string, error) {
	values, err := decodeUniqueObject(raw)
	if err != nil {
		return nil, err
	}
	fileFields := map[string]bool{}
	for _, key := range task.FileFields {
		fileFields[key] = true
	}
	result := map[string]string{}
	for key, value := range values {
		if ((task.Spawn != nil && key == task.Spawn.Field) || (task.Turn != nil && key == TurnRequestField)) && len(value) > 0 && (value[0] == '{' || value[0] == '[') {
			// Preserve proposed child data even when graph admission later rejects
			// it. Compaction retains duplicate keys for strict admission to reject;
			// decoding into a map here would erase that evidence. Existing string
			// fields stay byte-for-byte stable in checkpoints.
			var compact bytes.Buffer
			if err := json.Compact(&compact, value); err != nil {
				return nil, err
			}
			result[key] = compact.String()
			continue
		}
		if !fileFields[key] {
			var text string
			if err = json.Unmarshal(value, &text); err != nil || string(value) == "null" {
				return nil, fmt.Errorf("field %q must be a string", key)
			}
			result[key] = text
			continue
		}
		var entries []json.RawMessage
		if err = json.Unmarshal(value, &entries); err != nil || len(entries) == 0 {
			return nil, fmt.Errorf("file field %q must be a nonempty array of path/content objects", key)
		}
		files := map[string]string{}
		for _, entry := range entries {
			fields, e := decodeUniqueObject(entry)
			if e != nil {
				return nil, e
			}
			if len(fields) != 2 || fields["path"] == nil || fields["content"] == nil {
				return nil, fmt.Errorf("file entries require only path and content")
			}
			var name, content string
			if json.Unmarshal(fields["path"], &name) != nil || json.Unmarshal(fields["content"], &content) != nil || string(fields["content"]) == "null" {
				return nil, fmt.Errorf("file path and content must be strings")
			}
			if e = addArtifactFile(files, name, content); e != nil {
				return nil, e
			}
		}
		if err = validateFileTree(files); err != nil {
			return nil, err
		}
		canonical, _ := json.Marshal(files)
		if len(canonical) > model.MaxOutputBytes {
			return nil, fmt.Errorf("file field exceeds size limit")
		}
		result[key] = string(canonical)
	}
	if err = Accept(task, result); err != nil {
		return nil, err
	}
	return result, nil
}

func addArtifactFile(files map[string]string, name, content string) error {
	// Slash paths are portable across hosts. Reject Windows drives, traversal,
	// control bytes and reserved manifests even when running on Unix.
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") {
		return fmt.Errorf("unsafe artifact path %q", name)
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return fmt.Errorf("unsafe artifact path %q", name)
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return fmt.Errorf("unsafe artifact path %q", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".swarm-delivery.json" || strings.HasPrefix(clean, ".swarm-delivery.json/") {
		return fmt.Errorf("reserved or empty artifact path %q", name)
	}
	if _, exists := files[clean]; exists {
		return fmt.Errorf("duplicate normalized artifact path %q", clean)
	}
	files[clean] = content
	return nil
}

func validateFileTree(files map[string]string) error {
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists {
				return fmt.Errorf("file/directory collision %q", name)
			}
		}
	}
	return nil
}

func DecodeFileMap(raw []byte) (map[string]string, error) {
	values, err := decodeUniqueObject(raw)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("file map must not be empty")
	}
	files := map[string]string{}
	for name, value := range values {
		var content string
		if json.Unmarshal(value, &content) != nil || string(value) == "null" {
			return nil, fmt.Errorf("file content must be a string")
		}
		if err = addArtifactFile(files, name, content); err != nil {
			return nil, err
		}
	}
	if err = validateFileTree(files); err != nil {
		return nil, err
	}
	return files, nil
}
