//go:build integration

package cli

import (
	"strings"
	"testing"
)

func TestShapeCLIMutuallyExclusive(t *testing.T) {
	code, err := runCommand([]string{"size", "plan.json", "--shape", "2x2"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("%d %v", code, err)
	}
}
