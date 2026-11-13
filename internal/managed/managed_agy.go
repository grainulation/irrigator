package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// agyStrictSettings are session-only agy settings: print mode auto-denies every
// tool call ("strict"), and file tools stay inside the temporary workspace.
const agyStrictSettings = `{"toolPermission":"strict","allowNonWorkspaceAccess":false}`

// agyPromptLimit bounds the request passed as the -p argument.
const agyPromptLimit = 128 * 1024

// managedAgyArgs runs one structured, tool-free agy print turn. agy has no
// system-prompt flag: the managed system prompt is the workspace AGENTS.md.
func managedAgyArgs(b model.ExecutionBinding, prompt, schema string, timeout time.Duration) []string {
	return []string{"-p", prompt, "--output-format", "stream-json", "--model", b.Profile.Model, "--json-schema", schema,
		"--settings", agyStrictSettings, "--disable-slash-commands", "--print-timeout", fmt.Sprintf("%ds", int(timeout.Seconds()))}
}

func managedAgyWork(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding, input []byte) (model.WorkResponse, error) {
	response := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID}
	fail := func(certainty string, err error) (model.WorkResponse, error) {
		response.Outcome = certainty
		response.Error = err.Error()
		return response, model.WithOutcome(certainty, err)
	}
	schema, err := json.Marshal(artifactstore.ModelSchemaForRequest(req))
	if err != nil || int64(len(input)+len(schema)) > b.Profile.Limits.InputBytes || len(input) > agyPromptLimit || !utf8.Valid(input) {
		return fail(model.OutcomeNotStarted, errors.New("agy request plus schema exceeds profile or argument limit"))
	}
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	dir, err := os.MkdirTemp(tempRoot, "irrigator-agy-")
	if err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	defer os.RemoveAll(dir)
	if err = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(ManagedSystemPrompt+"\n"), 0600); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	c := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}
	if _, err = launch.ResolveExecutionCatalog(c, "managed-recheck", b.ProfileName); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	// agy's own limit ends the turn cleanly a little before the call deadline.
	timeout := 10 * time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline) - 5*time.Second
	}
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	cmd := exec.Command(b.Adapter.Command[0], managedAgyArgs(b, string(input), string(schema), timeout)...)
	cmd.Dir = dir
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "SystemRoot", "LANG"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	raw, processErr := launch.RunProcess(ctx, cmd, prefix, nil)
	if processErr != nil {
		if model.KnownNotStarted(processErr) || agyRejectedBeforeStart(raw) {
			return fail(model.OutcomeNotStarted, processErr)
		}
		response.CostUnknown = true
		return fail(model.OutcomeUncertain, processErr)
	}
	response.CostUnknown = true // agy reports tokens, never USD
	if int64(len(raw)) > b.Profile.Limits.OutputBytes {
		return fail(model.OutcomeUncertain, errors.New("agy stream exceeds profile output limit"))
	}
	output, usage, err := parseManagedAgyStream(raw, req.Task)
	if err != nil {
		return fail(model.OutcomeUncertain, err)
	}
	response.Output = output
	if usage != nil {
		usage.Model = b.Profile.Model
	}
	response.Usage = usage
	response.Model = b.Profile.Model
	response.Outcome = model.OutcomeSettled
	return response, nil
}

func agyObject(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8 agy event")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	v, err := jsonio.ParseStrictJSON(decoder, 0)
	if err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid or duplicate agy JSON")
	}
	object, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("agy event must be object")
	}
	return object, nil
}

// agyRejectedBeforeStart reports a result that agy emitted without ever
// starting a session (for example an unknown model): nothing ran.
func agyRejectedBeforeStart(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		event, err := agyObject(line)
		if err != nil || event["event"] != "result" {
			continue
		}
		res, _ := event["result"].(map[string]any)
		turns, _ := res["num_turns"].(json.Number)
		return res["status"] != "SUCCESS" && res["conversation_id"] == "" && (turns == "" || turns == "0")
	}
	return false
}

// parseManagedAgyStream accepts only a tool-free turn that ends in one SUCCESS
// result carrying the structured output, with no denied actions.
func parseManagedAgyStream(raw []byte, task model.Task) (map[string]string, *model.TaskUsage, error) {
	var output map[string]string
	var usage *model.TaskUsage
	results, events := 0, 0
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if events++; events > 4096 {
			return nil, nil, errors.New("too many agy events")
		}
		event, err := agyObject(line)
		if err != nil {
			return nil, nil, err
		}
		if results > 0 {
			return nil, nil, errors.New("agy events after the result")
		}
		switch event["event"] {
		case "init":
		case "step_update":
			step, _ := event["step_update"].(map[string]any)
			switch step["step_type"] {
			case "tool", "subagent":
				return nil, nil, errors.New("agy tool or subagent use rejected")
			}
		case "result":
			results++
			res, _ := event["result"].(map[string]any)
			if res["status"] != "SUCCESS" || res["finished_reason"] != nil {
				return nil, nil, fmt.Errorf("agy turn did not succeed (%v)", res["status"])
			}
			if denied, ok := res["denied_actions"].([]any); ok && len(denied) > 0 {
				return nil, nil, errors.New("agy attempted a denied action")
			}
			structured, ok := res["structured_output"].(map[string]any)
			if !ok {
				return nil, nil, errors.New("agy result has no structured output")
			}
			text, err := json.Marshal(structured)
			if err != nil {
				return nil, nil, err
			}
			if output, err = artifactstore.NormalizeModelOutput(task, json.RawMessage(text)); err != nil {
				return nil, nil, fmt.Errorf("agy output contract: %w", err)
			}
			if usage, err = agyUsage(res["usage"]); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, errors.New("unsupported agy stream event")
		}
	}
	if results != 1 {
		return nil, nil, errors.New("agy stream ended without a result")
	}
	return output, usage, nil
}

func agyUsage(value any) (*model.TaskUsage, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("agy success missing usage")
	}
	count := func(key string) (int64, error) {
		v, present := object[key]
		if !present {
			return 0, nil
		}
		n, ok := v.(json.Number)
		if !ok {
			return 0, errors.New("invalid agy usage count")
		}
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, errors.New("invalid agy usage count")
		}
		return i, nil
	}
	u := &model.TaskUsage{}
	var err error
	for key, dst := range map[string]*int64{"input_tokens": &u.InputTokens, "output_tokens": &u.OutputTokens,
		"thinking_tokens": &u.ThinkingTokens, "cache_read_tokens": &u.CacheReadTokens, "total_tokens": &u.TotalTokens} {
		if *dst, err = count(key); err != nil {
			return nil, err
		}
	}
	if _, ok := object["input_tokens"]; !ok {
		return nil, errors.New("agy success missing usage")
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens + u.ThinkingTokens + u.CacheReadTokens
	}
	return u, nil
}
