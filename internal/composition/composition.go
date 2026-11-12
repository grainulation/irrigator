package composition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// Version 1 preserves opaque strings; version 2 carries bounded JSON contracts.
type CompositionRef struct {
	Input   string  `json:"input,omitempty"`
	Node    string  `json:"node,omitempty"`
	Call    string  `json:"call,omitempty"`
	Port    string  `json:"port,omitempty"`
	Literal *string `json:"literal,omitempty"`
}
type CompositionPin struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}
type CompositionNode struct {
	ID              string                            `json:"id"`
	Prompt          string                            `json:"prompt"`
	Profile         string                            `json:"profile"`
	Inputs          map[string]CompositionRef         `json:"inputs"`
	Outputs         map[string]string                 `json:"outputs,omitempty"`
	InputContracts  map[string]model.ArtifactContract `json:"input_contracts,omitempty"`
	OutputContracts map[string]model.ArtifactContract `json:"output_contracts,omitempty"`
}
type CompositionTemplate struct {
	Schema         string                            `json:"schema"`
	Version        string                            `json:"version"`
	Inputs         map[string]string                 `json:"inputs,omitempty"`
	InputContracts map[string]model.ArtifactContract `json:"input_contracts,omitempty"`
	Nodes          []CompositionNode                 `json:"nodes"`
	Exports        map[string]CompositionRef         `json:"exports"`
}
type CompositionCall struct {
	ID       string                    `json:"id"`
	Template CompositionPin            `json:"template"`
	Inputs   map[string]CompositionRef `json:"inputs"`
}
type CompositionConfig struct {
	Schema         string                      `json:"schema"`
	Name           string                      `json:"name"`
	Execution      *model.ManagedExecutionPlan `json:"execution"`
	MaxTasks       int                         `json:"max_tasks"`
	MaxInputBytes  int64                       `json:"max_input_bytes"`
	MaxOutputBytes int64                       `json:"max_output_bytes"`
	Calls          []CompositionCall           `json:"calls"`
	Exports        map[string]CompositionRef   `json:"exports"`
}
type compositionOrigin struct {
	Call     string         `json:"call"`
	Template CompositionPin `json:"template"`
	Nodes    []string       `json:"nodes"`
}
type compositionProvenance struct {
	Schema       string                    `json:"schema"`
	ConfigSHA256 string                    `json:"config_sha256"`
	PlanSHA256   string                    `json:"plan_sha256"`
	Origins      []compositionOrigin       `json:"origins"`
	Exports      map[string]CompositionRef `json:"exports"`
}

const compositionFileLimit = 1024 * 1024

