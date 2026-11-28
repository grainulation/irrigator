package checkpoint

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const minRunPrefixLen = 4

type readOnlyRunCandidate struct {
	displayPath string
	canonPath   string
	dirName     string
	runID       string
	underRuns   bool
}

// ResolveReadOnlyRunDir resolves a run reference for read-only CLI commands
// (status, inspect, logs, debug, graph, triage, cost).
//
// If arg is an existing directory path, it is returned unchanged. If arg is a
// bare name or relative under ./runs, ./runs/ (and the session registry for
// bare names) is searched for run directories whose directory name or
// state.json run_id matches arg (exact basename under ./runs/<arg>, exact
// run_id, or a prefix of at least 4 characters).
//
// Mutating commands must never call this function.
func ResolveReadOnlyRunDir(arg string) (string, error) {
	if arg == "" || strings.ContainsRune(arg, 0) {
		return "", notFoundRunError(arg)
	}
	isExistingDir := false
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		isExistingDir = true
		if strings.ContainsRune(arg, filepath.Separator) || strings.ContainsRune(arg, '/') || arg == "." || arg == ".." {
			return arg, nil
		}
		if st, err := os.Lstat(filepath.Join(arg, "state.json")); err == nil && st.Mode().IsRegular() {
			return arg, nil
		}
	}
	query, includeRegistry, ok := parseReadOnlyRunQuery(arg)
	if !ok {
		if isExistingDir {
			return arg, nil
		}
		return "", notFoundRunError(arg)
	}
	candidates := collectReadOnlyRunCandidates(includeRegistry)
	var matches []readOnlyRunCandidate
	for _, c := range candidates {
		if matchReadOnlyRunCandidate(c, query) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 1 {
		return matches[0].displayPath, nil
	}
	if len(matches) > 1 {
		paths := make([]string, 0, len(matches))
		for _, m := range matches {
			paths = append(paths, m.displayPath)
		}
		sort.Strings(paths)
		return "", fmt.Errorf("ambiguous run prefix %q matches %d runs: %s", arg, len(paths), strings.Join(paths, ", "))
	}
	if isExistingDir {
		return arg, nil
	}
	return "", notFoundRunError(arg)
}

func parseReadOnlyRunQuery(arg string) (string, bool, bool) {
	if !strings.ContainsRune(arg, filepath.Separator) && !strings.ContainsRune(arg, '/') {
		if arg == "." || arg == ".." || arg == "" {
			return "", false, false
		}
		return arg, true, true
	}
	if filepath.IsAbs(arg) {
		return "", false, false
	}
	clean := filepath.Clean(arg)
	prefix := "runs" + string(filepath.Separator)
	if strings.HasPrefix(clean, prefix) {
		sub := strings.TrimPrefix(clean, prefix)
		if sub != "" && sub != "." && sub != ".." && !strings.ContainsRune(sub, filepath.Separator) && !strings.ContainsRune(sub, '/') {
			return sub, false, true
		}
	}
	return "", false, false
}

func canonicalRunPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

func readCheckpointRunID(statePath string) (string, bool) {
	info, err := os.Lstat(statePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > stateReadLimit {
		return "", false
	}
	f, err := os.Open(statePath)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, stateReadLimit+1))
	if err != nil || int64(len(data)) > stateReadLimit {
		return "", false
	}
	var head struct {
		RunID    string `json:"run_id"`
		PlanHash string `json:"plan_hash"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return "", false
	}
	if head.RunID == "" && head.PlanHash != "" {
		head.RunID = "legacy-" + head.PlanHash
	}
	return head.RunID, true
}

func collectReadOnlyRunCandidates(includeRegistry bool) []readOnlyRunCandidate {
	var out []readOnlyRunCandidate
	seen := map[string]bool{}
	if entries, err := os.ReadDir("runs"); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			relPath := filepath.Join("runs", e.Name())
			runID, ok := readCheckpointRunID(filepath.Join(relPath, "state.json"))
			if !ok {
				continue
			}
			canon := canonicalRunPath(relPath)
			if seen[canon] {
				continue
			}
			seen[canon] = true
			out = append(out, readOnlyRunCandidate{
				displayPath: relPath,
				canonPath:   canon,
				dirName:     e.Name(),
				runID:       runID,
				underRuns:   true,
			})
		}
	}
	if !includeRegistry {
		return out
	}
	regPath := filepath.Join(".irrigator", "current-session.json")
	if envReg := os.Getenv("IRRIGATOR_SESSION_REGISTRY"); envReg != "" {
		if !filepath.IsAbs(envReg) || strings.ContainsRune(envReg, 0) {
			return out
		}
		regPath = filepath.Clean(envReg)
	}
	info, err := os.Lstat(regPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64*1024 {
		return out
	}
	data, err := os.ReadFile(regPath)
	if err != nil || len(data) > 64*1024 {
		return out
	}
	var reg struct {
		Version int    `json:"version"`
		RunID   string `json:"run_id"`
		RunPath string `json:"run_path"`
		Entries []struct {
			RunID   string `json:"run_id"`
			RunPath string `json:"run_path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return out
	}
	addRegEntry := func(runPath, fallbackRunID string) {
		if !filepath.IsAbs(runPath) || filepath.Clean(runPath) != runPath {
			return
		}
		dirInfo, err := os.Stat(runPath)
		if err != nil || !dirInfo.IsDir() {
			return
		}
		runID, ok := readCheckpointRunID(filepath.Join(runPath, "state.json"))
		if !ok {
			return
		}
		if runID == "" {
			runID = fallbackRunID
		}
		canon := canonicalRunPath(runPath)
		if seen[canon] {
			return
		}
		seen[canon] = true
		out = append(out, readOnlyRunCandidate{
			displayPath: runPath,
			canonPath:   canon,
			dirName:     filepath.Base(runPath),
			runID:       runID,
			underRuns:   false,
		})
	}
	switch reg.Version {
	case 1:
		addRegEntry(reg.RunPath, reg.RunID)
	case 2:
		for _, e := range reg.Entries {
			addRegEntry(e.RunPath, e.RunID)
		}
	}
	return out
}

func matchReadOnlyRunCandidate(c readOnlyRunCandidate, query string) bool {
	if c.underRuns && c.dirName == query {
		return true
	}
	if c.runID != "" && c.runID == query {
		return true
	}
	if len(query) < minRunPrefixLen {
		return false
	}
	return strings.HasPrefix(c.dirName, query) || (c.runID != "" && strings.HasPrefix(c.runID, query))
}

func notFoundRunError(arg string) error {
	_, err := os.Stat(filepath.Join(arg, "state.json"))
	if err != nil {
		return err
	}
	return &os.PathError{Op: "stat", Path: filepath.Join(arg, "state.json"), Err: os.ErrNotExist}
}
