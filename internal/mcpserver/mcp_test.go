package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPSanitizeRedactsCredentialFields(t *testing.T) {
	got, _ := mcpSanitize(map[string]any{"api_key": "k", "nested": map[string]any{"Authorization": "Bearer x"}, "env": []string{"A=b"}, "ok": "fine"})
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "Bearer") || strings.Contains(string(raw), `"k"`) || strings.Contains(string(raw), "A=b") || !strings.Contains(string(raw), "fine") {
		t.Fatalf("sanitized: %s", raw)
	}
	if s, cut := mcpTruncate(strings.Repeat("é", 10), 5); !cut || !strings.HasPrefix(s, "éé") {
		t.Fatalf("rune-boundary truncation: %q", s)
	}
}
