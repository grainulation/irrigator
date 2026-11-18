package delivery

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/textsafe"
)

type summaryTask struct {
	ID             string         `json:"id"`
	Status         string         `json:"status"`
	Included       bool           `json:"included"`
	Attempt        int            `json:"attempt"`
	DeclaredChecks int            `json:"declared_checks"`
	CheckOutcome   string         `json:"check_outcome"`
	Validation     string         `json:"validation_level,omitempty"`
	Checks         []summaryCheck `json:"checks"`
}

type summaryCheck struct {
	Index   int    `json:"index"`
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Basis   string `json:"basis"`
}

var failedCheck = regexp.MustCompile(`^check ([0-9]+) \(([^)]+)\):`)

// Existing validation stops at the first failure. Project only what that
// checkpoint verdict establishes; interrupted/uncertain validation stays unknown.
func projectCheckOutcomes(row *model.TaskState) []summaryCheck {
	checks := []summaryCheck{}
	failureIndex := -1
	if row.Status == "failed" && row.Validation == "task-checks" {
		if m := failedCheck.FindStringSubmatch(row.Error); len(m) == 3 {
			n, err := strconv.Atoi(m[1])
			if err == nil && n < len(row.Spec.Checks) && row.Spec.Checks[n].Kind == m[2] {
				failureIndex = n
			}
		}
	}
	for i, c := range row.Spec.Checks {
		outcome := "unknown"
		basis := "no_individual_receipt"
		if row.Status == "verified" {
			outcome = "passed"
			basis = "accepted_node_contract"
		} else if failureIndex >= 0 {
			basis = "recorded_first_failure"
			switch {
			case i < failureIndex:
				outcome = "passed"
			case i == failureIndex:
				outcome = "failed"
			default:
				outcome = "not_run"
			}
		} else if row.Status == "pending" || row.Status == "running" || (row.Status == "failed" && row.Validation == "structural") {
			outcome = "not_run"
			basis = "validation_not_reached"
		}
		checks = append(checks, summaryCheck{Index: i, Kind: c.Kind, Outcome: outcome, Basis: basis})
	}
	return checks
}

type summaryConflict struct {
	Path  string   `json:"path"`
	Tasks []string `json:"tasks"`
	Kind  string   `json:"kind"`
}

type summary struct {
	Schema          string             `json:"schema"`
	RunID           string             `json:"run_id"`
	PlanSHA256      string             `json:"plan_sha256"`
	GraphRevision   int                `json:"graph_revision"`
	View            graph.DeliveryView `json:"delivery"`
	Tasks           []summaryTask      `json:"tasks"`
	Included        []string           `json:"included"`
	Excluded        []string           `json:"excluded"`
	Conflicts       []summaryConflict  `json:"conflicts"`
	Files           int                `json:"files"`
	CandidateSHA256 *string            `json:"candidate_sha256"`
	Acceptance      string             `json:"acceptance"`
	Issues          []string           `json:"issues"`
	Dispatched      bool               `json:"dispatched"`
}

func readSummaryFile(path string, limit int64) ([]byte, error) {
	f, err := localfs.OpenContextFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("summary source is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("summary source exceeds bound")
	}
	return raw, nil
}