func CompositionRead(r io.Reader, dst any) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, compositionFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > compositionFileLimit || !utf8.Valid(raw) {
		return nil, errors.New("composition input exceeds byte/encoding bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	value, err := jsonio.ParseStrictJSON(d, 0)
	if err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing composition JSON")
	}
	if err = launch.ExecutionJSONShape(value, reflect.TypeOf(dst).Elem()); err != nil {
		return nil, err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	err = d.Decode(dst)
	return raw, err
}
func CompositionDigest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func compositionName(s string) bool {
	return launch.ExecutionName.MatchString(s) && !strings.Contains(s, "__")
}
func compositionKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func compositionPorts(m map[string]string) error {
	for k, v := range m {
		if !compositionName(k) || v != "string" {
			return errors.New("ports require simple names and runtime-supported string type")
		}
	}
	return nil
}
func compositionTemplate(root *os.Root, p CompositionPin) (CompositionTemplate, error) {
	var t CompositionTemplate
	if !filepath.IsLocal(p.Path) || p.Path == "." || strings.Contains(p.Path, "\\") || p.Version == "" || len(p.Version) > 128 || len(p.SHA256) != 64 {
		return t, errors.New("invalid local template pin")
	}
	for _, part := range strings.Split(filepath.ToSlash(p.Path), "/") {
		if part == ".." || part == "." || part == "" {
			return t, errors.New("template traversal forbidden")
		}
	}
	f, err := root.OpenFile(p.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return t, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return t, errors.New("template must be regular file")
	}
	raw, err := CompositionRead(f, &t)
	if err != nil {
		return t, err
	}
	if CompositionDigest(raw) != p.SHA256 || t.Version != p.Version || (t.Schema != "irrigator.workflow/v1" && t.Schema != "irrigator.workflow/v2") {
		return t, errors.New("template version/hash/schema mismatch")
	}
	return t, nil
}
func CompileComposition(path string) (model.Plan, compositionProvenance, error) {
	var p model.Plan
	var provenance compositionProvenance
	var c CompositionConfig
	fail := func(err error) (model.Plan, compositionProvenance, error) {
		return model.Plan{}, compositionProvenance{}, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail(err)
	}
	st, statErr := f.Stat()
	if statErr != nil || !st.Mode().IsRegular() {
		f.Close()
		return fail(errors.New("composition must be regular file"))
	}
	raw, err := CompositionRead(f, &c)
	f.Close()
	if err != nil {
		return fail(err)
	}
	if (c.Schema != "irrigator.composition/v1" && c.Schema != "irrigator.composition/v2") || strings.TrimSpace(c.Name) == "" || c.Execution == nil || c.MaxTasks < 1 || c.MaxTasks > 1024 || c.MaxInputBytes < 1 || c.MaxOutputBytes < 1 || len(c.Calls) < 1 || len(c.Calls) > c.MaxTasks {
		return fail(errors.New("composition requires explicit bounded parent authority"))
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fail(err)
	}
	defer root.Close()
	p = model.Plan{APIVersion: launch.UnknownCostPlanVersion, Name: c.Name, Execution: c.Execution}
	typed := c.Schema == "irrigator.composition/v2"
	if typed {
		p.APIVersion = launch.ArtifactPlanVersion
	}
	allowed := map[string]bool{}
	for _, name := range c.Execution.AllowedProfiles {
		allowed[name] = true
	}
	templates := map[string]CompositionTemplate{}
	calls := map[string]CompositionCall{}
	nodes := map[string]CompositionNode{}
	provenance = compositionProvenance{Schema: "irrigator.composition-provenance/v1", ConfigSHA256: CompositionDigest(raw), Exports: map[string]CompositionRef{}}
	templateBytes := 0
	for _, call := range c.Calls {
		if !compositionName(call.ID) || calls[call.ID].ID != "" {
			return fail(errors.New("invalid/duplicate call id"))
		}
		calls[call.ID] = call
		t, e := compositionTemplate(root, call.Template)
		if e != nil {
			return fail(e)
		}
		encoded, _ := json.Marshal(t)
		templateBytes += len(encoded)
		if templateBytes > 8*compositionFileLimit {
			return fail(errors.New("aggregate template byte bound exceeded"))
		}
		if typed != (t.Schema == "irrigator.workflow/v2") {
			return fail(errors.New("composition/template schema mismatch"))
		}
		if typed {
			if len(t.Inputs) > 0 {
				return fail(errors.New("v2 requires input_contracts"))
			}
			t.Inputs = map[string]string{}
			for k, contract := range t.InputContracts {
				if !compositionName(k) {
					return fail(errors.New("invalid input name"))
				}
				if e = model.ValidateArtifactContract(contract); e != nil {
					return fail(e)
				}
				t.Inputs[k] = "string"
			}
			for i := range t.Nodes {
				n := &t.Nodes[i]
				if len(n.Outputs) > 0 || len(n.InputContracts) != len(n.Inputs) {
					return fail(errors.New("v2 requires exact node input/output contracts"))
				}
				n.Outputs = map[string]string{}
				for k, contract := range n.OutputContracts {
					if e = model.ValidateArtifactContract(contract); e != nil {
						return fail(e)
					}
					n.Outputs[k] = "string"
				}
				for k, contract := range n.InputContracts {
					if _, ok := n.Inputs[k]; !ok {
						return fail(errors.New("unknown node input contract"))
					}
					if e = model.ValidateArtifactContract(contract); e != nil {
						return fail(e)
					}
				}
			}
		} else {
			if len(t.InputContracts) > 0 {
				return fail(errors.New("v1 cannot carry typed contracts"))
			}
			for _, n := range t.Nodes {
				if len(n.InputContracts) > 0 || len(n.OutputContracts) > 0 {
					return fail(errors.New("v1 cannot carry typed contracts"))
				}
			}
		}
		templates[call.ID] = t
		if e = compositionPorts(t.Inputs); e != nil {
			return fail(e)
		}
		if len(call.Inputs) != len(t.Inputs) || len(t.Nodes) == 0 {
			return fail(errors.New("template input arity or empty graph"))
		}
		for name := range t.Inputs {
			if _, ok := call.Inputs[name]; !ok {
				return fail(errors.New("missing template input"))
			}
		}
		origin := compositionOrigin{Call: call.ID, Template: call.Template}
		for _, n := range t.Nodes {
			id := call.ID + "__" + n.ID
			if !compositionName(n.ID) || nodes[id].ID != "" || !allowed[n.Profile] {
				return fail(errors.New("node identity/profile exceeds parent authority"))
			}
			if e = compositionPorts(n.Outputs); e != nil {
				return fail(e)
			}
			if len(n.Outputs) == 0 {
				return fail(errors.New("node needs output contract"))
			}
			profile, ok := c.Execution.Catalog.Profiles[n.Profile]
			if !ok || profile.Limits.InputBytes > c.MaxInputBytes || profile.Limits.OutputBytes > c.MaxOutputBytes {
				return fail(errors.New("profile exceeds task byte limits"))
			}
			nodes[id] = n
			origin.Nodes = append(origin.Nodes, id)
		}
		provenance.Origins = append(provenance.Origins, origin)
		if len(nodes) > c.MaxTasks {
			return fail(errors.New("expanded graph exceeds max_tasks"))
		}
	}
	// Resolve exports without executing or interpreting provider output.
	output := func(call string, r CompositionRef) (CompositionRef, error) {
		if r.Node == "" || r.Port == "" || r.Call != "" || r.Input != "" || r.Literal != nil {
			return CompositionRef{}, errors.New("invalid template export reference")
		}
		id := call + "__" + r.Node
		if nodes[id].Outputs[r.Port] != "string" {
			return CompositionRef{}, errors.New("unknown node output port")
		}
		return CompositionRef{Node: id, Port: r.Port}, nil
	}
	exports := map[string]map[string]CompositionRef{}
	for _, callID := range compositionKeys(templates) {
		exports[callID] = map[string]CompositionRef{}
		for name, r := range templates[callID].Exports {
			if !compositionName(name) {
				return fail(errors.New("invalid export name"))
			}
			v, e := output(callID, r)
			if e != nil {
				return fail(e)
			}
			exports[callID][name] = v
		}
	}
	resolveCall := func(r CompositionRef) (CompositionRef, error) {
		if r.Input != "" || r.Node != "" {
			return CompositionRef{}, errors.New("invalid call binding")
		}
		if r.Literal != nil {
			if r.Call != "" || r.Port != "" {
				return CompositionRef{}, errors.New("ambiguous literal binding")
			}
			return r, nil
		}
		if v, ok := exports[r.Call][r.Port]; ok {
			return v, nil
		}
		return CompositionRef{}, errors.New("unknown call output port")
	}
	checkBinding := func(ref CompositionRef, target model.ArtifactContract) error {
		if ref.Literal != nil {
			canonical, e := artifactstore.ArtifactValue(target, *ref.Literal)
			if e != nil {
				return e
			}
			if canonical != *ref.Literal {
				return errors.New("literal must use canonical JSON")
			}
			return nil
		}
		source, ok := nodes[ref.Node].OutputContracts[ref.Port]
		if !ok || !artifactstore.ArtifactAssignable(source, target) {
			return errors.New("incompatible artifact port contracts")
		}
		return nil
	}
	// Declared call edges must be acyclic, even when a template ignores an input.
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return errors.New("composition call cycle")
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, ref := range calls[id].Inputs {
			if ref.Call != "" {
				if _, ok := calls[ref.Call]; !ok {
					return errors.New("unknown call")
				}
				if e := visit(ref.Call); e != nil {
					return e
				}
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for _, id := range compositionKeys(calls) {
		if e := visit(id); e != nil {
			return fail(e)
		}
	}
	for _, call := range c.Calls {
		t := templates[call.ID]
		bindings := map[string]CompositionRef{}
		for name, r := range call.Inputs {
			v, e := resolveCall(r)
			if e != nil {
				return fail(e)
			}
			if typed {
				if e = checkBinding(v, t.InputContracts[name]); e != nil {
					return fail(e)
				}
			}
			bindings[name] = v
		}
		for _, n := range t.Nodes {
			id := call.ID + "__" + n.ID
			task := model.Task{ID: id, Prompt: n.Prompt, Profile: n.Profile, Requires: compositionKeys(n.Outputs), Turn: &model.TurnIdentity{AgentID: id, Role: "main", Index: 1}, InputFields: map[string][]string{}}
			if typed {
				task.ArtifactContracts = n.OutputContracts
			}
			inputMap := map[string]CompositionRef{}
			for _, name := range compositionKeys(n.Inputs) {
				if !compositionName(name) {
					return fail(errors.New("invalid input port name"))
				}
				r := n.Inputs[name]
				var v CompositionRef
				var e error
				if r.Input != "" {
					if r.Node != "" || r.Call != "" || r.Port != "" || r.Literal != nil {
						return fail(errors.New("ambiguous input reference"))
					}
					var ok bool
					v, ok = bindings[r.Input]
					if !ok {
						return fail(errors.New("unknown template input"))
					}
					if typed && !artifactstore.ArtifactAssignable(t.InputContracts[r.Input], n.InputContracts[name]) {
						return fail(errors.New("template input contract incompatible with node input"))
					}
				} else {
					v, e = output(call.ID, r)
					if e != nil {
						return fail(e)
					}
				}
				if typed {
					if e = checkBinding(v, n.InputContracts[name]); e != nil {
						return fail(e)
					}
				}
				inputMap[name] = v
				if v.Node != "" {
					task.InputFields[v.Node] = append(task.InputFields[v.Node], v.Port)
				}
			}
			for source, fields := range task.InputFields {
				sort.Strings(fields)
				unique := fields[:0]
				for _, field := range fields {
					if len(unique) == 0 || unique[len(unique)-1] != field {
						unique = append(unique, field)
					}
				}
				task.InputFields[source] = unique
				task.DependsOn = append(task.DependsOn, source)
			}
			sort.Strings(task.DependsOn)
			if len(inputMap) > 0 {
				b, _ := json.Marshal(inputMap)
				task.Prompt += "\n\nWorkflow input bindings (node/port reference dependency artifacts; literal is supplied data):\n" + string(b)
			}
			if typed {
				contracts, _ := json.Marshal(n.OutputContracts)
				task.Prompt += "\n\nReturn each output as canonical compact JSON text (sorted object keys) inside the string-valued outputs envelope. Required bounded Irrigator contracts: " + string(contracts)
			}
			if strings.TrimSpace(n.Prompt) == "" {
				return fail(errors.New("node needs nonempty prompt"))
			}
			if int64(len(task.Prompt)) > c.MaxInputBytes || int64(len(task.Prompt)) > c.Execution.Catalog.Profiles[n.Profile].Limits.InputBytes {
				return fail(errors.New("compiled prompt exceeds parent input byte limit"))
			}
			p.Tasks = append(p.Tasks, task)
		}
	}
	for name, r := range c.Exports {
		if !compositionName(name) || r.Literal != nil {
			return fail(errors.New("invalid composition export"))
		}
		v, e := resolveCall(r)
		if e != nil {
			return fail(e)
		}
		provenance.Exports[name] = v
	}
	p.MaxTasks = len(p.Tasks)
	p.TurnShape = &model.TurnShape{Roots: len(p.Tasks), MainTurns: 1}
	if err = checkpoint.Validate(p); err != nil {
		return fail(err)
	}
	return p, provenance, nil
}
func CompositionCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("compose", flag.ContinueOnError)
	out := fs.String("out", "", "new plan path (required)")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 1 || *out == "" {
		return 2, errors.New("usage: irrigator compose CONFIG.json --out NEW-PLAN.json")
	}
	p, provenance, err := CompileComposition(fs.Arg(0))
	if err != nil {
		return 1, err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return 1, err
	}
	raw = append(raw, '\n')
	provenance.PlanSHA256 = CompositionDigest(raw)
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 1, err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return 1, err
	}
	if closeErr != nil {
		return 1, closeErr
	}
	receipt, _ := json.MarshalIndent(provenance, "", "  ")
	fmt.Println(string(receipt))
	return 0, nil
}
