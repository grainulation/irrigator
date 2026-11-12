package runops

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Adversarial contract tests for deterministic format normalization of a spawn
// owner's proposal. They exercise only the public expansion path already used by
// graph_test.go: expansion, commitExpansion, validateOwnership, retryNode and
// resolveNode. They reuse the fixture, task, read, update, spawnTask and
// stageExpansion helpers from the existing test files and guess no new helper API.
//
// Contract under test (see docs/child-normalization.md):
//   - A bare JSON array of tasks is deterministically equivalent to {"tasks":[...]}.
//   - Normalization never relaxes any bound: max_children, max_depth, max_tasks,
//     provider, effort, context, spawn budget, validators, output paths, unknown
//     fields, trailing data and sibling/dependency rules.
//   - The committed ExpansionHash stays sha256 of the RAW stored source.
//   - Lower child effort is allowed, higher or unknown effort is not, and the
//     provider capability never changes.
//   - Normalization never mutates stored results and never replays or retries an
//     uncertain provider operation.

func cnChild(id, extra string) string {
	return fmt.Sprintf(`{"id":%q,"prompt":"p","requires":["answer"]%s}`, id, extra)
}
func cnObject(c ...string) string { return `{"tasks":[` + strings.Join(c, ",") + `]}` }
func cnArray(c ...string) string  { return `[` + strings.Join(c, ",") + `]` }
func cnSum(src string) string     { return fmt.Sprintf("%x", sha256.Sum256([]byte(src))) }

