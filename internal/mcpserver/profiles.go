package mcpserver

import "github.com/modelcontextprotocol/go-sdk/mcp"

func validMCPProfile(profile string) bool {
	switch profile {
	case "", "legacy", "inspection", "execution", "delivery":
		return true
	}
	return false
}

// Inventories are selected once at startup. The empty/legacy contract keeps
// its existing 10/15 tools. New execution controls replace review inspection in
// execution; delivery owns the optional review and assembly/check workflow.
func registerMCPProfile(s *mcp.Server, cfg McpConfig, t *mcpTools) {
	profile := cfg.Profile
	if profile == "" {
		profile = "legacy"
	}
	if !validMCPProfile(profile) {
		return
	}
	addMCPTool(s, cfg, "list_runs", t.listRuns)
	addMCPTool(s, cfg, "run_status", t.runStatus)
	addMCPTool(s, cfg, "run_logs", t.runLogs)
	addMCPTool(s, cfg, "delivery_summary", t.deliverySummary)
	addMCPTool(s, cfg, "inspect_cell", t.inspectCell)
	if profile != "delivery" {
		addMCPTool(s, cfg, "attempt_output", t.attemptOutput)
		addMCPTool(s, cfg, "provider_limit", t.providerLimit)
		addMCPTool(s, cfg, "validate_plan", t.validatePlan)
	}
	if profile != "execution" {
		addMCPTool(s, cfg, "review_list", t.reviewList)
		addMCPTool(s, cfg, "inspect_review", t.inspectReview)
	}
	if !cfg.AllowControl || profile == "inspection" {
		return
	}
	if profile == "delivery" {
		addMCPTool(s, cfg, "assemble_delivery", t.assembleDelivery)
		addMCPTool(s, cfg, "check_delivery", t.checkDelivery)
		return
	}
	addMCPTool(s, cfg, "apply", t.apply)
	addMCPTool(s, cfg, "pause", t.pause)
	addMCPTool(s, cfg, "resume", t.resume)
	addMCPTool(s, cfg, "retry_cell", t.retryCell)
	addMCPTool(s, cfg, "decide_approval", t.decideApproval)
	if profile == "execution" {
		addMCPTool(s, cfg, "cancel_cell", t.cancelCell)
		addMCPTool(s, cfg, "stop_run", t.stopRun)
	}
}
