package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestExecutionProfileOfflineBindingAndCWD(t *testing.T) {
	path, c, marker := executionFixture(t, "claude_cli")
	t.Setenv("PROFILE_TEST_SECRET", "secret-value-never-output")
	before, err := ResolveExecutionProfile(path, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(before)
	if strings.Contains(string(raw), "secret-value") || before.AuthVerified || before.Dispatched || before.ActualModel != nil {
		t.Fatalf("false live capability or secret: %s", raw)
	}
	if !before.ExecutableVerified || before.OperationBudgetMicros != 250000 || before.Digest == "" {
		t.Fatalf("invalid binding: %+v", before)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("resolver executed adapter")
	}
	t.Chdir(t.TempDir())
	after, err := ResolveExecutionProfile(path, "small")
	if err != nil || after.Digest != before.Digest {
		t.Fatalf("cwd changed binding: %v", err)
	}
	indented, _ := json.MarshalIndent(c, "", "  ")
	if err = os.WriteFile(path, indented, 0600); err != nil {
		t.Fatal(err)
	}
	after, err = ResolveExecutionProfile(path, "")
	if err != nil || after.Digest != before.Digest {
		t.Fatalf("format changed canonical binding: %v", err)
	}
	p := c.Profiles["small"]
	p.OperationBudgetUSD = "0.5"
	c.Profiles["small"] = p
	writeExecutionFixture(t, path, c)
	after, err = ResolveExecutionProfile(path, "")
	if err != nil || after.Digest == before.Digest || before.OperationBudgetMicros != 250000 {
		t.Fatal("binding not immutable or constraints omitted from digest")
	}
}

func TestExecutionProfileStrictCatalog(t *testing.T) {
	path, _, _ := executionFixture(t, "mock")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(string) string{
		"duplicate": func(s string) string {
			return strings.Replace(s, `"schema":`, `"schema":"irrigator.execution/v1","schema":`, 1)
		},
		"alias": func(s string) string { return strings.Replace(s, `"limits":`, `"Limits":`, 1) },
		"unknown": func(s string) string {
			return strings.Replace(s, `"kind":"mock"`, `"kind":"mock","api_key":"secret"`, 1)
		},
		"missing_tools": func(s string) string { return strings.Replace(s, `"tools":[],`, "", 1) },
		"null_tools":    func(s string) string { return strings.Replace(s, `"tools":[]`, `"tools":null`, 1) },
		"trailing":      func(s string) string { return s + "{}" },
		"array":         func(s string) string { return "[" + s + "]" },
		"float_limit":   func(s string) string { return strings.Replace(s, `"input_bytes":1024`, `"input_bytes":1.5`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseExecutionCatalog([]byte(mutate(string(raw)))); err == nil {
				t.Fatal("accepted ambiguous catalog")
			}
		})
	}
	for _, amount := range []string{"-1", "1e2", "NaN", "+1", "01", "0.0000001", "9999999999999999999999999", "1000000.000001"} {
		if _, err := ExecutionBudgetMicros(amount); err == nil {
			t.Fatalf("accepted budget %q", amount)
		}
	}
	if n, err := ExecutionBudgetMicros("0.000001"); err != nil || n != 1 {
		t.Fatal("lost fixed-point precision")
	}
}

func TestExecutionProfileCapabilitiesFailClosed(t *testing.T) {
	for name, mutate := range map[string]func(*model.ExecutionCatalog){
		"tools": func(c *model.ExecutionCatalog) {
			p := c.Profiles["small"]
			p.Tools = []string{"Bash"}
			c.Profiles["small"] = p
		},
		"capability": func(c *model.ExecutionCatalog) {
			p := c.Profiles["small"]
			p.RequiredCapabilities = []string{"token_counting"}
			c.Profiles["small"] = p
		},
		"effort": func(c *model.ExecutionCatalog) { p := c.Profiles["small"]; p.Effort = "max"; c.Profiles["small"] = p },
		"input": func(c *model.ExecutionCatalog) {
			p := c.Profiles["small"]
			p.Limits.InputBytes = model.MaxInputBytes + 1
			c.Profiles["small"] = p
		},
		"instance": func(c *model.ExecutionCatalog) {
			p := c.Profiles["small"]
			p.Instance = "missing"
			c.Profiles["small"] = p
		},
		"args": func(c *model.ExecutionCatalog) {
			a := c.Adapters["adapter"]
			a.Command = append(a.Command, "--unsafe")
			c.Adapters["adapter"] = a
		},
		"relative": func(c *model.ExecutionCatalog) {
			a := c.Adapters["adapter"]
			a.Command = []string{"claude"}
			c.Adapters["adapter"] = a
		},
		"credential": func(c *model.ExecutionCatalog) {
			i := c.Instances["account"]
			i.CredentialRefs[0].Kind = "literal"
			c.Instances["account"] = i
		},
	} {
		t.Run(name, func(t *testing.T) {
			path, c, _ := executionFixture(t, "claude_cli")
			mutate(&c)
			writeExecutionFixture(t, path, c)
			if _, err := ResolveExecutionProfile(path, ""); err == nil {
				t.Fatal("accepted unsupported profile")
			}
		})
	}
	path, c, _ := executionFixture(t, "mock")
	p := c.Profiles["small"]
	p.OperationBudgetUSD = "0.01"
	c.Profiles["small"] = p
	writeExecutionFixture(t, path, c)
	if _, err := ResolveExecutionProfile(path, ""); err == nil {
		t.Fatal("accepted paid mock")
	}
}

func TestExecutionProfileRejectsChangedExecutableAndLinks(t *testing.T) {
	path, c, _ := executionFixture(t, "claude_cli")
	exe := c.Adapters["adapter"].Command[0]
	if err := os.WriteFile(exe, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveExecutionProfile(path, ""); err == nil {
		t.Fatal("accepted wrong executable hash")
	}
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExecutionCatalog(link); err == nil {
		t.Fatal("accepted catalog symlink")
	}
	fifo := filepath.Join(filepath.Dir(path), "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExecutionCatalog(fifo); err == nil {
		t.Fatal("accepted FIFO")
	}
}