// cnSource stages a produced owner whose stored graph result is exactly src.
func cnSource(t *testing.T, src string) (string, *model.State, *model.TaskState) {
	t.Helper()
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0)))
	runfixture.StageExpansion(t, root, "parent")
	s := runfixture.Read(t, root)
	r := s.Tasks["parent"]
	r.Result = map[string]string{"graph": src}
	return root, s, r
}
func cnMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func cnSnapshot(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func cnSetSource(t *testing.T, root, src string) {
	t.Helper()
	if err := checkpoint.Update(root, func(s *model.State) error {
		r := s.Tasks["parent"]
		r.Status = "produced"
		r.Result = map[string]string{"graph": src}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChildNormalizationBareArrayEquivalentToObject(t *testing.T) {
	kids := []string{cnChild("a", ""), cnChild("b", `,"depends_on":["a"],"outputs":{"answer":"out.txt"}`)}
	_, s, r := cnSource(t, cnObject(kids...))
	want, _, err := graph.Expansion(s, r)
	if err != nil || len(want) != 2 {
		t.Fatalf("canonical object rejected: %v %d", err, len(want))
	}
	if want[1].DependsOn[0] != "parent__a" || want[1].Outputs["answer"] != filepath.Join("parent", "b", "out.txt") {
		t.Fatalf("canonical namespace wrong: %+v", want[1])
	}
	variants := []string{
		cnArray(kids...),
		"\n\t " + cnArray(kids...) + " \r\n",
		"[ " + kids[0] + " ,\n" + kids[1] + " ]",
	}
	for i, src := range variants {
		_, s, r := cnSource(t, src)
		got, hash, err := graph.Expansion(s, r)
		if err != nil {
			t.Fatalf("variant %d rejected: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("variant %d normalized differently:\n%+v\n%+v", i, got, want)
		}
		if hash != cnSum(src) {
			t.Fatalf("variant %d hash is not the raw source hash", i)
		}
		if r.Result["graph"] != src {
			t.Fatalf("variant %d mutated stored result", i)
		}
		again, hash2, err := graph.Expansion(s, r)
		if err != nil || hash2 != hash || !reflect.DeepEqual(again, got) {
			t.Fatalf("variant %d nondeterministic", i)
		}
	}
	for _, src := range []string{`[]`, `{"tasks":[]}`, " [ ] "} {
		_, s, r := cnSource(t, src)
		got, hash, err := graph.Expansion(s, r)
		if err != nil || len(got) != 0 || hash != cnSum(src) {
			t.Fatalf("empty proposal %q: %v %d", src, err, len(got))
		}
	}
}

func TestChildNormalizationCommitKeepsRawHashAndOperationIdentity(t *testing.T) {
	src := "\n" + cnArray(cnChild("a", ""), cnChild("b", `,"depends_on":["a"]`)) + "\n"
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0)))
	runfixture.StageExpansion(t, root, "parent")
	cnSetSource(t, root, src)
	before := *runfixture.Read(t, root).Tasks["parent"]
	for i := 0; i < 2; i++ {
		if err := commitExpansion(root, "parent", "structural"); err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	p := s.Tasks["parent"]
	if p.ExpansionHash != cnSum(src) || p.Result["graph"] != src {
		t.Fatal("commit rewrote the stored source or hashed a normalized form")
	}
	if p.Attempt != before.Attempt || p.Generation != before.Generation || p.OperationID != before.OperationID {
		t.Fatal("normalization changed the owner's logical operation or attempt")
	}
	if p.Status != "waiting" || len(p.Children) != 2 || p.Children[0] != "parent__a" || p.Children[1] != "parent__b" {
		t.Fatalf("wrong committed graph: %s %v", p.Status, p.Children)
	}
	for _, id := range p.Children {
		c := s.Tasks[id]
		if c.Status != "pending" || c.Attempt != 0 || c.Generation != 1 {
			t.Fatalf("child %s not a fresh unstarted node: %+v", id, c)
		}
	}
	expanded := 0
	for _, e := range s.Events {
		if e.Kind == "expanded" && e.Task == "parent" {
			expanded++
		}
	}
	if expanded != 1 {
		t.Fatalf("idempotent commit emitted %d expanded events", expanded)
	}
	if err := checkpoint.ValidateOwnership(s); err != nil {
		t.Fatalf("committed normalized graph fails ownership validation: %v", err)
	}
	// Rewriting the source into its canonical object form is still a changed source.
	p.Result["graph"] = cnObject(cnChild("a", ""), cnChild("b", `,"depends_on":["a"]`))
	if _, _, err := graph.Expansion(s, p); err == nil {
		t.Fatal("canonical rewrite of a committed source was accepted")
	}
	if err := checkpoint.ValidateOwnership(s); err == nil {
		t.Fatal("ownership accepted a rewritten committed source")
	}
}

type cnCase struct {
	name   string
	kids   []string
	mutate func(*model.State, *model.TaskState)
}

func cnEffort(e string) func(*model.State, *model.TaskState) {
	return func(_ *model.State, r *model.TaskState) { r.Spec.Effort = e }
}

func TestChildNormalizationNeverRelaxesSafetyBounds(t *testing.T) {
	good := cnChild("a", "")
	cases := []cnCase{
		{"provider escalation", []string{cnChild("x", `,"provider":"haiku"`)}, nil},
		{"unknown provider", []string{cnChild("x", `,"provider":"cn-other-provider"`)}, nil},
		{"valid sibling then provider escalation", []string{good, cnChild("b", `,"provider":"cn-other-provider"`)}, nil},
		{"higher effort", []string{cnChild("x", `,"effort":"high"`)}, cnEffort("medium")},
		{"unknown effort", []string{cnChild("x", `,"effort":"turbo"`)}, cnEffort("high")},
		{"case variant effort", []string{cnChild("x", `,"effort":"HIGH"`)}, cnEffort("high")},
		{"command validator", []string{cnChild("x", `,"checks":[{"kind":"command","command":["bad"]}]`)}, nil},
		{"ungranted context", []string{cnChild("x", `,"context":["secret"]`)}, nil},
		{"spawn budget raise", []string{cnChild("x", `,"spawn":{"field":"graph","max_children":99}`)}, nil},
		{"parent traversal output", []string{cnChild("x", `,"outputs":{"answer":"../escape"}`)}, nil},
		{"nested traversal output", []string{cnChild("x", `,"outputs":{"answer":"a/../../escape"}`)}, nil},
		{"absolute output", []string{cnChild("x", `,"outputs":{"answer":"/tmp/escape"}`)}, nil},
		{"dot output", []string{cnChild("x", `,"outputs":{"answer":"."}`)}, nil},
		{"unknown field", []string{cnChild("x", `,"bogus":true`)}, nil},
		{"duplicate ids", []string{good, good}, nil},
		{"namespace separator id", []string{cnChild("x__y", "")}, nil},
		{"empty id", []string{cnChild("", "")}, nil},
		{"unknown dependency", []string{cnChild("x", `,"depends_on":["nope"]`)}, nil},
		{"owner as dependency", []string{cnChild("x", `,"depends_on":["parent"]`)}, nil},
		{"self dependency", []string{cnChild("x", `,"depends_on":["x"]`)}, nil},
		{"sibling cycle", []string{cnChild("x", `,"depends_on":["y"]`), cnChild("y", `,"depends_on":["x"]`)}, nil},
		{"duplicate input source", []string{good, cnChild("b", `,"input_fields":{"a":["answer"],"parent__a":["answer"]}`)}, nil},
		{"exceeds max children", []string{cnChild("a", ""), cnChild("b", ""), cnChild("c", ""), cnChild("d", "")}, nil},
		{"exceeds task budget", []string{good}, func(s *model.State, _ *model.TaskState) { s.Plan.MaxTasks = 1 }},
	}
	shapes := []struct {
		name string
		wrap func(...string) string
		low  bool
	}{{"object", cnObject, false}, {"array", cnArray, false}, {"array-lower-effort", cnArray, true}}
	for _, c := range cases {
		for _, shape := range shapes {
			t.Run(c.name+"/"+shape.name, func(t *testing.T) {
				kids := append([]string{}, c.kids...)
				if shape.low {
					for i, k := range kids {
						if strings.Contains(k, `"effort"`) {
							t.Skip("case already controls effort")
						}
						// Lower effort must not buy any relaxation of another bound.
						kids[i] = strings.Replace(k, `"requires":["answer"]`, `"requires":["answer"],"effort":"low"`, 1)
					}
				}
				_, s, r := cnSource(t, shape.wrap(kids...))
				if shape.low {
					r.Spec.Effort = "high"
				}
				if c.mutate != nil {
					c.mutate(s, r)
				}
				before := cnMarshal(t, s)
				tasks, hash, err := graph.Expansion(s, r)
				if err == nil {
					t.Fatal("accepted unsafe proposal")
				}
				if len(tasks) != 0 || hash != "" {
					t.Fatalf("rejected proposal leaked partial graph: %d %q", len(tasks), hash)
				}
				if cnMarshal(t, s) != before {
					t.Fatal("rejected proposal mutated state")
				}
			})
		}
	}
}

func TestChildNormalizationRejectsMalformedAndAmbiguousSources(t *testing.T) {
	good := cnChild("a", "")
	arr, obj := cnArray(good), cnObject(good)
	doubleEncoded, _ := json.Marshal(arr)
	doubleEncoded, _ = json.Marshal(string(doubleEncoded))
	sources := map[string]string{
		"array then empty array":  arr + " []",
		"array then object":       arr + obj,
		"object then array":       obj + arr,
		"two arrays":              arr + arr,
		"array trailing comma":    arr + ",",
		"array trailing garbage":  arr + " garbage",
		"array trailing NUL":      arr + "\x00",
		"prose prefix":            "Here is the plan: " + arr,
		"prose suffix":            arr + "\nHope this helps",
		"truncated array":         arr[:len(arr)-1],
		"empty":                   "",
		"blank":                   " \n\t ",
		"null":                    "null",
		"bare true":               "true",
		"tasks as object":         `{"tasks":` + good + `}`,
		"scalar element":          "[1]",
		"null element":            "[null]",
		"string element":          `["x"]`,
		"array of arrays":         "[" + arr + "]",
		"wrapper inside array":    "[" + obj + "]",
		"double encoded string":   string(doubleEncoded),
		"pathological nesting":    strings.Repeat("[", 100000),
		"pathological close-less": strings.Repeat(`{"tasks":`, 5000),
	}
	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			_, s, r := cnSource(t, src)
			before := cnMarshal(t, s)
			tasks, hash, err := graph.Expansion(s, r)
			if err == nil || len(tasks) != 0 || hash != "" {
				t.Fatalf("accepted ambiguous source: %v %d %q", err, len(tasks), hash)
			}
			if cnMarshal(t, s) != before {
				t.Fatal("rejected source mutated state")
			}
		})
	}
}

