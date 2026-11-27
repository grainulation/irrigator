package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grainulation/irrigator/internal/jsonio"
)

const ExecutionPreferencesSchema = "irrigator.preferences/v1"
const ExecutionPreferencesEnv = "IRRIGATOR_EXECUTION_PREFERENCES"
const executionPreferencesLimit = 64 * 1024

// ExecutionPreference is a requested identity, not proof of authentication,
// entitlement or dispatch. Host integrations supply canonical model selectors.
type ExecutionPreference struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// ExecutionPreferences is independent of the strict launch/v1 capsule and of
// admitted plans. Loading it never changes an existing run or widens its grants.
type ExecutionPreferences struct {
	Schema            string               `json:"schema"`
	Launcher          *ExecutionPreference `json:"launcher,omitempty"`
	Default           *ExecutionPreference `json:"default,omitempty"`
	Multimodel        bool                 `json:"multimodel,omitempty"`
	IncludeProfiles   []string             `json:"include_profiles,omitempty"`
	ExcludeProfiles   []string             `json:"exclude_profiles,omitempty"`
	DisabledProviders []string             `json:"disabled_providers,omitempty"`
}

type ExecutionSelection struct {
	Schema     string              `json:"schema"`
	Preference ExecutionPreference `json:"preference"`
	Source     string              `json:"source"`
}

// ExecutionEffortSupported describes the effort values represented by the
// current adapters, not live entitlement for any model.
func ExecutionEffortSupported(provider, effort string) bool {
	switch provider {
	case "codex", "claude":
		return slices.Contains([]string{"low", "medium", "high"}, effort)
	case "agy":
		return effort == "low"
	case "ollama":
		return effort == "none"
	}
	return false
}

func validateExecutionPreference(p ExecutionPreference) error {
	if !slices.Contains([]string{"claude", "codex", "agy", "ollama"}, p.Provider) {
		return errors.New("provider must be claude, codex, agy or ollama")
	}
	if p.Model != "" && !ExecutionModel.MatchString(p.Model) {
		return errors.New("model must be a canonical provider selector")
	}
	if p.Endpoint != "" {
		if p.Provider != "ollama" {
			return errors.New("endpoint preferences are only supported for local Ollama")
		}
		if err := ValidateOllamaEndpoint(p.Endpoint); err != nil {
			return err
		}
	}
	if p.Effort != "" && !ExecutionEffortSupported(p.Provider, p.Effort) {
		return fmt.Errorf("unsupported %s effort %q; no effort fallback was applied", p.Provider, p.Effort)
	}
	return nil
}

// ParseExecutionSelector parses an explicit provider/model identity.
func ParseExecutionSelector(selector string) (ExecutionPreference, error) {
	if selector == "" {
		return ExecutionPreference{}, nil
	}
	provider, name, ok := strings.Cut(selector, ":")
	if !ok || name == "" {
		return ExecutionPreference{}, errors.New("profile requires PROVIDER:MODEL")
	}
	p := ExecutionPreference{Provider: provider, Model: name}
	return p, validateExecutionPreference(p)
}

