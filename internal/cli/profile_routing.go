package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
)

func profileRoutingCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("profiles route", flag.ContinueOnError)
	policyPath := fs.String("policy", "", "explicit roles/difficulty JSON policy")
	preferences := fs.String("preferences", "", "execution preference JSON; otherwise IRRIGATOR_EXECUTION_PREFERENCES")
	multimodel := fs.Bool("multimodel", false, "explicitly opt a new launcher-selected plan into multiple profiles")
	include := fs.String("include-profiles", "", "comma-separated profiles to opt in; narrows the existing allowed set")
	exclude := fs.String("exclude-profiles", "", "comma-separated profiles to opt out")
	out := fs.String("out", "", "new routed plan file; omitted previews only")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 1 || *policyPath == "" {
		return 2, errors.New("profiles route PLAN --policy FILE [--out NEWPLAN]")
	}
	plan, err := checkpoint.LoadPlan(fs.Arg(0))
	if err != nil {
		return 1, err
	}
	policyAbs, err := filepath.Abs(*policyPath)
	if err != nil {
		return 1, err
	}
	f, err := localfs.OpenContextFile(policyAbs, os.O_RDONLY, 0)
	if err != nil {
		return 1, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 1, errors.New("role policy must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil {
		return 1, err
	}
	if len(raw) > 256*1024 {
		return 1, errors.New("role policy exceeds 256 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := jsonio.ParseStrictJSON(dec, 0); err != nil {
		return 1, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return 1, errors.New("role policy requires one JSON object")
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var policy launch.RolePolicy
	if err := dec.Decode(&policy); err != nil {
		return 1, err
	}
	routed, routes, err := launch.RouteProfiles(plan, policy)
	if err != nil {
		return 1, err
	}
	prefs, err := launch.ReadExecutionPreferences(*preferences)
	if err != nil {
		return 1, err
	}
	includes, excludes := append([]string{}, prefs.IncludeProfiles...), append([]string{}, prefs.ExcludeProfiles...)
	if *include != "" {
		includes = strings.Split(*include, ",")
	}
	if *exclude != "" {
		excludes = append(excludes, strings.Split(*exclude, ",")...)
	}
	for _, name := range routed.Execution.AllowedProfiles {
		profile := routed.Execution.Catalog.Profiles[name]
		adapter := routed.Execution.Catalog.Adapters[routed.Execution.Catalog.Instances[profile.Instance].Adapter]
		provider := strings.TrimSuffix(adapter.Kind, "_cli")
		if provider == "ollama_http" {
			provider = "ollama"
		}
		if slices.Contains(prefs.DisabledProviders, provider) {
			excludes = append(excludes, name)
		}
	}
	allowed, err := launch.FilterAllowedProfiles(routed.Execution.AllowedProfiles, includes, excludes)
	if err != nil {
		return 1, err
	}
	used := map[string]bool{}
	for i := range routed.Tasks {
		// Freeze the prior default before narrowing the new plan's allowed set.
		profile := model.ManagedProfile(routed, routed.Tasks[i])
		if !slices.Contains(allowed, profile) {
			return 1, errors.New("role routing selects an opted-out profile; update the explicit routes")
		}
		routed.Tasks[i].Profile = profile
		used[profile] = true
	}
	if len(used) > 1 && !*multimodel && !prefs.Multimodel && (routed.Context["execution_selection"] != "" || prefs.Schema != "") {
		return 1, errors.New("single-model preferences cannot create mixed routing; opt in with --multimodel and allowed profiles")
	}
	routed.Execution.AllowedProfiles = allowed
	if !slices.Contains(allowed, routed.Execution.Catalog.DefaultProfile) {
		routed.Execution.Catalog.DefaultProfile = allowed[0]
	}
	if err := checkpoint.Validate(routed); err != nil {
		return 1, err
	}
	if *out != "" {
		if err := checkpoint.WriteContextPlan(*out, routed); err != nil {
			return 1, err
		}
	}
	return 0, jsonio.PrintJSON(map[string]any{"schema": "irrigator.role-preview/v1", "routes": routes, "out": *out, "dispatched": false, "actual_model_verified": false, "active_runs_changed": false, "allowed_profiles": allowed, "multimodel": len(used) > 1, "exposure_basis": "per-operation caps; helpers and continuations may add operations"})
}
