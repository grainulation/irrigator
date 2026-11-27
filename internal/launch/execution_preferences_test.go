package launch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func preferenceFile(t *testing.T, body string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "preferences.json")
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestExecutionPreferenceLauncherIdentityAndOverrides(t *testing.T) {
	name := preferenceFile(t, `{"schema":"irrigator.preferences/v1","launcher":{"provider":"codex","model":"gpt-6.2-sol","effort":"high"},"default":{"provider":"claude","model":"sonnet","effort":"low"}}`)
	t.Setenv(ExecutionPreferencesEnv, name)
	selected, err := ResolveExecutionPreference(ExecutionPreference{}, "", true)
	if err != nil || selected.Source != "launcher" || selected.Preference.Provider != "codex" || selected.Preference.Model != "gpt-6.2-sol" || selected.Preference.Effort != "high" {
		t.Fatalf("lost launcher identity: %+v, %v", selected, err)
	}
	selected, err = ResolveExecutionPreference(ExecutionPreference{Effort: "medium"}, "", true)
	if err != nil || selected.Preference.Model != "gpt-6.2-sol" || selected.Preference.Effort != "medium" || selected.Source != "explicit" {
		t.Fatalf("override did not preserve matching identity: %+v %v", selected, err)
	}
	selected, err = ResolveExecutionPreference(ExecutionPreference{Provider: "claude", Model: "opus"}, "", true)
	if err != nil || selected.Preference.Effort != "" || selected.Preference.Model != "opus" {
		t.Fatalf("carried Codex effort across providers: %+v %v", selected, err)
	}
}

func TestExecutionPreferenceDoesNotGuessOrOverrideOptOut(t *testing.T) {
	t.Setenv(ExecutionPreferencesEnv, "")
	if _, err := ResolveExecutionPreference(ExecutionPreference{}, "", false); err == nil {
		t.Fatal("guessed an installed provider")
	}
	name := preferenceFile(t, `{"schema":"irrigator.preferences/v1","launcher":{"provider":"codex","model":"gpt-6.2-sol","effort":"high"},"disabled_providers":["codex"]}`)
	if _, err := ResolveExecutionPreference(ExecutionPreference{Provider: "codex", Model: "gpt-6.2-sol"}, name, true); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal("explicit selection bypassed opt-out", err)
	}
	if _, err := ResolveExecutionPreference(ExecutionPreference{Provider: "codex", Model: "gpt-6.2-sol", Effort: "unknown"}, "", true); err == nil {
		t.Fatal("silently downgraded unsupported effort")
	}
}

func TestExecutionPreferenceStrictInput(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"schema":"future"}`, `{"schema":"irrigator.preferences/v1","schema":"irrigator.preferences/v1"}`,
		`{"Schema":"irrigator.preferences/v1"}`, `{"schema":"irrigator.preferences/v1","launcher":{"Provider":"codex"}}`,
		`{"schema":"irrigator.preferences/v1","launcher":null}`, `{"schema":"irrigator.preferences/v1","launcher":{"provider":"codex","model":"a;echo"}}`,
		`{"schema":"irrigator.preferences/v1","launcher":{"provider":"codex","effort":"none"}}`,
		`{"schema":"irrigator.preferences/v1","launcher":{"provider":"ollama","model":"local","endpoint":"http://example.com:11434"}}`,
		`{"schema":"irrigator.preferences/v1","launcher":{"provider":"codex","model":"model","endpoint":"http://127.0.0.1:11434"}}`,
		`{"schema":"irrigator.preferences/v1","include_profiles":["a","a"]}`, strings.Repeat(" ", executionPreferencesLimit+1),
	} {
		if _, err := ReadExecutionPreferences(preferenceFile(t, body)); err == nil {
			t.Fatalf("accepted malformed preference %q", body[:min(len(body), 100)])
		}
	}
}

func TestProfileOptInsNeverWidenAllowedSet(t *testing.T) {
	allowed := []string{"primary", "reviewer", "local"}
	got, err := FilterAllowedProfiles(allowed, []string{"primary", "local"}, []string{"local"})
	if err != nil || !reflect.DeepEqual(got, []string{"primary"}) || !reflect.DeepEqual(allowed, []string{"primary", "reviewer", "local"}) {
		t.Fatalf("wrong narrowed set %v %v", got, err)
	}
	for _, include := range [][]string{{"unknown"}, {"local"}} {
		if _, err := FilterAllowedProfiles(allowed, include, []string{"local"}); err == nil {
			t.Fatal("allowed widening or empty policy", include)
		}
	}
}
