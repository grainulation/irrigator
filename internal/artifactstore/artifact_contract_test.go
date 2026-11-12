package artifactstore

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func reportContract() model.ArtifactContract {
	return model.ArtifactContract{Type: "object", MaxBytes: 512, Properties: map[string]model.ArtifactContract{
		"count": {Type: "integer", MaxBytes: 16}, "ready": {Type: "boolean", MaxBytes: 5},
		"tags": {Type: "array", MaxBytes: 128, MaxItems: 2, Items: &model.ArtifactContract{Type: "string", MaxBytes: 32}},
	}}
}
func TestArtifactContractAdversarial(t *testing.T) {
	c := reportContract()
	valid := `{"count":2,"ready":true,"tags":["release","tested"]}`
	if _, err := ArtifactValue(c, valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`null`, `[]`, `{"count":"2","ready":true,"tags":[]}`,
		`{"count":2.5,"ready":true,"tags":[]}`, `{"count":2,"ready":true,"tags":["a","b","c"]}`,
		`{"count":2,"ready":true,"tags":[false]}`, `{"count":2,"ready":true,"tags":[],"extra":1}`,
		`{"count":2,"tags":[]}`, `{"count":2,"count":2,"ready":true,"tags":[]}`,
		`{"count":2,"ready":null,"tags":[]}`, `{"count":1e999,"ready":true,"tags":[]}`,
		valid + ` {}`, `{"count":NaN}`, `{"count":2`, strings.Repeat("[", 100),
	} {
		if _, err := ArtifactValue(c, raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	task := model.Task{Requires: []string{"report"}, ArtifactContracts: map[string]model.ArtifactContract{"report": c}}
	if err := Accept(task, map[string]string{"report": valid}); err != nil {
		t.Fatal(err)
	}
	if err := Accept(task, map[string]string{"report": " " + valid}); err == nil {
		t.Fatal("accepted noncanonical value")
	}
	if err := Accept(model.Task{Requires: []string{"report"}}, map[string]string{"report": "opaque legacy text"}); err != nil {
		t.Fatal(err)
	}
}
func TestArtifactContractNarrowing(t *testing.T) {
	c := reportContract()
	narrow := reportContract()
	narrow.MaxBytes = 256
	if !ArtifactAssignable(narrow, c) || ArtifactAssignable(c, narrow) {
		t.Fatal("byte narrowing violated")
	}
	if !ArtifactAssignable(model.ArtifactContract{Type: "integer", MaxBytes: 8}, model.ArtifactContract{Type: "number", MaxBytes: 16}) {
		t.Fatal("integer widening rejected")
	}
	c.Properties["extra"] = model.ArtifactContract{Type: "string", MaxBytes: 4}
	if ArtifactAssignable(c, narrow) {
		t.Fatal("object keys widened")
	}
	nested := model.ArtifactContract{Type: "string", MaxBytes: 8}
	for i := 0; i < 10; i++ {
		copy := nested
		nested = model.ArtifactContract{Type: "array", MaxBytes: 64, MaxItems: 1, Items: &copy}
	}
	if model.ValidateArtifactContract(nested) == nil {
		t.Fatal("unbounded contract depth")
	}
}

func TestArtifactContractPrimitiveAndBounds(t *testing.T) {
	for _, tc := range []struct{ kind, good, bad string }{
		{"string", `"ready"`, `true`}, {"boolean", `false`, `"false"`}, {"integer", `9007199254740993`, `9007199254740993.1`}, {"number", `1.5`, `1e999`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			c := model.ArtifactContract{Type: tc.kind, MaxBytes: 128}
			if _, err := ArtifactValue(c, tc.good); err != nil {
				t.Fatal(err)
			}
			if _, err := ArtifactValue(c, tc.bad); err == nil {
				t.Fatal("wrong primitive accepted")
			}
		})
	}
	if _, err := ArtifactValue(model.ArtifactContract{Type: "string", MaxBytes: 4}, `"1234"`); err == nil {
		t.Fatal("byte limit ignored")
	}
	if _, err := ArtifactValue(model.ArtifactContract{Type: "integer", MaxBytes: 64}, `1e-999999999`); err == nil {
		t.Fatal("unbounded integer exponent")
	}
	for _, c := range []model.ArtifactContract{{Type: "json-schema", MaxBytes: 1}, {Type: "string"}, {Type: "array", MaxBytes: 10}, {Type: "number", MaxBytes: 1048577}, {Type: "boolean", MaxBytes: 5, Items: &model.ArtifactContract{Type: "string", MaxBytes: 2}}} {
		if model.ValidateArtifactContract(c) == nil {
			t.Fatalf("invalid contract %+v", c)
		}
	}
}

func TestArtifactContractStrictDecoding(t *testing.T) {
	for _, raw := range []string{`null`, `{"type":"string","max_bytes":8,"items":null}`, `{"type":"object","max_bytes":8,"properties":null}`, `{"type":"string","max_bytes":8,"unknown":true}`, `{"Type":"string","max_bytes":8}`, `{"type":"string","type":"string","max_bytes":8}`, `{"type":"array","max_bytes":64,"max_items":2,"items":{"type":"string","max_bytes":null}}`} {
		var c model.ArtifactContract
		if json.Unmarshal([]byte(raw), &c) == nil {
			t.Errorf("accepted malformed contract %s", raw)
		}
	}
}
