package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectDefinitionTest(t *testing.T, cfg McpConfig) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, b := mcp.NewInMemoryTransports()
	ss, err := NewMCPServer(cfg).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "definition-test", Version: "0"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMCPExactOutputRetainsSchemaValidation(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		server := NewMCPServer(cfg)
		addMCPTool(server, cfg, "run_status", func(_ context.Context, _ *mcp.CallToolRequest, in mcpRunInput) (*mcp.CallToolResult, mcpOut, error) {
			out := mcpOut{"path": "fixture", "backend": "mock", "control": "run", "executor": "local", "graph_revision": json.Number("9007199254740993"), "verified": 0, "total": 0, "cost": mcpOut{}, "cells": []any{}}
			switch in.Run {
			case "missing-field":
				delete(out, "cells")
			case "wrong-type":
				out["graph_revision"] = "not-a-number"
			case "fractional":
				out["graph_revision"] = json.Number("1.5")
			case "rounded-fraction":
				out["graph_revision"] = json.Number("9007199254740993.5")
			}
			return mcpResult("fixture", out)
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a, b := mcp.NewInMemoryTransports()
		ss, err := server.Connect(ctx, a, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ss.Close() }()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "output-validation-test", Version: "1"}, nil).Connect(ctx, b, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cs.Close() }()
		for _, run := range []string{"valid", "missing-field", "wrong-type", "fractional", "rounded-fraction"} {
			result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: mcpToolName(cfg, "run_status"), Arguments: map[string]any{"run": run}})
			if err != nil {
				t.Fatal(err)
			}
			if run == "valid" {
				if result.IsError || result.StructuredContent == nil {
					t.Fatalf("valid exact numeric output rejected: %+v", result)
				}
			} else if !result.IsError || result.StructuredContent != nil || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "validating tool output") {
				t.Fatalf("invalid %s output escaped schema validation: %+v", run, result)
			}
		}
	})
}

func TestMCPToolProfilesExposeExactNamesAndBehavior(t *testing.T) {
	read := "attempt_output,delivery_summary,inspect_cell,inspect_review,list_runs,provider_limit,review_list,run_logs,run_status,validate_plan"
	control := "apply,attempt_output,decide_approval,delivery_summary,inspect_cell,inspect_review,list_runs,pause,provider_limit,resume,retry_cell,review_list,run_logs,run_status,validate_plan"
	canonicalRead := "get_attempt_limit_advisory,get_attempt_output,get_delivery_summary,get_run_status,inspect_cell,inspect_review,list_reviews,list_run_events,list_runs,validate_plan"
	canonicalControl := "decide_approval,get_attempt_limit_advisory,get_attempt_output,get_delivery_summary,get_run_status,inspect_cell,inspect_review,list_reviews,list_run_events,list_runs,pause_run,resume_run,retry_cell,start_run,validate_plan"
	for _, canonical := range []bool{false, true} {
		for _, allowControl := range []bool{false, true} {
			t.Run(fmt.Sprintf("canonical=%t/control=%t", canonical, allowControl), func(t *testing.T) {
				cfg := McpConfig{Workspace: t.TempDir(), canonicalNames: canonical, AllowControl: allowControl}
				cs := connectDefinitionTest(t, cfg)
				result, err := cs.ListTools(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				names := []string{}
				for _, tool := range result.Tools {
					names = append(names, tool.Name)
					annotation := tool.Annotations
					if annotation == nil || annotation.DestructiveHint == nil || annotation.OpenWorldHint == nil {
						t.Fatalf("%s has unspecified hints: %+v", tool.Name, annotation)
					}
					readOnly, destructive, idempotent, openWorld := true, false, true, false
					switch tool.Name {
					case "apply", "start_run", "resume", "resume_run":
						readOnly, destructive, idempotent, openWorld = false, true, false, true
					case "pause", "pause_run":
						readOnly, destructive, idempotent = false, true, false
					case "retry_cell", "decide_approval":
						readOnly, destructive, idempotent = false, true, false
					}
					if annotation.ReadOnlyHint != readOnly || *annotation.DestructiveHint != destructive || annotation.IdempotentHint != idempotent || *annotation.OpenWorldHint != openWorld {
						t.Fatalf("%s hints: %+v", tool.Name, annotation)
					}
					if strings.Contains(tool.Description, "{{") || tool.Description == "" {
						t.Fatalf("invalid profile description: %s", tool.Description)
					}
					var schema struct {
						Properties map[string]struct{ Description string }
					}
					data, err := json.Marshal(tool.OutputSchema)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &schema); err != nil || len(schema.Properties) < 2 {
						t.Fatalf("%s has no meaningful output contract: %s", tool.Name, data)
					}
					for key, property := range schema.Properties {
						if property.Description == "" {
							t.Fatalf("%s output field %s has no semantics", tool.Name, key)
						}
					}
				}
				sort.Strings(names)
				want := read
				if allowControl {
					want = control
				}
				if canonical {
					want = canonicalRead
					if allowControl {
						want = canonicalControl
					}
				}
				if strings.Join(names, ",") != want {
					t.Fatalf("got %s; want %s", strings.Join(names, ","), want)
				}
				if !allowControl {
					for _, disabled := range []string{"apply", "start_run", "pause", "pause_run", "resume", "resume_run", "retry_cell", "decide_approval"} {
						_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: disabled, Arguments: map[string]any{}})
						if err == nil {
							t.Fatalf("disabled control %s accepted", disabled)
						}
					}
				}
			})
		}
	}
}