// ReadExecutionPreferences reads explicit saved preferences without probing a
// provider or reading host credentials. An omitted path consults only the named
// environment variable, never guesses a model from a process or CLI default.
func ReadExecutionPreferences(name string) (ExecutionPreferences, error) {
	var p ExecutionPreferences
	if name == "" {
		name = os.Getenv(ExecutionPreferencesEnv)
	}
	if name == "" {
		return p, nil
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return p, err
	}
	f, err := OpenContextFile(abs, os.O_RDONLY, 0)
	if err != nil {
		return p, fmt.Errorf("execution preferences: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > executionPreferencesLimit {
		return p, errors.New("execution preferences must be a regular file of at most 64 KiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, executionPreferencesLimit+1))
	if err != nil || len(raw) > executionPreferencesLimit {
		return p, errors.New("execution preferences exceed read bound")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	value, err := jsonio.ParseStrictJSON(dec, 0)
	if err != nil || dec.Decode(new(any)) != io.EOF {
		return p, errors.New("execution preferences require one object without duplicate fields")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return p, errors.New("execution preferences require an object")
	}
	allowed := []string{"schema", "launcher", "default", "multimodel", "include_profiles", "exclude_profiles", "disabled_providers"}
	for key, value := range object {
		if !slices.Contains(allowed, key) || value == nil {
			return p, errors.New("unknown, noncanonical or null execution preference field")
		}
		if key == "launcher" || key == "default" {
			tuple, ok := value.(map[string]any)
			if !ok {
				return p, errors.New("execution preference identity must be an object")
			}
			for field, v := range tuple {
				if !slices.Contains([]string{"provider", "model", "effort", "endpoint"}, field) || v == nil {
					return p, errors.New("unknown, noncanonical or null execution identity field")
				}
			}
		}
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, errors.New("invalid execution preference field type")
	}
	if p.Schema != ExecutionPreferencesSchema {
		return p, errors.New("unsupported execution preferences schema")
	}
	for _, pref := range []*ExecutionPreference{p.Launcher, p.Default} {
		if pref != nil {
			if err := validateExecutionPreference(*pref); err != nil {
				return p, err
			}
		}
	}
	for _, names := range [][]string{p.IncludeProfiles, p.ExcludeProfiles, p.DisabledProviders} {
		if len(names) > 128 {
			return p, errors.New("execution preference list exceeds 128 entries")
		}
		seen := map[string]bool{}
		for _, name := range names {
			if !ExecutionName.MatchString(name) || seen[name] {
				return p, errors.New("invalid or duplicate execution preference list entry")
			}
			seen[name] = true
		}
	}
	for _, provider := range p.DisabledProviders {
		if !slices.Contains([]string{"claude", "codex", "agy", "ollama"}, provider) {
			return p, errors.New("disabled_providers requires canonical provider names")
		}
	}
	return p, nil
}

// ResolveExecutionPreference resolves new-run preferences without invoking a
// provider. Explicit values override launcher values and saved defaults.
func ResolveExecutionPreference(override ExecutionPreference, name string, requireModel bool) (ExecutionSelection, error) {
	prefs, err := ReadExecutionPreferences(name)
	if err != nil {
		return ExecutionSelection{}, err
	}
	selected, source := ExecutionPreference{}, ""
	if prefs.Default != nil {
		selected, source = *prefs.Default, "saved_default"
	}
	if prefs.Launcher != nil {
		selected, source = *prefs.Launcher, "launcher"
	}
	if override.Provider != "" {
		// Never carry a model selector or effort across providers.
		if override.Provider != selected.Provider {
			selected = ExecutionPreference{Provider: override.Provider}
		}
		selected.Provider, source = override.Provider, "explicit"
	}
	if override.Model != "" {
		selected.Model, source = override.Model, "explicit"
	}
	if override.Effort != "" {
		selected.Effort, source = override.Effort, "explicit"
	}
	if override.Endpoint != "" {
		selected.Endpoint, source = override.Endpoint, "explicit"
	}
	if selected.Provider == "" {
		return ExecutionSelection{}, errors.New("no execution provider selected; pass --profile PROVIDER:MODEL (or --agent for code), or supply launcher preferences; run irrigator agents for available routes")
	}
	if slices.Contains(prefs.DisabledProviders, selected.Provider) {
		return ExecutionSelection{}, fmt.Errorf("provider %s is disabled by execution preferences; change the opt-in policy before using it", selected.Provider)
	}
	if requireModel && selected.Model == "" {
		return ExecutionSelection{}, errors.New("the selected provider has no model; supply a canonical model selector instead of inferring the conversation model")
	}
	if err := validateExecutionPreference(selected); err != nil {
		return ExecutionSelection{}, err
	}
	return ExecutionSelection{Schema: "irrigator.selection/v1", Preference: selected, Source: source}, nil
}

// FilterAllowedProfiles narrows an already admitted-capable set. It never adds
// profiles, changes bindings or edits an existing run.
func FilterAllowedProfiles(allowed, include, exclude []string) ([]string, error) {
	for _, names := range [][]string{include, exclude} {
		for _, name := range names {
			if !slices.Contains(allowed, name) {
				return nil, fmt.Errorf("profile %s is not in the plan's allowed set", name)
			}
		}
	}
	result := []string{}
	for _, name := range allowed {
		if (len(include) == 0 || slices.Contains(include, name)) && !slices.Contains(exclude, name) {
			result = append(result, name)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("profile opt-in selection is empty")
	}
	return result, nil
}
