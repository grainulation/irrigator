package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/grainulation/irrigator/internal/delivery"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpProviderLimitInput struct {
	Run     string `json:"run" jsonschema:"saved run directory or a run name under --runs"`
	Node    string `json:"node" jsonschema:"exact saved node ID"`
	Attempt int    `json:"attempt" jsonschema:"explicit historical attempt number"`
}

func (t *mcpTools) providerLimit(_ context.Context, _ *mcp.CallToolRequest, in mcpProviderLimitInput) (*mcp.CallToolResult, mcpOut, error) {
	root, _, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	advisory, err := provider.DiagnoseLimit(root, in.Node, in.Attempt, time.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	out, err := mcpDiagnosticObject(advisory)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult("Provider reset advisory: "+advisory.Status+"; no retry authorized", out)
}

func (t *mcpTools) deliverySummary(_ context.Context, _ *mcp.CallToolRequest, in mcpRunInput) (*mcp.CallToolResult, mcpOut, error) {
	root, _, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	summary, err := delivery.Summarize(root)
	if err != nil {
		return nil, nil, err
	}
	out, err := mcpDiagnosticObject(summary)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult(fmt.Sprintf("Delivery: %d included task(s), %d excluded, %d conflict(s); no work started", len(summary.Included), len(summary.Excluded), len(summary.Conflicts)), out)
}

func mcpDiagnosticObject(v any) (mcpOut, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out mcpOut
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