func TestCanonicalNamesRouteWithoutLegacyAliases(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("worker", 0))
	cs := connectDefinitionTest(t, McpConfig{Workspace: t.TempDir(), canonicalNames: true})
	for _, name := range []string{"get_run_status", "inspect_cell", "list_run_events"} {
		args := map[string]any{"run": root}
		if name == "inspect_cell" {
			args["cell"] = "worker"
		}
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError || result.StructuredContent == nil {
			t.Fatalf("%s result=%+v error=%v", name, result, err)
		}
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_status", Arguments: map[string]any{"run": root}}); err == nil {
		t.Fatal("canonical profile accepted legacy alias")
	}
}

func TestPublishedSelectorSchemasMatchArgumentContracts(t *testing.T) {
	for _, test := range []struct {
		tool    string
		valid   []map[string]any
		invalid []map[string]any
	}{
		{"inspect_review", []map[string]any{{"branch": "feature"}, {"snapshot": strings.Repeat("a", 64)}}, []map[string]any{{}, {"branch": ""}, {"branch": "feature", "snapshot": strings.Repeat("a", 64)}, {"snapshot": strings.Repeat("a", 64), "file": "file.go"}, {"snapshot": strings.Repeat("a", 64), "base": "main"}}},
		{"validate_plan", []map[string]any{{"path": "plan.json"}, {"json": "{}"}}, []map[string]any{{}, {"path": ""}, {"path": "-"}, {"json": ""}, {"path": "plan.json", "json": "{}"}}},
	} {
		data, err := json.Marshal(mcpInputSchema(test.tool))
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range test.valid {
			if err := resolved.Validate(args); err != nil {
				t.Fatalf("%s rejects valid %+v: %v", test.tool, args, err)
			}
		}
		for _, args := range test.invalid {
			if err := resolved.Validate(args); err == nil {
				t.Fatalf("%s accepts invalid %+v", test.tool, args)
			}
		}
	}
}

func TestCanonicalInstructionAndParameterReferences(t *testing.T) {
	cs := connectDefinitionTest(t, McpConfig{Workspace: t.TempDir(), canonicalNames: true, AllowControl: true})
	instructions := cs.InitializeResult().Instructions
	if !strings.Contains(instructions, "resume_run executes") || strings.Contains(instructions, "; resume executes") {
		t.Fatalf("wrong canonical instruction: %s", instructions)
	}
	result, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Tools {
		if tool.Name != "inspect_review" {
			continue
		}
		data, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "review_list") || !strings.Contains(string(data), "list_reviews") {
			t.Fatalf("wrong canonical parameter reference: %s", data)
		}
	}
}

