package mcpserver

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/grainulation/irrigator/internal/delivery"
	"github.com/grainulation/irrigator/internal/runops"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpControlInput struct {
	Run string `json:"run"`
	runops.ControlRequest
}

func (t *mcpTools) cancelCell(ctx context.Context, _ *mcp.CallToolRequest, in mcpControlInput) (*mcp.CallToolResult, mcpOut, error) {
	return t.requestControl(ctx, "cancel_cell", in)
}

func (t *mcpTools) stopRun(ctx context.Context, _ *mcp.CallToolRequest, in mcpControlInput) (*mcp.CallToolResult, mcpOut, error) {
	return t.requestControl(ctx, "stop_run", in)
}

func (t *mcpTools) requestControl(ctx context.Context, kind string, in mcpControlInput) (*mcp.CallToolResult, mcpOut, error) {
	if !t.cfg.AllowControl {
		return nil, nil, errors.New("control tools require --allow-control")
	}
	root, _, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	receipt, err := runops.RequestControl(ctx, root, kind, in.ControlRequest)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult("Control intent recorded; inspect workers and unresolved cleanup separately", mcpOut{"run": root, "run_id": in.ExpectedRunID, "request_id": in.RequestID, "requested": kind, "cell": in.Cell, "changed": receipt.Changed, "cleanup_confirmed": false, "dispatched": false})
}

type mcpAssembleInput struct {
	Run string `json:"run"`
	delivery.AssembleRequest
}

func (t *mcpTools) assembleDelivery(ctx context.Context, _ *mcp.CallToolRequest, in mcpAssembleInput) (*mcp.CallToolResult, mcpOut, error) {
	if !t.cfg.AllowControl {
		return nil, nil, errors.New("delivery mutations require --allow-control")
	}
	root, _, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	in.Directory = t.resolvePath(in.Directory)
	result, err := delivery.AssembleCompletedRun(ctx, root, in.AssembleRequest)
	if err != nil {
		return nil, nil, err
	}
	out, err := mcpDiagnosticObject(result)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult("Accepted file maps assembled; no graph work or acceptance command started", out)
}

type mcpCheckInput struct {
	Run string `json:"run"`
	delivery.CompletedRequest
	ExpectedCandidateSHA256 string   `json:"expected_candidate_sha256"`
	Check                   []string `json:"check"`
	TimeoutSeconds          float64  `json:"timeout_seconds"`
	Recheck                 bool     `json:"recheck,omitempty"`
}

func (t *mcpTools) checkDelivery(ctx context.Context, _ *mcp.CallToolRequest, in mcpCheckInput) (*mcp.CallToolResult, mcpOut, error) {
	if !t.cfg.AllowControl {
		return nil, nil, errors.New("delivery mutations require --allow-control")
	}
	if math.IsNaN(in.TimeoutSeconds) || math.IsInf(in.TimeoutSeconds, 0) || in.TimeoutSeconds <= 0 || in.TimeoutSeconds > 86400 {
		return nil, nil, errors.New("timeout_seconds must be above zero and at most 86400")
	}
	root, _, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	result, err := delivery.CheckCompletedDelivery(ctx, root, delivery.CheckRequest{CompletedRequest: in.CompletedRequest, ExpectedCandidateSHA256: in.ExpectedCandidateSHA256, Check: in.Check, Timeout: time.Duration(in.TimeoutSeconds * float64(time.Second)), Recheck: in.Recheck})
	if err != nil {
		return nil, nil, err
	}
	out, err := mcpDiagnosticObject(result)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult("Delivery check outcome: "+result.Status+"; no graph work started", out)
}
