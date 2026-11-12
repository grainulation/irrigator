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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// codexTokenUsage is provider-reported token accounting, never a USD estimate.
type codexTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

func (c *codexTokenUsage) toTaskUsage(modelName string) *model.TaskUsage {
	if c == nil {
		return nil
	}
	tot := c.InputTokens + c.OutputTokens + c.ReasoningOutputTokens + c.CachedInputTokens + c.CacheWriteInputTokens
	return &model.TaskUsage{
		InputTokens:      c.InputTokens,
		OutputTokens:     c.OutputTokens,
		ThinkingTokens:   c.ReasoningOutputTokens,
		CacheReadTokens:  c.CachedInputTokens,
		CacheWriteTokens: c.CacheWriteInputTokens,
		TotalTokens:      tot,
		Model:            modelName,
	}
}

var codexDisabledFeatures = []string{"shell_tool", "apps", "plugins", "hooks", "multi_agent", "multi_agent_v2", "browser_use", "browser_use_external", "computer_use", "image_generation", "view_image", "code_mode", "code_mode_host", "goals", "sleep_tool", "skill_search", "skill_mcp_dependency_install", "memories", "workspace_dependencies", "remote_plugin", "tool_suggest", "shell_snapshot", "unbounded_connection_retries"}

// Same isolation controls exercised against the actual 0.163 CLI in the free
// owned-loopback fixture. No custom provider, auth key, or model fallback here.
func managedCodexArgs(b model.ExecutionBinding, dir, schema string) []string {
	args := []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--sandbox", "read-only", "--skip-git-repo-check", "--cd", dir, "--model", b.Profile.Model, "--output-schema", schema, "--json", "--color", "never"}
	config := []string{`model_reasoning_effort="low"`, `approval_policy="never"`, `web_search="disabled"`, `project_doc_max_bytes=0`, `features.skip_host_skill_discovery=true`, "projects." + strconv.Quote(dir) + `.trust_level="untrusted"`}
	for _, f := range codexDisabledFeatures {
		config = append(config, "features."+f+"=false")
	}
	for _, c := range config {
		args = append(args, "-c", c)
	}
	return append(args, "-")
}

func managedCodexWork(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding, input []byte) (model.WorkResponse, error) {
	response := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID}
	fail := func(certainty string, err error) (model.WorkResponse, error) {
		response.Outcome = certainty
		response.Error = err.Error()
		return response, model.WithOutcome(certainty, err)
	}
	schema, err := json.Marshal(artifactstore.ModelSchemaForRequest(req))
	if err != nil || int64(len(input)+len(schema)) > b.Profile.Limits.InputBytes {
		return fail(model.OutcomeNotStarted, errors.New("Codex request plus schema exceeds profile limit"))
	}
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	dir, err := os.MkdirTemp(tempRoot, "irrigator-codex-")
	if err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	defer os.RemoveAll(dir)
	schemaPath := filepath.Join(dir, "output-schema.json")
	if err = os.WriteFile(schemaPath, schema, 0600); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	c := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}
	if _, err = launch.ResolveExecutionCatalog(c, "managed-recheck", b.ProfileName); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	cmd := exec.Command(b.Adapter.Command[0], managedCodexArgs(b, dir, schemaPath)...)
	cmd.Dir = dir
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "SystemRoot", "LANG", "CODEX_HOME"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	raw, processErr := launch.RunProcess(ctx, cmd, prefix, input)
	if processErr != nil {
		if model.KnownNotStarted(processErr) {
			return fail(model.OutcomeNotStarted, processErr)
		}
		response.CostUnknown = true
		return fail(model.OutcomeUncertain, processErr)
	}
	response.CostUnknown = true
	if int64(len(raw)) > b.Profile.Limits.OutputBytes {
		return fail(model.OutcomeUncertain, errors.New("Codex stream exceeds profile output limit"))
	}
	output, usage, err := ParseManagedCodexStream(raw, req.Task)
	if err != nil {
		return fail(model.OutcomeUncertain, err)
	}
	response.Output = output
	response.Usage = usage.toTaskUsage(b.Profile.Model)
	response.Model = b.Profile.Model
	response.Outcome = model.OutcomeSettled
	return response, nil
}

