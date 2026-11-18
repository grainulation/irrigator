package runops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/textsafe"
)

const (
	maxActivityTailBytes = 4 * 1024
	maxActivityRunes     = 200
)

// CellActivity is a bounded, read-only snapshot of the latest progress line
// from a cell attempt's stderr log.
type CellActivity struct {
	Attempt   int    `json:"attempt"`
	Line      string `json:"line"`
	Truncated bool   `json:"truncated"`
	UpdatedAt string `json:"updated_at"`
}

// ReadCellLastActivity reads at most the last 4 KiB of
// <root>/cells/<id>/<attempt>.stderr.log (regular file only, no symlinks),
// extracts the last non-empty line, sanitizes it via textsafe.Terminal, and
// caps it at 200 runes.
func ReadCellLastActivity(root, id string, attempt int) *CellActivity {
	if root == "" || id == "" || attempt < 1 {
		return nil
	}
	cellsDir := filepath.Join(root, "cells")
	if info, err := os.Lstat(cellsDir); err != nil || !info.IsDir() {
		return nil
	}
	cellDir := filepath.Join(cellsDir, id)
	if info, err := os.Lstat(cellDir); err != nil || !info.IsDir() {
		return nil
	}
	logPath := filepath.Join(cellDir, strconv.Itoa(attempt)+".stderr.log")
	linfo, err := os.Lstat(logPath)
	if err != nil || !linfo.Mode().IsRegular() || linfo.Size() <= 0 {
		return nil
	}
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	finfo, err := f.Stat()
	if err != nil || !finfo.Mode().IsRegular() || !os.SameFile(linfo, finfo) || finfo.Size() <= 0 {
		return nil
	}
	size := finfo.Size()
	offset := int64(0)
	readLen := size
	if size > maxActivityTailBytes {
		offset = size - maxActivityTailBytes
		readLen = maxActivityTailBytes
	}
	buf := make([]byte, readLen)
	n, err := f.ReadAt(buf, offset)
	if n <= 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return nil
	}
	chunk := string(buf[:n])
	if offset > 0 {
		for len(chunk) > 0 && !utf8.RuneStart(chunk[0]) {
			chunk = chunk[1:]
		}
	}
	lines := strings.Split(chunk, "\n")
	rawLine := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			rawLine = trimmed
			break
		}
	}
	if rawLine == "" {
		return nil
	}
	clean := textsafe.Terminal(rawLine)
	runes := []rune(clean)
	truncated := false
	if len(runes) > maxActivityRunes {
		clean = string(runes[:maxActivityRunes])
		truncated = true
	}
	return &CellActivity{
		Attempt:   attempt,
		Line:      clean,
		Truncated: truncated,
		UpdatedAt: finfo.ModTime().UTC().Format(time.RFC3339),
	}
}

