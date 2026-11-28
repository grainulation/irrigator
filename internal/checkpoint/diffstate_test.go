package checkpoint

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

// referenceDiffState is the original diff, which compares every node before
// descending. diffState must emit exactly the same patches.
func referenceDiffState(a, b reflect.Value, path []string, out *[]statePatch) error {
	if reflect.DeepEqual(a.Interface(), b.Interface()) {
		return nil
	}
	emit := func(value reflect.Value, appendOnly bool) error {
		raw, err := json.Marshal(value.Interface())
		if err == nil {
			*out = append(*out, statePatch{Path: append([]string{}, path...), Value: raw, Append: appendOnly})
		}
		return err
	}
	if a.Kind() == reflect.Pointer && !a.IsNil() && !b.IsNil() {
		return referenceDiffState(a.Elem(), b.Elem(), path, out)
	}
	switch a.Kind() {
	case reflect.Struct:
		if a.Type().PkgPath() == "time" {
			return emit(b, false)
		}
		for i := 0; i < a.NumField(); i++ {
			field := a.Type().Field(i)
			name := jsonField(field)
			if !field.IsExported() || name == "-" {
				continue
			}
			if err := referenceDiffState(a.Field(i), b.Field(i), append(append([]string{}, path...), name), out); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return emit(b, false)
		}
		if a.Type().Key().Kind() != reflect.String {
			return emit(b, false)
		}
		it := a.MapRange()
		for it.Next() {
			key := it.Key()
			next := b.MapIndex(key)
			p := append(append([]string{}, path...), key.String())
			if !next.IsValid() {
				*out = append(*out, statePatch{Path: p, Remove: true})
				continue
			}
			if err := referenceDiffState(it.Value(), next, p, out); err != nil {
				return err
			}
		}
		it = b.MapRange()
		for it.Next() {
			if !a.MapIndex(it.Key()).IsValid() {
				raw, err := json.Marshal(it.Value().Interface())
				if err != nil {
					return err
				}
				*out = append(*out, statePatch{Path: append(append([]string{}, path...), it.Key().String()), Value: raw})
			}
		}
		return nil
	case reflect.Slice:
		if b.Len() > a.Len() && reflect.DeepEqual(a.Interface(), b.Slice(0, a.Len()).Interface()) {
			return emit(b.Slice(a.Len(), b.Len()), true)
		}
	}
	return emit(b, false)
}

func patchKeys(patches []statePatch) []string {
	keys := make([]string, 0, len(patches))
	for _, p := range patches {
		keys = append(keys, fmt.Sprintf("%s|remove=%t|append=%t|%s", strings.Join(p.Path, "/"), p.Remove, p.Append, p.Value))
	}
	sort.Strings(keys) // map iteration order is random in both implementations
	return keys
}

func diffBoth(t *testing.T, before, after *model.State) []string {
	t.Helper()
	var want, got []statePatch
	if err := referenceDiffState(reflect.ValueOf(before).Elem(), reflect.ValueOf(after).Elem(), nil, &want); err != nil {
		t.Fatal(err)
	}
	if err := diffState(reflect.ValueOf(before).Elem(), reflect.ValueOf(after).Elem(), nil, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(patchKeys(want), patchKeys(got)) {
		t.Fatalf("diff differs from the reference\nwant: %v\n got: %v", patchKeys(want), patchKeys(got))
	}
	return patchKeys(got)
}

func TestDiffStateMatchesReference(t *testing.T) {
	base := cloneTestStates(t)["plain-fixture"]
	cases := map[string]func(s *model.State){
		"unchanged":            func(s *model.State) {},
		"scalar field":         func(s *model.State) { s.Control = "down" },
		"task field":           func(s *model.State) { s.Tasks["a"].Attempt = 7 },
		"task string":          func(s *model.State) { s.Tasks["b"].Error = "boom" },
		"task map set":         func(s *model.State) { s.Tasks["a"].Result["answer"] = "changed" },
		"task map added key":   func(s *model.State) { s.Tasks["a"].Result["extra"] = "x" },
		"task map removed key": func(s *model.State) { delete(s.Tasks["a"].Notes, "k") },
		"task map nil":         func(s *model.State) { s.Tasks["a"].Notes = nil },
		"task map empty":       func(s *model.State) { s.Tasks["a"].Result = map[string]string{} },
		"slice replaced":       func(s *model.State) { s.Tasks["b"].Children = []string{"z"} },
		"slice appended":       func(s *model.State) { s.Tasks["b"].Children = append(s.Tasks["b"].Children, "w") },
		"slice shortened":      func(s *model.State) { s.Tasks["b"].Children = s.Tasks["b"].Children[:1] },
		"slice nil":            func(s *model.State) { s.Tasks["b"].Children = nil },
		"pointer set":          func(s *model.State) { s.Tasks["a"].Usage = &model.TaskUsage{} },
		"pointer cleared":      func(s *model.State) { s.Tasks["b"].Usage = nil },
		"pointer field":        func(s *model.State) { s.Tasks["b"].Usage.InputTokens = 5 },
		"task added":           func(s *model.State) { s.Tasks["new"] = &model.TaskState{Status: "pending"} },
		"task removed":         func(s *model.State) { delete(s.Tasks, "c") },
		"event appended":       func(s *model.State) { s.Event("later", "a", map[string]string{"k": "v"}) },
		"event edited":         func(s *model.State) { s.Events[0].Kind = "edited" },
		"event detail edited": func(s *model.State) {
			s.Events[len(s.Events)-2].Detail.(map[string]any)["nested"].(map[string]any)["list"].([]any)[0] = "x"
		},
		"time field": func(s *model.State) { s.Events[0].Time = s.Events[0].Time.Add(time.Second) },
		"many changes": func(s *model.State) {
			s.Control = "x"
			s.Tasks["a"].Attempt = 2
			delete(s.Tasks, "b")
			s.Event("e", "", nil)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			before, after := cloneCheckpoint(base), cloneCheckpoint(base)
			mutate(after)
			diffBoth(t, before, after)
			if name == "unchanged" {
				if keys := diffBoth(t, before, after); len(keys) != 0 {
					t.Fatalf("unchanged state produced patches: %v", keys)
				}
			}
		})
	}
	t.Run("both nil maps", func(t *testing.T) {
		before, after := cloneCheckpoint(base), cloneCheckpoint(base)
		before.Tasks["a"].Notes, after.Tasks["a"].Notes = nil, nil
		if keys := diffBoth(t, before, after); len(keys) != 0 {
			t.Fatalf("nil maps produced patches: %v", keys)
		}
	})
	t.Run("managed shared profile", func(t *testing.T) {
		before := sharedProfileState(t, 30, false)
		after := cloneCheckpoint(before)
		for id := range after.Tasks {
			after.Tasks[id].Status = "verified"
			break
		}
		diffBoth(t, before, after)
	})
}
