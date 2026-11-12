package clock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSkillsDescribeShippedFeatures(t *testing.T) {
	stale := []string{"Do not claim an MCP server", "Irrigator ships no MCP server", "There is no Irrigator MCP server"}
	skills, err := filepath.Glob(filepath.Join("..", "..", "skills", "*", "SKILL.md"))
	if err != nil || len(skills) == 0 {
		t.Fatalf("no skills found: %v", err)
	}
	for _, path := range skills {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stale {
			if strings.Contains(string(raw), s) {
				t.Errorf("%s still says %q", path, s)
			}
		}
	}
	hub := readRepoFile(t, "skills/irrigator/SKILL.md")
	for _, want := range []string{
		"irrigator mcp", "--allow-control", "--executor local|herdr|tmux", "irrigator doctor",
		"sandbox", "not a hardened", "approvals link", "approvals serve", "approvals notify",
	} {
		if !strings.Contains(hub, want) {
			t.Errorf("skills/irrigator/SKILL.md does not mention %q", want)
		}
	}
	if strings.Contains(hub, "Cells are trusted local directories, not sandboxes.\n") {
		t.Error("hub skill keeps the unqualified sandbox statement")
	}
}

func TestSkillsClaudePluginRegistersReadOnlyMCP(t *testing.T) {
	var manifest struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, ".claude-plugin/plugin.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	entry, ok := manifest.MCPServers["irrigator"]
	if !ok || len(manifest.MCPServers) != 1 {
		t.Fatalf("mcpServers = %v", manifest.MCPServers)
	}
	if entry.Command != "irrigator" || len(entry.Args) != 1 || entry.Args[0] != "mcp" {
		t.Fatalf("entry must be exactly `irrigator mcp` (read-only): %+v", entry)
	}
}