// Wrapper forms the production normalizer may choose to accept or to refuse. The
// property is closed either way: an accepted form is identical to the canonical
// graph and hashes its raw bytes; one decoding level at most.
func TestChildNormalizationOptionalWrappersAreEquivalentOrRefused(t *testing.T) {
	arr := cnArray(cnChild("a", ""))
	_, s, r := cnSource(t, cnObject(cnChild("a", "")))
	want, _, err := graph.Expansion(s, r)
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(arr)
	quotedObject, _ := json.Marshal(cnObject(cnChild("a", "")))
	forms := map[string]string{
		"markdown fence":        "```json\n" + arr + "\n```",
		"bare fence":            "```\n" + arr + "\n```",
		"json string of array":  string(quoted),
		"json string of object": string(quotedObject),
	}
	for name, src := range forms {
		t.Run(name, func(t *testing.T) {
			_, s, r := cnSource(t, src)
			got, hash, err := graph.Expansion(s, r)
			if err != nil {
				return
			}
			if !reflect.DeepEqual(got, want) || hash != cnSum(src) || r.Result["graph"] != src {
				t.Fatal("accepted wrapper is not equivalent to the canonical graph over the raw hash")
			}
		})
	}
}

func TestChildNormalizationBoundsApplyToBareArrays(t *testing.T) {
	many := func(n int) string {
		kids := make([]string, n)
		for i := range kids {
			kids[i] = cnChild(fmt.Sprintf("k%d", i), "")
		}
		return cnArray(kids...)
	}
	t.Run("exactly max children accepted", func(t *testing.T) {
		_, s, r := cnSource(t, many(3))
		if got, _, err := graph.Expansion(s, r); err != nil || len(got) != 3 {
			t.Fatalf("boundary rejected: %v %d", err, len(got))
		}
	})
	t.Run("default max children", func(t *testing.T) {
		_, s, r := cnSource(t, many(9))
		r.Spec.Spawn.MaxChildren = 0
		if _, _, err := graph.Expansion(s, r); err == nil {
			t.Fatal("nine children passed the default cap of eight")
		}
	})
	t.Run("huge array is bounded and fast", func(t *testing.T) {
		_, s, r := cnSource(t, many(20000))
		start := time.Now()
		got, _, err := graph.Expansion(s, r)
		if err == nil || len(got) != 0 {
			t.Fatal("20000-child array accepted")
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("oversized array took unbounded time")
		}
	})
	t.Run("nested bare array honors depth", func(t *testing.T) {
		root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.SpawnTask(t, "branch", runfixture.Task("leaf", 0))))
		runfixture.StageExpansion(t, root, "parent")
		if err := commitExpansion(root, "parent", "structural"); err != nil {
			t.Fatal(err)
		}
		runfixture.StageExpansion(t, root, "parent__branch")
		s := runfixture.Read(t, root)
		r := s.Tasks["parent__branch"]
		r.Result = map[string]string{"graph": cnArray(cnChild("leaf2", ""))}
		if _, _, err := graph.Expansion(s, r); err != nil {
			t.Fatalf("default depth rejected a legal nested bare array: %v", err)
		}
		s.Plan.MaxDepth = 1
		if _, _, err := graph.Expansion(s, r); err == nil {
			t.Fatal("bare array bypassed max_depth")
		}
	})
}

