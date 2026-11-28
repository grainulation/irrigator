package checkpoint

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

// sharedMemory reports the first place where a and b, walked in step through
// exported fields, hold the same non-empty map, slice backing array or pointer.
func sharedMemory(a, b reflect.Value, path string) string {
	if !a.IsValid() || !b.IsValid() || a.Type() != b.Type() {
		return ""
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return ""
		}
		if a.Pointer() == b.Pointer() {
			return path
		}
		return sharedMemory(a.Elem(), b.Elem(), path+"*")
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return ""
		}
		return sharedMemory(a.Elem(), b.Elem(), path+".(iface)")
	case reflect.Map:
		if a.IsNil() || b.IsNil() || a.Len() == 0 {
			return ""
		}
		if a.Pointer() == b.Pointer() {
			return path
		}
		it := a.MapRange()
		for it.Next() {
			if got := sharedMemory(it.Value(), b.MapIndex(it.Key()), fmt.Sprintf("%s[%v]", path, it.Key())); got != "" {
				return got
			}
		}
	case reflect.Slice:
		if a.IsNil() || b.IsNil() || a.Len() == 0 {
			return ""
		}
		if a.Pointer() == b.Pointer() {
			return path
		}
		for i := 0; i < a.Len(); i++ {
			if got := sharedMemory(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i)); got != "" {
				return got
			}
		}
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !a.Type().Field(i).IsExported() {
				continue
			}
			if got := sharedMemory(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name); got != "" {
				return got
			}
		}
	}
	return ""
}

func cloneTestStates(t *testing.T) map[string]*model.State {
	t.Helper()
	states := map[string]*model.State{}
	root := fixture(t, task("a", 0), task("b", 0, "a"), task("c", 0, "a", "b"))
	s, err := ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	s.Control = "pause"
	s.Tasks["a"].Notes = map[string]string{"review": "retained", "k": "v"}
	s.Tasks["a"].Result = map[string]string{"answer": "ok"}
	s.Tasks["b"].Children = []string{"x", "y"}
	s.Tasks["b"].Usage = &model.TaskUsage{}
	s.Event("note", "a", map[string]any{"nested": map[string]any{"list": []any{"a", 1.0, map[string]any{"z": "y"}}}})
	s.Event("note2", "", []string{"plain", "slice"})
	states["plain-fixture"] = s
	states["managed-shared-profile"] = sharedProfileState(t, 40, true)
	states["zero"] = &model.State{}
	return states
}

func TestCloneCheckpointSharesNothing(t *testing.T) {
	for name, s := range cloneTestStates(t) {
		t.Run(name, func(t *testing.T) {
			got := cloneCheckpoint(s)
			if !reflect.DeepEqual(got, s) {
				t.Fatal("clone differs from the original")
			}
			if shared := sharedMemory(reflect.ValueOf(s), reflect.ValueOf(got), "State"); shared != "" {
				t.Fatalf("clone shares memory with the original at %s", shared)
			}
		})
	}
}

func TestCloneCheckpointIsIndependentOfOriginal(t *testing.T) {
	s := cloneTestStates(t)["plain-fixture"]
	snapshot := cloneCheckpoint(s)
	c := cloneCheckpoint(s)
	c.Control = "down"
	c.Tasks["a"].Notes["review"] = "mutated"
	c.Tasks["a"].Result["answer"] = "mutated"
	c.Tasks["b"].Children[0] = "mutated"
	c.Events[len(c.Events)-1].Kind = "mutated"
	c.Events[len(c.Events)-2].Detail.(map[string]any)["nested"].(map[string]any)["list"].([]any)[0] = "mutated"
	delete(c.Tasks, "c")
	if !reflect.DeepEqual(s, snapshot) {
		t.Fatal("mutating a clone changed the original")
	}
}

func BenchmarkCloneCheckpoint(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		s := sharedProfileState(b, n, true)
		b.Run(fmt.Sprintf("tasks=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				cloneCheckpoint(s)
			}
		})
	}
}
