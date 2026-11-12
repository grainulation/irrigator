package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runops"
)

type deliveryManifest struct {
	RunID         string            `json:"run_id"`
	GraphRevision int               `json:"graph_revision,omitempty"`
	Nodes         []string          `json:"nodes"`
	Field         string            `json:"field"`
	SHA256        map[string]string `json:"sha256"`
}

func Deliver(planPath, root, backend, dir, nodes, field, checkJSON string, checkTimeout time.Duration, recheck bool, opts model.Options) (int, error) {
	if root == "" || dir == "" || nodes == "" || field == "" || checkTimeout <= 0 {
		return 2, errors.New("deliver requires --run, --dir, --nodes, a field and positive check timeout")
	}
	var check []string
	if checkJSON != "" {
		if err := json.Unmarshal([]byte(checkJSON), &check); err != nil || len(check) == 0 || check[0] == "" {
			return 2, errors.New("--check must be a nonempty JSON argv array")
		}
	}
	if len(check) > 0 {
		_, lookupErr := exec.LookPath(check[0])
		if lookupErr != nil {
			return 2, fmt.Errorf("acceptance executable: %w", lookupErr)
		}
	}
	ids := strings.Split(nodes, ",")
	sort.Strings(ids)
	for i, id := range ids {
		if id == "" || (i > 0 && id == ids[i-1]) {
			return 2, errors.New("delivery nodes must be nonempty and unique")
		}
	}
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		return 1, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return 1, err
	}
	if rel, e := filepath.Rel(root, dir); e == nil && (rel == "." || filepath.IsLocal(rel)) {
		return 2, errors.New("delivery directory must be outside the durable run")
	}
	p, err := checkpoint.LoadPlan(planPath)
	if err != nil {
		return 1, err
	}
	if _, err = os.Lstat(root); os.IsNotExist(err) {
		if err = checkpoint.Initialize(p, root, backend); err != nil {
			return 1, err
		}
	} else if err != nil {
		return 1, err
	}
	lock, err := checkpoint.FileLock(filepath.Join(root, "delivery.lock"), true)
	if err != nil {
		return 1, err
	}
	defer checkpoint.Unlock(lock)
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return 1, err
	}
	encoded, _ := json.Marshal(p)
	if fmt.Sprintf("%x", sha256.Sum256(encoded)) != s.PlanHash || backend != s.Backend {
		return 1, errors.New("existing run differs from requested plan/backend")
	}
	for _, id := range ids {
		if s.Tasks[id] != nil {
			continue
		}
		rootID := strings.SplitN(id, "__", 2)[0]
		if row := s.Tasks[rootID]; row == nil || row.Spec.Spawn == nil || rootID == id {
			return 2, fmt.Errorf("unknown delivery node %q", id)
		}
	}
	path := filepath.Join(root, "delivery.json")
	record := graph.DeliveryRecord{RunID: s.RunID, Directory: dir, Nodes: ids, Field: field, Check: check, Status: "running", Started: time.Now().UTC()}
	if raw, e := graph.ReadBoundedFile(path, graph.DeliveryJournalLimit); e == nil {
		var prior graph.DeliveryRecord
		if e = json.Unmarshal(raw, &prior); e != nil {
			return 1, e
		}
		if prior.RunID != record.RunID || prior.Directory != dir || !reflect.DeepEqual(prior.Nodes, ids) || prior.Field != field || !reflect.DeepEqual(prior.Check, check) {
			return 1, errors.New("delivery configuration differs from durable record")
		}
		record = prior
		if prior.GraphRevision != s.GraphRevision && (prior.Status == "accepted" || prior.Status == "exported" || prior.Status == "checking" || prior.Status == "check_failed") {
			return 1, errors.New("graph revision differs from prior delivery; prior acceptance cannot be reused")
		}
		if (record.Status == "checking" || record.Status == "check_failed") && !recheck {
			return 1, errors.New("acceptance failed or was interrupted; inspect delivery logs and use --recheck to explicitly rerun it")
		}
	} else if !os.IsNotExist(e) {
		return 1, e
	} else if _, destinationErr := os.Lstat(dir); !os.IsNotExist(destinationErr) {
		return 1, errors.New("new delivery destination must not already exist")
	}
	// An interrupted or failed acceptance attempt is durable evidence. Until an
	// explicit recheck actually starts, pre-check failures must not downgrade it to
	// a status that a later plain deliver would treat as permission to rerun.
	gate := ""
	if record.Status == "checking" || record.Status == "check_failed" {
		gate = record.Status
	}
	revisionBound, boundRevision := graph.DeliveryRevisionBound(record.Status), record.GraphRevision
	// A journaled acceptance is durable evidence. A transient precondition failure
	// (controller lock busy, graph not complete, node no longer verified) must not
	// downgrade it, or a later plain deliver would silently rerun the check.
	entryAccepted := record.Status == "accepted"
	save := func() error {
		record.Updated = time.Now().UTC()
		record.WallMS = time.Since(record.Started).Milliseconds()
		return jsonio.AtomicJSON(path, record)
	}
	fail := func(status string, e error) (int, error) {
		if entryAccepted && status == "graph_failed" {
			return 1, e
		}
		if gate != "" && status != "check_failed" {
			status = gate
		}
		record.Status = status
		record.Error = e.Error()
		if se := save(); se != nil {
			return 1, errors.Join(e, se)
		}
		return 1, e
	}
	if record.Status != "accepted" && record.Status != "exported" && record.Status != "checking" && record.Status != "check_failed" {
		record.Status = "running"
		record.Error = ""
		if err = save(); err != nil {
			return 1, err
		}
		start := time.Now()
		code, runErr := runops.Run(root, opts)
		record.GraphMS += time.Since(start).Milliseconds()
		if runErr != nil {
			return fail("graph_failed", runErr)
		}
		if code != 0 {
			return fail("graph_failed", fmt.Errorf("graph stopped with exit code %d; recover nodes explicitly before delivery", code))
		}
	}
	// Keep execution/revision commits out of the assembly and acceptance window.
	// Delivery's own lock only protects competing deliveries, not resume.
	executionLock, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		return fail("graph_failed", errors.New("controller resumed during delivery; retry after it stops"))
	}
	defer checkpoint.Unlock(executionLock)
	s, err = checkpoint.ReadState(root)
	if err != nil {
		return fail("export_failed", err)
	}
	// Re-check the revision fence under the controller lock: a revision can commit
	// between the first read and this lock. The journal is deliberately untouched.
	if revisionBound && boundRevision != s.GraphRevision {
		return 1, errors.New("graph revision changed during delivery; prior delivery journal preserved and cannot be reused")
	}
	if s.Control != "complete" {
		return fail("graph_failed", fmt.Errorf("graph is %s, not complete; delivery deferred", s.Control))
	}
	for id, row := range s.Tasks {
		if row.Status != "verified" {
			return fail("graph_failed", fmt.Errorf("graph node %s is not verified", id))
		}
	}
	record.GraphRevision = s.GraphRevision
	record.ReportedCost = s.ReportedCost()
	start := time.Now()
	err = assembleDelivery(s, dir, ids, field)
	record.ExportMS += time.Since(start).Milliseconds()
	if err != nil {
		return fail("export_failed", err)
	}
	if record.Status == "accepted" {
		return 0, jsonio.PrintJSON(record)
	}
	record.Error = ""
	if gate == "" {
		record.Status = "exported"
		if err = save(); err != nil {
			return 1, err
		}
	}
	if len(check) > 0 {
		prefix := filepath.Join(root, "delivery-check")
		if raw, e := os.ReadFile(prefix + ".process.json"); e == nil {
			var marker model.ProcessMarker
			if json.Unmarshal(raw, &marker) != nil || marker.Status != "stopped" {
				return fail("check_failed", errors.New("acceptance process cleanup unconfirmed; inspect delivery-check.process.json before rerun"))
			}
		} else if !os.IsNotExist(e) {
			return fail("check_failed", e)
		}
		record.Status = "checking"
		if err = save(); err != nil {
			return 1, err
		}
		gate = ""
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		cmd := exec.Command(check[0], check[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SWARM_DELIVERY_DIR="+dir, "SWARM_RUN_DIR="+root)
		start = time.Now()
		_, err = launch.RunProcess(ctx, cmd, prefix, nil)
		cancel()
		record.CheckMS += time.Since(start).Milliseconds()
		if err != nil {
			return fail("check_failed", err)
		}
	}
	if err = assembleDelivery(s, dir, ids, field); err != nil {
		return fail("check_failed", fmt.Errorf("acceptance modified source artifacts: %w", err))
	}
	record.Status = "accepted"
	if err = save(); err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(record)
}

// Merge disjoint maps without a model integration call. Existing deliveries are
// reused only when their provenance and every source file match exactly.
func assembleDelivery(s *model.State, dir string, ids []string, field string) error {
	files := map[string]string{}
	for _, id := range ids {
		row := s.Tasks[id]
		if row == nil || row.Status != "verified" {
			return fmt.Errorf("delivery node %q is not verified", id)
		}
		mapped, err := artifactstore.DecodeFileMap([]byte(row.Result[field]))
		if err != nil {
			return fmt.Errorf("node %q field %q must contain a nonempty JSON file map", id, field)
		}
		for path, content := range mapped {
			if !filepath.IsLocal(path) || path == "." || strings.Contains(path, "\\") {
				return fmt.Errorf("unsafe delivery path %q", path)
			}
			clean := filepath.Clean(path)
			if clean == ".swarm-delivery.json" || strings.HasPrefix(clean, ".swarm-delivery.json"+string(filepath.Separator)) {
				return errors.New("reserved delivery manifest path")
			}
			if prior, ok := files[clean]; ok && prior != content {
				return fmt.Errorf("conflicting delivery path %q", clean)
			}
			files[clean] = content
		}
	}
	manifest := deliveryManifest{RunID: s.RunID, GraphRevision: s.GraphRevision, Nodes: ids, Field: field, SHA256: map[string]string{}}
	for path, content := range files {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if _, ok := files[parent]; ok {
				return fmt.Errorf("file/directory collision %q", path)
			}
		}
		manifest.SHA256[path] = fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	}
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() {
			return errors.New("delivery destination is not a directory")
		}
		raw, err := os.ReadFile(filepath.Join(dir, ".swarm-delivery.json"))
		if err != nil {
			return err
		}
		var existing deliveryManifest
		if err = json.Unmarshal(raw, &existing); err != nil {
			return err
		}
		if !reflect.DeepEqual(existing, manifest) {
			return errors.New("existing delivery manifest differs")
		}
		for path, expected := range manifest.SHA256 {
			for part := path; part != "."; part = filepath.Dir(part) {
				info, e := os.Lstat(filepath.Join(dir, part))
				if e != nil {
					return e
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return errors.New("delivery contains a symlink")
				}
			}
			raw, err := os.ReadFile(filepath.Join(dir, path))
			if err != nil {
				return err
			}
			if fmt.Sprintf("%x", sha256.Sum256(raw)) != expected {
				return fmt.Errorf("existing delivery file changed: %s", path)
			}
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".swarm-deliver-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for path, content := range files {
		dest := filepath.Join(tmp, path)
		if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		if err = localfs.WriteNew(dest, []byte(content)); err != nil {
			return err
		}
	}
	if err = jsonio.AtomicJSON(filepath.Join(tmp, ".swarm-delivery.json"), manifest); err != nil {
		return err
	}
	var dirs []string
	if err = filepath.WalkDir(tmp, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = syncDeliveryDir(dirs[i]); err != nil {
			return err
		}
	}
	if err = os.Rename(tmp, dir); err != nil {
		return err
	}
	return syncDeliveryDir(filepath.Dir(dir))
}

func syncDeliveryDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
