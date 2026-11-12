package model

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// TurnIdentity separates stable logical agents from their durable executions.
// Only first main turns are user-admitted; subsequent identities are runtime-owned.
type TurnIdentity struct {
	Async        *HelperAdmission `json:"async,omitempty"`
	AgentID      string           `json:"agent_id"`
	Role         string           `json:"role"`
	Index        int              `json:"index"`
	PreviousTask string           `json:"previous_task,omitempty"`
	OwnerAgentID string           `json:"owner_agent_id,omitempty"`
	OwnerTurn    int              `json:"owner_turn,omitempty"`
}

// encoding/json accepts case-insensitive aliases. Runtime admission accepts only
// canonical tagged keys, recursively, in addition to rejecting duplicate keys.
func ExactTurnJSONFields(value any, typ reflect.Type) error {
	return exactTurnJSONFieldsAt(value, typ, "$")
}
func exactTurnJSONFieldsAt(value any, typ reflect.Type, path string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if value == nil {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		obj, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag != "" && tag != "-" {
				fields[tag] = f.Type
			}
		}
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			v := obj[key]
			field, ok := fields[key]
			if !ok {
				return fmt.Errorf("noncanonical turn request field %q at %s", key, path)
			}
			if err := exactTurnJSONFieldsAt(v, field, path+"."+key); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if values, ok := value.([]any); ok {
			for i, v := range values {
				if err := exactTurnJSONFieldsAt(v, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if obj, ok := value.(map[string]any); ok {
			keys := make([]string, 0, len(obj))
			for key := range obj {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := exactTurnJSONFieldsAt(obj[key], typ.Elem(), path+"."+key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
