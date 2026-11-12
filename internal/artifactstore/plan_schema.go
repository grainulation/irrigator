package artifactstore

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

type PlanSchemaBuilder struct{ Defs map[string]any }

func schemaNullable(s map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
}
func (b *PlanSchemaBuilder) Schema(t reflect.Type) map[string]any {
	if t == reflect.TypeOf(json.RawMessage{}) {
		return map[string]any{"description": "Raw JSON value; runtime validation is authoritative."}
	}
	if t == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return schemaNullable(b.Schema(t.Elem()))
	case reflect.Interface:
		return map[string]any{"description": "Arbitrary JSON value; runtime validation is authoritative."}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer", "minimum": 0}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return schemaNullable(map[string]any{"type": "string", "contentEncoding": "base64"})
		}
		return schemaNullable(map[string]any{"type": "array", "items": b.Schema(t.Elem())})
	case reflect.Array:
		return map[string]any{"type": "array", "items": b.Schema(t.Elem()), "minItems": t.Len(), "maxItems": t.Len()}
	case reflect.Map:
		return schemaNullable(map[string]any{"type": "object", "additionalProperties": b.Schema(t.Elem())})
	case reflect.Struct:
		name := t.Name()
		if name != "" {
			ref := map[string]any{"$ref": "#/$defs/" + name}
			if _, ok := b.Defs[name]; ok {
				return ref
			}
			b.Defs[name] = map[string]any{} // reserve before recursive fields
			b.Defs[name] = b.object(t)
			return ref
		}
		return b.object(t)
	default:
		panic("unsupported plan schema Go type: " + t.String())
	}
}
func (b *PlanSchemaBuilder) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")
		if tag[0] == "-" {
			continue
		}
		name := tag[0]
		if name == "" {
			name = f.Name
		}
		// Plan types use explicitly named fields, not flattened anonymous structs.
		if f.Anonymous && tag[0] == "" {
			panic("anonymous JSON fields require explicit schema support")
		}
		s := b.Schema(f.Type)
		omit := false
		for _, option := range tag[1:] {
			if option == "omitempty" || option == "omitzero" {
				omit = true
			}
			if option == "string" {
				s = map[string]any{"type": "string"}
			}
		}
		s["x-go-omitempty"] = omit
		props[name] = s
	}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
}
