package graph

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestNormalizeChildProposalForms(t *testing.T) {
	for _, source := range []string{`[{"id":"a"}]`, " {\"tasks\":[{\"id\":\"a\"}]}\n"} {
		got, err := normalizeChildProposal(source)
		if err != nil || got != `{"tasks":[{"id":"a"}]}` {
			t.Fatalf(`%s -> %q, %v`, source, got, err)
		}
	}
	for _, source := range []string{`[]`, `{}`, `{"tasks":null}`, `{"tasks":[]}`} {
		got, err := normalizeChildProposal(source)
		if err != nil || got != `{"tasks":[]}` {
			t.Fatalf(`%s -> %q, %v`, source, got, err)
		}
	}
}

func TestNormalizeChildDisplayMove(t *testing.T) {
	got, err := normalizeChildProposal(`[{"id":"a","title":"T","why_spawned":"W","done_when":"D","outcome_field":"outcome","display":{"title":"T"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil || len(parsed.Tasks) != 1 {
		t.Fatalf(`%q %v`, got, err)
	}
	child := parsed.Tasks[0]
	for _, key := range childDisplayKeys {
		if _, left := child[key]; left {
			t.Fatalf(`%s not moved: %s`, key, got)
		}
	}
	display, _ := child[`display`].(map[string]any)
	if len(display) != 4 || display[`title`] != `T` || display[`why_spawned`] != `W` || display[`done_when`] != `D` || display[`outcome_field`] != `outcome` {
		t.Fatalf(`display %v`, display)
	}
}

func TestNormalizeChildProposalRejects(t *testing.T) {
	cases := map[string]string{
		`duplicate`:        `[{"id":"a","id":"b"}]`,
		`nested duplicate`: `[{"id":"a","input_fields":{"x":[],"x":[]}}]`,
		`case collision`:   `[{"id":"a","ID":"b"}]`,
		`trailing array`:   `[] x`,
		`trailing object`:  `{"tasks":[]} {}`,
		`unknown top key`:  `{"tasks":[],"extra":1}`,
		`bare title`:       `{"tasks":[],"title":"x"}`,
		`non object child`: `[1]`,
		`null child`:       `[null]`,
		`scalar`:           `"x"`,
		`empty`:            ``,
		`syntax`:           `[{"id":}`,
		`tasks type`:       `{"tasks":{}}`,
		`conflict`:         `[{"id":"a","title":"A","display":{"title":"B"}}]`,
		`non string`:       `[{"id":"a","title":5}]`,
		`display type`:     `[{"id":"a","display":"x"}]`,
		`deep`:             strings.Repeat(`[`, 100) + strings.Repeat(`]`, 100),
		`oversize`:         `["` + strings.Repeat(`a`, childNormalizeMaxBytes) + `"]`,
	}
	for name, source := range cases {
		if got, err := normalizeChildProposal(source); err == nil {
			t.Errorf(`%s accepted: %s`, name, got)
		}
	}
}

func TestChildEffort(t *testing.T) {
	ok := [][3]string{{`high`, ``, `high`}, {`high`, `low`, `low`}, {`high`, `high`, `high`}, {`medium`, `low`, `low`}, {``, ``, ``}}
	for _, c := range ok {
		if got, err := childEffort(c[0], c[1]); err != nil || got != c[2] {
			t.Errorf(`%v -> %q %v`, c, got, err)
		}
	}
	bad := [][2]string{{`low`, `high`}, {`medium`, `high`}, {`low`, `medium`}, {``, `low`}, {`high`, `max`}, {`odd`, `low`}}
	for _, c := range bad {
		if got, err := childEffort(c[0], c[1]); err == nil {
			t.Errorf(`%v accepted as %q`, c, got)
		}
	}
}

func normalizationState(effort, source string) (*model.State, *model.TaskState) {
	parent := model.Task{ID: `p`, Prompt: `x`, Requires: []string{`children`}, Provider: `mock`, Effort: effort, Spawn: &model.SpawnPolicy{Field: `children`}}
	row := &model.TaskState{Spec: parent, Status: `produced`, Result: map[string]string{`children`: source}}
	return &model.State{Plan: model.Plan{Name: `n`, Tasks: []model.Task{parent}}, Backend: `mock`, Tasks: map[string]*model.TaskState{`p`: row}}, row
}

func TestExpansionNormalizesBareArrayPreservingSource(t *testing.T) {
	source := `[{"id":"c","prompt":"y","requires":["r"],"effort":"low"}]`
	s, row := normalizationState(`high`, source)
	children, hash, err := Expansion(s, row)
	if err != nil || len(children) != 1 {
		t.Fatalf(`%v %v`, children, err)
	}
	if children[0].ID != `p__c` || children[0].Effort != `low` || children[0].Provider != `mock` {
		t.Fatalf(`%+v`, children[0])
	}
	if hash != fmt.Sprintf(`%x`, sha256.Sum256([]byte(source))) || row.Result[`children`] != source || row.ExpansionHash != `` {
		t.Fatal(`original bytes/hash/state must be preserved`)
	}
}

func TestExpansionEffortPolicy(t *testing.T) {
	child := func(effort string) string {
		return `{"tasks":[{"id":"c","prompt":"y","requires":["r"]` + effort + `}]}`
	}
	s, row := normalizationState(`medium`, child(``))
	if children, _, err := Expansion(s, row); err != nil || children[0].Effort != `medium` {
		t.Fatalf(`inherit: %v %v`, children, err)
	}
	s, row = normalizationState(`medium`, child(`,"effort":"high"`))
	if _, _, err := Expansion(s, row); err == nil || !strings.Contains(err.Error(), `increase`) {
		t.Fatalf(`higher effort: %v`, err)
	}
	s, row = normalizationState(``, child(`,"effort":"low"`))
	if _, _, err := Expansion(s, row); err == nil {
		t.Fatal(`explicit child effort with unspecified parent must fail`)
	}
}

func TestExpansionRejectsMalformedProposal(t *testing.T) {
	for _, source := range []string{`[{"id":"c","id":"d"}]`, `[] []`, `{"tasks":[],"x":1}`, `[{"id":"c","prompt":"y","requires":["r"],"provider":"","bogus":1}]`} {
		s, row := normalizationState(`high`, source)
		if _, _, err := Expansion(s, row); err == nil {
			t.Errorf(`accepted %s`, source)
		}
	}
}
