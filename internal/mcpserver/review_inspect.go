package mcpserver

import (
	"context"
	"fmt"

	"github.com/grainulation/irrigator/internal/branchreview"
	"github.com/grainulation/irrigator/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpInspectReviewInput struct {
	Workspace string `json:"workspace,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"`
	Base      string `json:"base,omitempty"`
	File      string `json:"file,omitempty"`
}

func (t *mcpTools) inspectReview(_ context.Context, _ *mcp.CallToolRequest, in mcpInspectReviewInput) (*mcp.CallToolResult, mcpOut, error) {
	dir := t.cfg.Workspace
	if in.Workspace != "" {
		dir = t.resolvePath(in.Workspace)
	}
	ws, err := session.ChangeWorkspace(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace: %w", err)
	}
	out, err := branchreview.InspectReview(ws, in.Branch, in.Base, in.Snapshot, in.File)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult("Read queued "+out["kind"].(string)+" review; no decision recorded", out)
}