func TestMCPBehaviorHintsExistOnRawWireForEveryProfile(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		for _, control := range []bool{false, true} {
			t.Run(fmt.Sprintf("canonical=%t/control=%t", canonical, control), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				server, client := net.Pipe()
				if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				ss, err := NewMCPServer(McpConfig{Workspace: t.TempDir(), canonicalNames: canonical, AllowControl: control}).Connect(ctx, &mcp.IOTransport{Reader: server, Writer: server}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = client.Close(); _ = ss.Close() }()
				enc, dec := json.NewEncoder(client), json.NewDecoder(client)
				if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw-wire-test", "version": "1"}}}); err != nil {
					t.Fatal(err)
				}
				var first map[string]json.RawMessage
				if err := dec.Decode(&first); err != nil {
					t.Fatal(err)
				}
				if first["error"] != nil {
					t.Fatalf("initialize: %s", first["error"])
				}
				if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
					t.Fatal(err)
				}
				if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}}); err != nil {
					t.Fatal(err)
				}
				var response struct {
					Result struct {
						Tools []struct {
							Name        string                     `json:"name"`
							Annotations map[string]json.RawMessage `json:"annotations"`
						} `json:"tools"`
					} `json:"result"`
					Error json.RawMessage `json:"error"`
				}
				if err := dec.Decode(&response); err != nil {
					t.Fatal(err)
				}
				wantCount := 10
				if control {
					wantCount = 15
				}
				if response.Error != nil || len(response.Result.Tools) != wantCount {
					t.Fatalf("bad wire inventory: %+v", response)
				}
				for _, tool := range response.Result.Tools {
					want := map[string]bool{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
					switch tool.Name {
					case "apply", "start_run", "resume", "resume_run":
						want["readOnlyHint"], want["destructiveHint"], want["idempotentHint"], want["openWorldHint"] = false, true, false, true
					case "pause", "pause_run", "retry_cell", "decide_approval":
						want["readOnlyHint"], want["destructiveHint"], want["idempotentHint"] = false, true, false
					}
					for key, expected := range want {
						if !matchesRawWireHint(tool.Annotations, key, expected) {
							t.Fatalf("%s wire hint %s missing/wrong: %s", tool.Name, key, tool.Annotations[key])
						}
					}
				}
			})
		}
	}
}

func matchesRawWireHint(fields map[string]json.RawMessage, key string, want bool) bool {
	raw, present := fields[key]
	literal := "false"
	if want {
		literal = "true"
	}
	return present && strings.TrimSpace(string(raw)) == literal
}

func TestMCPRawHintAssertionRejectsMissingNullAndNonBooleans(t *testing.T) {
	if matchesRawWireHint(nil, "hint", false) || matchesRawWireHint(nil, "hint", true) {
		t.Fatal("missing key accepted")
	}
	for _, raw := range []string{"null", "0", "\"false\"", "{}", "[]", ""} {
		fields := map[string]json.RawMessage{"hint": json.RawMessage(raw)}
		if matchesRawWireHint(fields, "hint", false) || matchesRawWireHint(fields, "hint", true) {
			t.Fatalf("nonboolean accepted: %s", raw)
		}
	}
	for _, expected := range []bool{false, true} {
		literal := "false"
		if expected {
			literal = "true"
		}
		fields := map[string]json.RawMessage{"hint": json.RawMessage(literal)}
		if !matchesRawWireHint(fields, "hint", expected) || matchesRawWireHint(fields, "hint", !expected) {
			t.Fatal("boolean value not checked exactly")
		}
	}
}

// The lifecycle and delivery controls sit beside tools an agent could confuse
// with them (pause, stop_run and cancel_cell all end work), so each must name
// its siblings with the {{tool}} form, which follows the host's naming contract.
func TestControlToolDescriptionsPointAtTheirSiblings(t *testing.T) {
	for name, siblings := range map[string][]string{
		"pause":             {"{{stop_run}}", "{{cancel_cell}}"},
		"cancel_cell":       {"{{pause}}", "{{stop_run}}", "{{run_status}}", "{{inspect_cell}}"},
		"stop_run":          {"{{pause}}", "{{cancel_cell}}", "{{run_status}}"},
		"assemble_delivery": {"{{run_status}}", "{{delivery_summary}}", "{{check_delivery}}"},
		"check_delivery":    {"{{assemble_delivery}}", "{{delivery_summary}}"},
	} {
		desc := mcpToolDescriptions[name]
		for _, sibling := range siblings {
			if !strings.Contains(desc, sibling) {
				t.Errorf("%s does not reference %s: %q", name, sibling, desc)
			}
		}
	}
}