// Summarize projects the recorded candidate and check outcomes without applying,
// running a checker, changing acceptance or starting workers.
func Summarize(root string) (summary, error) {
	var out summary
	root, err := filepath.Abs(root)
	if err != nil {
		return out, err
	}
	raw, err := readSummaryFile(filepath.Join(root, "state.json"), 16<<20)
	if err != nil {
		return out, err
	}
	s, err := checkpoint.DecodeState(root, raw)
	if err != nil {
		return out, err
	}
	out = summary{Schema: "irrigator.delivery-summary/v1", RunID: s.RunID, PlanSHA256: s.PlanHash, GraphRevision: s.GraphRevision, View: graph.ProjectDelivery(root, s), Tasks: []summaryTask{}, Included: []string{}, Excluded: []string{}, Conflicts: []summaryConflict{}, Issues: []string{}}
	out.Acceptance = out.View.Acceptance
	var rec graph.DeliveryRecord
	raw, err = readSummaryFile(filepath.Join(root, "delivery.json"), graph.DeliveryJournalLimit)
	if err == nil {
		if json.Unmarshal(raw, &rec) != nil || rec.RunID != s.RunID {
			return out, errors.New("delivery journal does not match run")
		}
		if graph.DeliveryRevisionBound(rec.Status) && rec.GraphRevision != s.GraphRevision {
			out.Issues = append(out.Issues, "stale_delivery_revision")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	seenNodes := map[string]bool{}
	for _, id := range rec.Nodes {
		if s.Tasks[id] == nil || seenNodes[id] {
			out.Issues = append(out.Issues, "unknown_or_duplicate_delivery_node")
		}
		seenNodes[id] = true
	}
	ids := []string{}
	for id := range s.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	files := map[string]string{}
	owners := map[string][]string{}
	for _, id := range ids {
		row := s.Tasks[id]
		included := slices.Contains(rec.Nodes, id)
		checkOutcome := "not_completed"
		if row.Status == "verified" {
			checkOutcome = "passed_node_contract"
		} else if row.Status == "failed" {
			checkOutcome = "failed_or_not_reached"
		} else if row.Status == "uncertain" {
			checkOutcome = "unknown"
		}
		out.Tasks = append(out.Tasks, summaryTask{ID: id, Status: row.Status, Included: included, Attempt: row.Attempt, DeclaredChecks: len(row.Spec.Checks), CheckOutcome: checkOutcome, Validation: row.Validation, Checks: projectCheckOutcomes(row)})
		if !included {
			out.Excluded = append(out.Excluded, id)
			continue
		}
		out.Included = append(out.Included, id)
		if row.Status != "verified" {
			out.Issues = append(out.Issues, "selected_node_unverified:"+id)
			continue
		}
		mapped, err := artifactstore.DecodeFileMap([]byte(row.Result[rec.Field]))
		if err != nil {
			out.Issues = append(out.Issues, "invalid_file_map:"+id)
			continue
		}
		paths := []string{}
		for path := range mapped {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			clean := filepath.Clean(path)
			if !filepath.IsLocal(path) || path == "." || strings.Contains(path, "\\") || clean == ".swarm-delivery.json" || strings.HasPrefix(clean, ".swarm-delivery.json"+string(filepath.Separator)) {
				out.Issues = append(out.Issues, "unsafe_delivery_path:"+id)
				continue
			}
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(mapped[path])))
			owners[clean] = append(owners[clean], id)
			if prior, ok := files[clean]; ok && prior != digest {
				out.Conflicts = append(out.Conflicts, summaryConflict{Path: textsafe.Terminal(clean), Tasks: append([]string{}, owners[clean]...), Kind: "different_content"})
			}
			files[clean] = digest
		}
	}
	paths := []string{}
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if _, ok := files[parent]; ok {
				tasks := append(append([]string{}, owners[parent]...), owners[path]...)
				sort.Strings(tasks)
				tasks = slices.Compact(tasks)
				out.Conflicts = append(out.Conflicts, summaryConflict{Path: textsafe.Terminal(path), Tasks: tasks, Kind: "file_directory_collision"})
			}
		}
	}
	out.Files = len(files)
	if len(out.Included) > 0 && len(out.Conflicts) == 0 && len(out.Issues) == 0 {
		identity := struct {
			RunID    string
			Plan     string
			Revision int
			Nodes    []string
			Files    map[string]string
		}{s.RunID, s.PlanHash, s.GraphRevision, out.Included, files}
		raw, _ := json.Marshal(identity)
		digest := fmt.Sprintf("%x", sha256.Sum256(raw))
		out.CandidateSHA256 = &digest
	}
	return out, nil
}