func TestChildNormalizationLowerEffortAllowedCapabilityUnchanged(t *testing.T) {
	for _, shape := range []func(...string) string{cnObject, cnArray} {
		_, s, r := cnSource(t, "")
		effective := checkpoint.ProviderName(s, r.Spec)
		src := shape(
			cnChild("low", `,"effort":"low"`),
			cnChild("mid", `,"effort":"medium"`),
			cnChild("same", `,"effort":"high"`),
			cnChild("inherit", ""),
			cnChild("provider", fmt.Sprintf(`,"provider":%q`, effective)),
		)
		r.Result = map[string]string{"graph": src}
		r.Spec.Effort = "high"
		r.Spec.Spawn.MaxChildren = 5
		got, _, err := graph.Expansion(s, r)
		if err != nil {
			t.Fatalf("lower or equal effort rejected: %v", err)
		}
		want := map[string]string{"parent__low": "low", "parent__mid": "medium", "parent__same": "high", "parent__inherit": "high", "parent__provider": "high"}
		for _, c := range got {
			if c.Effort != want[c.ID] {
				t.Fatalf("%s effort %q, want %q (lowered effort must not be raised back)", c.ID, c.Effort, want[c.ID])
			}
			if c.Provider != effective {
				t.Fatalf("%s provider %q changed capability from %q", c.ID, c.Provider, effective)
			}
			if c.Parent != "parent" || len(c.Checks) != 0 {
				t.Fatalf("%s gained ownership or validators: %+v", c.ID, c)
			}
		}
		if len(got) != 5 {
			t.Fatalf("expected 5 children, got %d", len(got))
		}
	}
	// An owner at a lower effort can never be exceeded by a child.
	_, s, r := cnSource(t, cnArray(cnChild("x", `,"effort":"high"`)))
	r.Spec.Effort = "low"
	if _, _, err := graph.Expansion(s, r); err == nil {
		t.Fatal("child exceeded owner effort")
	}
}