func GraphMermaid(s *model.State) string {
	var out strings.Builder
	out.WriteString("flowchart TD\n")
	nodes := map[string]string{}
	for i, t := range s.AllTasks() {
		nodes[t.ID] = fmt.Sprintf("n%d", i)
		fmt.Fprintf(&out, "  %s[\"%s: %s\"]\n", nodes[t.ID], t.ID, s.Tasks[t.ID].Status)
	}
	for _, t := range s.AllTasks() {
		for _, d := range t.DependsOn {
			fmt.Fprintf(&out, "  %s --> %s\n", nodes[d], nodes[t.ID])
		}
		if t.Parent != "" {
			fmt.Fprintf(&out, "  %s -. owns .-> %s\n", nodes[t.Parent], nodes[t.ID])
		}
	}
	return out.String()
}
func InspectNode(root, id string) (map[string]any, error) {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return nil, err
	}
	r := s.Tasks[id]
	if r == nil {
		return nil, errors.New("unknown node")
	}
	out := map[string]any{"node": r, "provider": checkpoint.ProviderName(s, r.Spec), "replay_safe": graph.ReplaySafe(s, r.Spec), "cell": filepath.Join(root, "cells", id), "descendants": managed.Descendants(s, id)[1:]}
	if act := ReadCellLastActivity(root, id, r.Attempt); act != nil {
		out["last_activity"] = act
	}
	return out, nil
}
func ExportArtifacts(root, out, dir string) error {
	// Check the file destination before quarantine traversal: an existing regular
	// file is not a directory and must not yield an opaque ENOTDIR marker error.
	if out != "" {
		if _, err := os.Lstat(out); err == nil {
			return fmt.Errorf("export file already exists: %s; choose a new --out path: %w", out, os.ErrExist)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	for _, dest := range []string{out, dir} {
		if dest != "" {
			if err := checkpoint.RejectQuarantinedRun(dest); err != nil {
				return err
			}
		}
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	results := map[string]map[string]string{}
	files := map[string]string{}
	foldedPaths := map[string]bool{}
	for _, t := range s.AllTasks() {
		r := s.Tasks[t.ID]
		if r.Status != "verified" {
			continue
		}
		results[t.ID] = r.Result
		for field, path := range t.Outputs {
			if !filepath.IsLocal(path) || path == "." {
				return errors.New("unsafe output path")
			}
			path = filepath.Clean(path)
			folded := strings.ToLower(path)
			if folded == "swarm-export.json" || strings.HasPrefix(folded, "swarm-export.json"+string(filepath.Separator)) {
				return errors.New("output path collides with reserved swarm-export.json manifest")
			}
			if foldedPaths[folded] {
				return errors.New("duplicate output path")
			}
			foldedPaths[folded] = true
			files[path] = r.Result[field]
		}
	}
	for path := range foldedPaths {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if foldedPaths[parent] {
				return errors.New("output file and directory paths collide")
			}
		}
	}
	if len(results) == 0 {
		return errors.New("no accepted artifacts to export")
	}
	if dir != "" {
		if len(files) == 0 {
			return errors.New("no task outputs mappings; use JSON export")
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
			return err
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			return errors.New("export directory already exists")
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dir), ".swarm-export-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		store, err := artifactstore.NewLocal(tmp, s.RunID, model.MaxOutputBytes)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		if err := publishExportFiles(context.Background(), store, s.RunID, files); err != nil {
			return err
		}
		manifest, err := json.MarshalIndent(map[string]any{"run_id": s.RunID, "plan_hash": s.PlanHash, "graph_revision": s.GraphRevision, "artifacts": results}, "", "  ")
		if err != nil {
			return err
		}
		// Exclusive files also catch filesystem-specific Unicode aliases without
		// claiming a universal normalization rule for every destination filesystem.
		if err := localfs.WriteNew(filepath.Join(tmp, "swarm-export.json"), append(manifest, '\n')); err != nil {
			return err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return err
		}
		fmt.Println(dir)
		return nil
	}
	bundle := map[string]any{"api_version": "swarm/export-v1", "run_id": s.RunID, "plan_hash": s.PlanHash, "graph_revision": s.GraphRevision, "artifacts": results}
	if out == "" {
		return jsonio.PrintJSON(bundle)
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	if err := localfs.WriteNew(out, append(data, '\n')); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("export file already exists: %s; choose a new --out path: %w", out, os.ErrExist)
		}
		return err
	}
	return nil
}

// Publication is separate from export selection and acceptance. A future remote
// store receives the same bounded immutable content identities, not local paths.
func publishExportFiles(ctx context.Context, store artifactstore.Store, runID string, files map[string]string) error {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		data := []byte(files[key])
		ref, err := artifactstore.Describe(runID, key, data)
		if err != nil {
			return err
		}
		if err := store.Put(ctx, ref, bytes.NewReader(data)); err != nil {
			return err
		}
	}
	return nil
}

// gcReport lists run-scoped obsolete attempt diagnostics only. GC never
// collects Git worktrees: no authoritative worker lifetime protocol exists that
// would prove a worktree (or its descendants) is dead and owned by this run.
type gcReport struct {
	Files   []string `json:"files"`
	Bytes   int64    `json:"bytes"`
	Applied bool     `json:"applied"`
}