func codexObject(raw []byte, keys ...string) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8 Codex event")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	v, err := jsonio.ParseStrictJSON(decoder, 0)
	if err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid or duplicate Codex JSON")
	}
	object, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("Codex event must be object")
	}
	for key := range object {
		for _, canonical := range keys {
			if strings.EqualFold(key, canonical) && key != canonical {
				return nil, errors.New("noncanonical Codex field alias")
			}
		}
	}
	return object, nil
}

func codexUsage(value any) (*codexTokenUsage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	keys := []string{"input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens", "reasoning_output_tokens"}
	object, err := codexObject(raw, keys...)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if _, ok := object[key]; !ok {
			return nil, errors.New("Codex success missing usage")
		}
	}
	for key, value := range object {
		known := false
		for _, k := range keys {
			if key == k {
				known = true
			}
		}
		if !known {
			return nil, errors.New("unsupported Codex usage field")
		}
		if value == nil {
			return nil, errors.New("null Codex usage")
		}
	}
	var usage codexTokenUsage
	if json.Unmarshal(raw, &usage) != nil || usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.CacheWriteInputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningOutputTokens < 0 {
		return nil, errors.New("invalid Codex usage counts")
	}
	return &usage, nil
}

func ParseManagedCodexStream(raw []byte, task model.Task) (map[string]string, *codexTokenUsage, error) {
	var output map[string]string
	var usage *codexTokenUsage
	thread, started, completed, finals := false, false, false, 0
	events := 0
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		events++
		if events > 4096 {
			return nil, nil, errors.New("too many Codex events")
		}
		event, err := codexObject(line, "type", "item", "usage", "thread_id")
		if err != nil {
			return nil, nil, err
		}
		if completed {
			return nil, nil, errors.New("Codex events after terminal success")
		}
		kind, ok := event["type"].(string)
		if !ok {
			return nil, nil, errors.New("Codex event missing type")
		}
		switch kind {
		case "thread.started":
			id, ok := event["thread_id"].(string)
			if thread || started || !ok || !launch.ContextText(id, 128) {
				return nil, nil, errors.New("invalid Codex thread start")
			}
			thread = true
		case "turn.started":
			if !thread || started {
				return nil, nil, errors.New("invalid Codex turn start")
			}
			started = true
		case "item.completed":
			itemRaw, _ := json.Marshal(event["item"])
			item, err := codexObject(itemRaw, "type", "text", "message", "id")
			if err != nil {
				return nil, nil, err
			}
			itemType, _ := item["type"].(string)
			if itemType == "error" && !started && kind == "item.completed" {
				message, _ := item["message"].(string)
				if strings.HasPrefix(message, "Under-development features enabled: skip_host_skill_discovery.") || message == "Code Mode is unavailable because code-mode host is disabled. Code mode will fail closed; enable `features.code_mode_host` and install `codex-code-mode-host`." {
					continue
				}
			}
			if !started {
				return nil, nil, errors.New("unexpected Codex item before turn")
			}
			if itemType != "agent_message" && itemType != "reasoning" {
				return nil, nil, errors.New("Codex tool or unsupported item rejected")
			}
			if itemType == "agent_message" && kind == "item.completed" {
				finals++
				if finals != 1 {
					return nil, nil, errors.New("multiple Codex final messages")
				}
				text, ok := item["text"].(string)
				if !ok {
					return nil, nil, errors.New("missing Codex output text")
				}
				if _, err = codexObject([]byte(text)); err != nil {
					return nil, nil, err
				}
				output, err = artifactstore.NormalizeModelOutput(task, json.RawMessage(text))
				if err != nil {
					return nil, nil, fmt.Errorf("Codex output contract: %w", err)
				}
			}
		case "turn.completed":
			if !started || finals != 1 {
				return nil, nil, errors.New("Codex completion without one final result")
			}
			usage, err = codexUsage(event["usage"])
			if err != nil {
				return nil, nil, err
			}
			completed = true
		default:
			return nil, nil, errors.New("Codex failed or unsupported stream event")
		}
	}
	if !completed {
		return nil, nil, errors.New("Codex stream ended without success")
	}
	return output, usage, nil
}