func TestChildNormalizationAtomicRejectionLeavesNoState(t *testing.T) {
	bad := cnArray(cnChild("a", ""), cnChild("b", `,"provider":"cn-other-provider"`))
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0)))
	runfixture.StageExpansion(t, root, "parent")
	cnSetSource(t, root, bad)
	before := cnSnapshot(t, root)
	if err := commitExpansion(root, "parent", "structural"); err == nil {
		t.Fatal("committed a graph containing an unsafe sibling")
	}
	if cnSnapshot(t, root) != before {
		t.Fatal("failed commit changed the checkpoint")
	}
	for _, id := range []string{"parent__a", "parent__b"} {
		if _, err := os.Stat(filepath.Join(root, "cells", id)); err == nil {
			t.Fatalf("cell directory for unaccepted child %s exists", id)
		}
	}
}

func cnAssertOnlyEmptyChildCells(t *testing.T, root string) {
	t.Helper()
	for _, id := range []string{"parent__a"} {
		entries, err := os.ReadDir(filepath.Join(root, "cells", id))
		if err == nil && len(entries) != 0 {
			t.Fatalf("child cell %s holds work artifacts without a committed node", id)
		}
	}
}

func TestChildNormalizationNeverMutatesOrReplaysUncertainProvider(t *testing.T) {
	src := cnArray(cnChild("a", ""))
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0)))
	if err := checkpoint.Update(root, func(s *model.State) error {
		r := s.Tasks["parent"]
		r.Status, r.Attempt = "uncertain", 1
		r.Error = "provider outcome uncertain (malformed_response); reconcile external outcome before explicit replay"
		r.Result = map[string]string{"graph": src}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := cnSnapshot(t, root)

	// The pure expansion path must not touch an uncertain row.
	s := runfixture.Read(t, root)
	mem := cnMarshal(t, s)
	_, _, _ = graph.Expansion(s, s.Tasks["parent"])
	if cnMarshal(t, s) != mem {
		t.Fatal("expansion mutated an uncertain owner or the graph")
	}

	// Committing an uncertain owner is refused and registers nothing.
	if err := commitExpansion(root, "parent", "structural"); err == nil {
		t.Fatal("committed an expansion for an uncertain provider outcome")
	}
	cnAssertOnlyEmptyChildCells(t, root)
	if cnSnapshot(t, root) != before {
		t.Fatal("refused commit changed the checkpoint")
	}

	// Neither plain nor recursive retry may replay it without explicit authorization.
	for _, recursive := range []bool{false, true} {
		if err := retryNode(root, "parent", recursive, false); err == nil {
			t.Fatalf("implicit retry (recursive=%v) of an uncertain operation", recursive)
		}
		if cnSnapshot(t, root) != before {
			t.Fatalf("refused retry (recursive=%v) changed the checkpoint", recursive)
		}
	}
}

// The supported reconcile path may use a normalizable stored source, but it must
// continue the SAME operation: no new generation, attempt or operation id.
func TestChildNormalizationReconcileContinuesSameOperation(t *testing.T) {
	src := cnArray(cnChild("a", ""))
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0)))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["parent"].Status, s.Tasks["parent"].Attempt = "uncertain", 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := *runfixture.Read(t, root).Tasks["parent"]
	if err := ResolveNode(root, "parent", map[string]string{"graph": src}); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["parent"].Status == "produced" {
		// Controller is down: the saved outcome is validated on resume.
		if err := commitExpansion(root, "parent", "structural"); err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	p := s.Tasks["parent"]
	if p.Attempt != before.Attempt || p.Generation != before.Generation || p.OperationID != before.OperationID {
		t.Fatalf("reconcile replayed the provider: %+v", p)
	}
	if p.ExpansionHash != cnSum(src) || len(p.Children) != 1 || p.Status != "waiting" {
		t.Fatalf("reconciled bare array not committed over raw hash: %+v", p)
	}
	if err := checkpoint.ValidateOwnership(s); err != nil {
		t.Fatal(err)
	}
}

// Native fixture correction: expansion assigns ownership from the actual parent.
func TestChildNormalizationParentFieldCannotOverrideOwnership(t *testing.T) {
	for _, source := range []string{cnObject(cnChild("x", `,"parent":"other"`)), cnArray(cnChild("x", `,"parent":"other"`))} {
		_, s, r := cnSource(t, source)
		children, _, err := graph.Expansion(s, r)
		if err != nil || len(children) != 1 || children[0].Parent != r.Spec.ID {
			t.Fatalf("ownership changed: %v %v", children, err)
		}
	}
}