// gcValidateRun proves root is an existing run with a readable checkpoint
// before GC creates any lock file, so a mistyped path has no side effect.
func gcValidateRun(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no irrigator run at %s: %w", root, err)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not an irrigator run directory: %s", root)
	}
	state, err := os.Lstat(filepath.Join(root, "state.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no irrigator run state at %s: %w", root, err)
		}
		return err
	}
	if !state.Mode().IsRegular() {
		return fmt.Errorf("run checkpoint is not a regular file: %s", filepath.Join(root, "state.json"))
	}
	_, err = checkpoint.ReadState(root)
	return err
}

func GarbageCollect(root string, keep int, apply bool) (gcReport, error) {
	report := gcReport{Files: []string{}, Applied: apply}
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return report, err
	}
	if keep < 1 {
		return report, errors.New("keep must be at least 1")
	}
	if err := gcValidateRun(root); err != nil {
		return report, err
	}
	controller, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		return report, errors.New("GC refuses an active controller")
	}
	defer checkpoint.Unlock(controller)
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return report, err
	}
	var leases []*os.File
	defer func() {
		for _, f := range leases {
			checkpoint.Unlock(f)
		}
	}()
	for _, t := range s.AllTasks() {
		f, e := checkpoint.FileLock(filepath.Join(root, "cells", t.ID, "lease"), true)
		if e != nil {
			return report, errors.New("GC refuses active workers")
		}
		leases = append(leases, f)
	}
	// A worker may have committed an expansion while its lease was being acquired.
	// Refuse this pass rather than operate on an incomplete set of leases.
	latest, err := checkpoint.ReadState(root)
	if err != nil {
		return report, err
	}
	if len(latest.Tasks) != len(s.Tasks) {
		return report, errors.New("graph changed while acquiring leases; retry GC")
	}
	for id := range latest.Tasks {
		if s.Tasks[id] == nil {
			return report, errors.New("graph changed while acquiring leases; retry GC")
		}
	}
	s = latest
	if apply {
		if err := checkpoint.ValidateCampaignAuthority(root, s.Plan); err != nil {
			return report, err
		}
	}
	pattern := regexp.MustCompile(`^([0-9]+)(?:\.check-[0-9]+)?\.(?:input\.json|stdout\.json|stderr\.log)$`)
	protectedAttempts := map[string]int{}
	for _, row := range s.ArchivedTasks {
		if row.Attempt > protectedAttempts[row.Spec.ID] {
			protectedAttempts[row.Spec.ID] = row.Attempt
		}
	}
	for id, row := range s.Tasks {
		entries, err := os.ReadDir(filepath.Join(root, "cells", id))
		if err != nil {
			return report, err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			match := pattern.FindStringSubmatch(entry.Name())
			if match == nil {
				continue
			}
			attempt, _ := strconv.Atoi(match[1])
			// Retain diagnostics needed to reconcile uncertain effects of earlier attempts.
			protected := row.Status == "uncertain"
			if s.Execution != nil {
				for _, reservation := range s.Execution.Reservations {
					if reservation.TaskID == id && reservation.Attempt == attempt && reservation.Status != "settled" {
						protected = true
					}
				}
			}
			if protected {
				continue
			}
			// Revision history retains its execution evidence until an explicit
			// revision-retention policy can safely release those references.
			if attempt <= protectedAttempts[id] {
				continue
			}
			if attempt < 1 || attempt > row.Attempt-keep {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return report, err
			}
			report.Bytes += info.Size()
			report.Files = append(report.Files, filepath.Join("cells", id, entry.Name()))
		}
	}
	sort.Strings(report.Files)
	if apply {
		for _, path := range report.Files {
			if err := os.Remove(filepath.Join(root, path)); err != nil && !os.IsNotExist(err) {
				return report, err
			}
		}
		if err := checkpoint.Update(root, func(s *model.State) error { s.Event("gc", "", report); return nil }); err != nil {
			return report, err
		}
	}
	return report, nil
}
