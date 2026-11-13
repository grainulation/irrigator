package managed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

const agyInit = `{"event":"init","conversation_id":"c1","init":{"model":"m","cwd":"/tmp/x","tools":["finish"],"permission_mode":"strict"}}`
const agyFinish = `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"DONE","step_type":"finish"}}`

func agyResult(extra string) string {
	return `{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"{\"answer\":\"ready\"}","num_turns":1,` +
		`"structured_output":{"answer":"ready"},"usage":{"input_tokens":8209,"output_tokens":14,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":8223}` + extra + `}}`
}

func TestParseManagedAgyStream(t *testing.T) {
	task := model.Task{ID: "ready", Requires: []string{"answer"}}
	out, usage, err := parseManagedAgyStream([]byte(agyInit+"\n"+agyFinish+"\n"+agyResult("")+"\n"), task)
	if err != nil || out["answer"] != "ready" || usage == nil || usage.InputTokens != 8209 || usage.TotalTokens != 8223 {
		t.Fatalf("success: %v %+v %v", out, usage, err)
	}
	tool := `{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"run_command"}}`
	sub := `{"event":"step_update","step_update":{"step_type":"subagent","state":"DONE","tool_name":"invoke_subagent"}}`
	for name, stream := range map[string]string{
		"tool step":        agyInit + "\n" + tool + "\n" + agyResult(""),
		"subagent":         agyInit + "\n" + sub + "\n" + agyResult(""),
		"denied action":    agyInit + "\n" + agyResult(`,"denied_actions":[{"action":"command"}]`),
		"timeout":          agyInit + "\n" + strings.Replace(agyResult(`,"finished_reason":"print_timeout"`), `"SUCCESS"`, `"ERROR"`, 1),
		"no result":        agyInit + "\n" + agyFinish,
		"after result":     agyInit + "\n" + agyResult("") + "\n" + agyFinish,
		"no structured":    agyInit + "\n" + strings.Replace(agyResult(""), `"structured_output":{"answer":"ready"},`, "", 1),
		"wrong field":      agyInit + "\n" + strings.Replace(agyResult(""), `"structured_output":{"answer":"ready"}`, `"structured_output":{"other":"x"}`, 1),
		"duplicate key":    `{"event":"init","event":"init"}`,
		"unknown event":    agyInit + "\n" + `{"event":"surprise"}` + "\n" + agyResult(""),
		"negative tokens":  agyInit + "\n" + strings.Replace(agyResult(""), `"input_tokens":8209`, `"input_tokens":-1`, 1),
		"usage not object": agyInit + "\n" + strings.Replace(agyResult(""), `"usage":{"input_tokens":8209,"output_tokens":14,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":8223}`, `"usage":3`, 1),
	} {
		if _, _, err := parseManagedAgyStream([]byte(stream), task); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	rejected := `{"event":"result","result":{"conversation_id":"","status":"ERROR","response":"","error":"invalid model selection","num_turns":0,"usage":{"input_tokens":0},"finished_reason":"error"}}`
	if !agyRejectedBeforeStart([]byte(rejected)) || agyRejectedBeforeStart([]byte(agyResult(""))) {
		t.Fatal("before-start rejection detection")
	}
	args := managedAgyArgs(model.ExecutionBinding{Profile: model.ExecutionProfile{Model: "gemini-3.8-flash-low"}}, "PROMPT", "{}", 0)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--json-schema {}", `"toolPermission":"strict"`, "--model gemini-3.8-flash-low", "--output-format stream-json", "--disable-slash-commands"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args lack %q: %s", want, joined)
		}
	}
}

func TestManagedInitialPlanForAgy(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "agy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	p, err := managedInitialPlan("agy:gemini-3.8-flash-low")
	if err != nil {
		t.Fatal(err)
	}
	a := p.Execution.Catalog.Adapters["agy"]
	ref := p.Execution.Catalog.Instances["local"].CredentialRefs[0]
	if a.Kind != "agy_cli" || ref.Kind != "host_session" || ref.Name != "agy" || p.Execution.Catalog.Profiles["default"].Model != "gemini-3.8-flash-low" {
		t.Fatalf("plan %+v", p.Execution.Catalog)
	}
	if _, err := managedInitialPlan("gemini:x"); err == nil || !strings.Contains(err.Error(), "agy:MODEL") {
		t.Fatalf("unknown provider: %v", err)
	}
}
