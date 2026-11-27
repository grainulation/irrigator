package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

const maxDeliveryRequests = 1024

var deliveryRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// CompletedRequest fences local delivery work to an already completed graph.
// These APIs never initialize a run, resume workers or invoke a model.
type CompletedRequest struct {
	RequestID        string `json:"request_id"`
	ExpectedRunID    string `json:"expected_run_id"`
	ExpectedPlanHash string `json:"expected_plan_sha256"`
	ExpectedRevision int    `json:"expected_graph_revision"`
}

type AssembleRequest struct {
	CompletedRequest
	Directory string   `json:"directory"`
	Nodes     []string `json:"nodes"`
	Field     string   `json:"field"`
}

type CheckRequest struct {
	CompletedRequest
	ExpectedCandidateSHA256 string        `json:"expected_candidate_sha256"`
	Check                   []string      `json:"check"`
	Timeout                 time.Duration `json:"timeout"`
	Recheck                 bool          `json:"recheck"`
}

type CompletedResult struct {
	RunID           string `json:"run_id"`
	RequestID       string `json:"request_id"`
	CandidateSHA256 string `json:"candidate_sha256"`
	Status          string `json:"status"`
	CheckStarted    bool   `json:"check_started"`
	Replayed        bool   `json:"replayed"`
	Dispatched      bool   `json:"dispatched"`
	Error           string `json:"error,omitempty"`
}

func requestDigest(kind string, value any) string {
	raw, _ := json.Marshal(struct {
		Kind  string
		Value any
	}{kind, value})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func validateCompletedRequest(in CompletedRequest) error {
	if !deliveryRequestID.MatchString(in.RequestID) || in.ExpectedRunID == "" || len(in.ExpectedPlanHash) != 64 || in.ExpectedRevision < 0 {
		return errors.New("request_id and the inspected run, plan hash and graph revision are required")
	}
	return nil
}

// completedLocks serializes candidate/check operations with both other
// deliveries and running controllers. A precondition failure writes nothing.
func completedLocks(ctx context.Context, root string, in CompletedRequest) (string, *model.State, func(), error) {
	noop := func() {}
	if err := validateCompletedRequest(in); err != nil {
		return "", nil, noop, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, noop, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", nil, noop, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, noop, err
	}
	if err = checkpoint.RejectQuarantinedRun(root); err != nil {
		return "", nil, noop, err
	}
	lock, err := checkpoint.FileLock(filepath.Join(root, "delivery.lock"), true)
	if err != nil {
		return "", nil, noop, err
	}
	controller, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		checkpoint.Unlock(lock)
		return "", nil, noop, errors.New("stop the controller before local delivery work")
	}
	release := func() { checkpoint.Unlock(controller); checkpoint.Unlock(lock) }
	s, err := checkpoint.ReadState(root)
	if err == nil && (s.RunID != in.ExpectedRunID || s.PlanHash != in.ExpectedPlanHash || s.GraphRevision != in.ExpectedRevision) {
		err = errors.New("run or graph identity changed; inspect before delivery")
	}
	if err == nil && s.Control != "complete" {
		err = errors.New("assembly and checks require an already complete graph; no workers were started")
	}
	if err == nil {
		for _, row := range s.Tasks {
			if row == nil || row.Status != "verified" {
				err = errors.New("every graph cell must already be verified")
				break
			}
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		release()
		return "", nil, noop, err
	}
	return root, s, release, nil
}

func readDelivery(root string) (graph.DeliveryRecord, error) {
	var rec graph.DeliveryRecord
	raw, err := graph.ReadBoundedFile(filepath.Join(root, "delivery.json"), graph.DeliveryJournalLimit)
	if err != nil {
		return rec, err
	}
	err = json.Unmarshal(raw, &rec)
	return rec, err
}

func saveDelivery(root string, rec *graph.DeliveryRecord) error {
	rec.Updated = time.Now().UTC()
	rec.WallMS = time.Since(rec.Started).Milliseconds()
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if len(raw)+1 > graph.DeliveryJournalLimit {
		return errors.New("delivery journal capacity exhausted")
	}
	return jsonio.AtomicJSON(filepath.Join(root, "delivery.json"), rec)
}

func completedResult(rec graph.DeliveryRecord, requestID string, receipt graph.DeliveryRequestReceipt, replayed bool) CompletedResult {
	status := receipt.Status
	if replayed && status == "checking" {
		status = "unknown"
	}
	return CompletedResult{RunID: rec.RunID, RequestID: requestID, CandidateSHA256: receipt.CandidateSHA256, Status: status, CheckStarted: receipt.CheckStarted, Replayed: replayed, Error: receipt.Error}
}

func priorDeliveryRequest(rec graph.DeliveryRecord, id, digest string) (graph.DeliveryRequestReceipt, bool, error) {
	prior, ok := rec.Requests[id]
	if ok && prior.Digest != digest {
		return prior, false, errors.New("request_id already binds different delivery arguments")
	}
	if !ok && len(rec.Requests) >= maxDeliveryRequests {
		return prior, false, errors.New("delivery request capacity exhausted; request IDs cannot be recycled")
	}
	return prior, ok, nil
}

func candidateDigest(s *model.State, ids []string, field string) (string, error) {
	files := map[string]string{}
	for _, id := range ids {
		row := s.Tasks[id]
		if row == nil || row.Status != "verified" {
			return "", fmt.Errorf("delivery node %q is not verified", id)
		}
		mapped, err := artifactstore.DecodeFileMap([]byte(row.Result[field]))
		if err != nil {
			return "", fmt.Errorf("node %q field %q must contain a file map", id, field)
		}
		for path, content := range mapped {
			clean := filepath.Clean(path)
			if !filepath.IsLocal(path) || clean == "." || strings.Contains(path, "\\") || clean == ".swarm-delivery.json" || strings.HasPrefix(clean, ".swarm-delivery.json"+string(filepath.Separator)) {
				return "", errors.New("unsafe candidate path")
			}
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
			if prior, ok := files[clean]; ok && prior != digest {
				return "", errors.New("conflicting candidate file contents")
			}
			files[clean] = digest
		}
	}
	for path := range files {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if _, ok := files[parent]; ok {
				return "", errors.New("candidate file/directory collision")
			}
		}
	}
	identity := struct {
		RunID    string
		Plan     string
		Revision int
		Nodes    []string
		Files    map[string]string
	}{s.RunID, s.PlanHash, s.GraphRevision, ids, files}
	raw, _ := json.Marshal(identity)
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// AssembleCompletedRun only materializes accepted file maps. It does not run
// graph work or a checker, even when the existing journal names one.
func AssembleCompletedRun(ctx context.Context, root string, in AssembleRequest) (CompletedResult, error) {
	var result CompletedResult
	if in.Directory == "" || len(in.Nodes) == 0 || in.Field == "" {
		return result, errors.New("directory, nodes and field are required")
	}
	in.Nodes = slices.Clone(in.Nodes)
	slices.Sort(in.Nodes)
	for i, id := range in.Nodes {
		if id == "" || (i > 0 && id == in.Nodes[i-1]) {
			return result, errors.New("delivery nodes must be nonempty and unique")
		}
	}
	dir, err := filepath.Abs(in.Directory)
	if err != nil {
		return result, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return result, errors.New("delivery parent directory must already exist")
	}
	in.Directory = filepath.Join(parent, filepath.Base(dir))
	root, s, release, err := completedLocks(ctx, root, in.CompletedRequest)
	if err != nil {
		return result, err
	}
	defer release()
	if rel, err := filepath.Rel(root, in.Directory); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return result, errors.New("delivery directory must be outside the durable run")
	}
	rec, err := readDelivery(root)
	if errors.Is(err, os.ErrNotExist) {
		rec = graph.DeliveryRecord{RunID: s.RunID, GraphRevision: s.GraphRevision, Directory: in.Directory, Nodes: in.Nodes, Field: in.Field, Status: "exported", Started: time.Now().UTC(), ReportedCost: s.ReportedCost()}
	} else if err != nil {
		return result, err
	}
	if rec.RunID != s.RunID || rec.GraphRevision != s.GraphRevision || rec.Directory != in.Directory || !reflect.DeepEqual(rec.Nodes, in.Nodes) || rec.Field != in.Field {
		return result, errors.New("delivery configuration or graph differs from durable record")
	}
	digest := requestDigest("assemble", in)
	if prior, ok, err := priorDeliveryRequest(rec, in.RequestID, digest); err != nil {
		return result, err
	} else if ok {
		return completedResult(rec, in.RequestID, prior, true), nil
	}
	candidate, err := candidateDigest(s, in.Nodes, in.Field)
	if err != nil {
		return result, err
	}
	if rec.CandidateSHA256 != "" && rec.CandidateSHA256 != candidate {
		return result, errors.New("candidate identity changed")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	// Existing destinations are validated read-only before any migration or
	// publication. New destinations have already passed pure map/path checks.
	if _, statErr := os.Lstat(in.Directory); statErr == nil {
		if err = assembleDelivery(s, in.Directory, in.Nodes, in.Field); err != nil {
			return result, err
		}
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}
	// Lock order is delivery -> controller -> state. Neither migration nor
	// normal checkpoint publication acquires the outer two locks.
	if err = checkpoint.EnableJournal(root); err != nil {
		return result, err
	}
	if err = assembleDelivery(s, in.Directory, in.Nodes, in.Field); err != nil {
		return result, err
	}
	if rec.Status != "accepted" && rec.Status != "checking" && rec.Status != "check_failed" {
		rec.Status = "exported"
	}
	rec.CandidateSHA256 = candidate
	if rec.Requests == nil {
		rec.Requests = map[string]graph.DeliveryRequestReceipt{}
	}
	receipt := graph.DeliveryRequestReceipt{Digest: digest, Kind: "assemble", Status: rec.Status, CandidateSHA256: candidate}
	rec.Requests[in.RequestID] = receipt
	if err = saveDelivery(root, &rec); err != nil {
		return result, err
	}
	return completedResult(rec, in.RequestID, receipt, false), nil
}

// CheckCompletedDelivery executes only the explicitly supplied local checker.
// Its intent is durable before launch. The same request is never executed a
// second time, including after an interruption or failed receipt publication.
func CheckCompletedDelivery(ctx context.Context, root string, in CheckRequest) (CompletedResult, error) {
	var result CompletedResult
	if len(in.ExpectedCandidateSHA256) != 64 || len(in.Check) == 0 || in.Check[0] == "" || in.Timeout <= 0 || in.Timeout > 24*time.Hour {
		return result, errors.New("candidate digest, checker argv and a positive timeout up to 24h are required")
	}
	root, s, release, err := completedLocks(ctx, root, in.CompletedRequest)
	if err != nil {
		return result, err
	}
	defer release()
	rec, err := readDelivery(root)
	if err != nil {
		return result, err
	}
	if rec.RunID != s.RunID || rec.GraphRevision != s.GraphRevision {
		return result, errors.New("delivery graph identity changed")
	}
	digest := requestDigest("check", in)
	if prior, ok, err := priorDeliveryRequest(rec, in.RequestID, digest); err != nil {
		return result, err
	} else if ok {
		return completedResult(rec, in.RequestID, prior, true), nil
	}
	candidate, err := candidateDigest(s, rec.Nodes, rec.Field)
	if err != nil {
		return result, err
	}
	if candidate != in.ExpectedCandidateSHA256 || rec.CandidateSHA256 != candidate {
		return result, errors.New("candidate digest changed or was not assembled by the completed-run API")
	}
	if rec.Status != "exported" && !in.Recheck {
		return result, errors.New("a new check after acceptance, failure or interruption requires explicit recheck")
	}
	if !filepath.IsAbs(rec.Directory) || filepath.Clean(rec.Directory) != rec.Directory {
		return result, errors.New("candidate directory is not a clean absolute path")
	}
	canonicalDir, pathErr := filepath.EvalSymlinks(rec.Directory)
	if pathErr != nil || canonicalDir != rec.Directory {
		return result, errors.New("candidate directory identity changed through a symlink")
	}
	if rel, err := filepath.Rel(root, rec.Directory); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return result, errors.New("candidate directory must be outside the durable run")
	}
	if info, err := os.Lstat(rec.Directory); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("assembled candidate directory is missing or unsafe")
	}
	if err = assembleDelivery(s, rec.Directory, rec.Nodes, rec.Field); err != nil {
		return result, err
	}
	for _, prefix := range []string{"delivery-check", rec.CheckPrefix} {
		if prefix == "" {
			continue
		}
		if prefix != "delivery-check" && !validCheckPrefix(prefix) {
			return result, errors.New("invalid delivery checker prefix")
		}
		raw, err := graph.ReadBoundedFile(filepath.Join(root, prefix+".process.json"), 64<<10)
		if errors.Is(err, os.ErrNotExist) {
			current := prefix == rec.CheckPrefix || (rec.CheckPrefix == "" && prefix == "delivery-check")
			if current && (rec.Status == "checking" || rec.Status == "check_failed") {
				return result, errors.New("acceptance cleanup is unknown: the prior admitted checker has no process marker")
			}
			continue
		}
		var marker model.ProcessMarker
		if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Status != "stopped" {
			return result, errors.New("acceptance process cleanup unconfirmed; inspect its process marker before explicit recheck")
		}
	}
	if _, err = exec.LookPath(in.Check[0]); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = checkpoint.EnableJournal(root); err != nil {
		return result, err
	}
	if rec.Requests == nil {
		rec.Requests = map[string]graph.DeliveryRequestReceipt{}
	}
	receipt := graph.DeliveryRequestReceipt{Digest: digest, Kind: "check", Status: "checking", CandidateSHA256: candidate, CheckStarted: true}
	rec.Requests[in.RequestID] = receipt
	rec.Status, rec.Check, rec.CheckPrefix, rec.Error = "checking", slices.Clone(in.Check), "delivery-check-"+digest[:32], ""
	if err = saveDelivery(root, &rec); err != nil {
		return result, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, in.Timeout)
	defer cancel()
	cmd := exec.Command(in.Check[0], in.Check[1:]...)
	cmd.Dir = rec.Directory
	cmd.Env = append(os.Environ(), "SWARM_DELIVERY_DIR="+rec.Directory, "SWARM_RUN_DIR="+root)
	start := time.Now()
	_, err = launch.RunProcess(checkCtx, cmd, filepath.Join(root, rec.CheckPrefix), nil)
	rec.CheckMS += time.Since(start).Milliseconds()
	if err == nil {
		err = assembleDelivery(s, rec.Directory, rec.Nodes, rec.Field)
	}
	rec.Status = "accepted"
	if err != nil {
		rec.Status, rec.Error = "check_failed", err.Error()
	}
	receipt.Status = rec.Status
	receipt.Error = rec.Error
	rec.Requests[in.RequestID] = receipt
	if saveErr := saveDelivery(root, &rec); saveErr != nil {
		return result, errors.Join(err, saveErr)
	}
	// A recorded check failure is an inspectable outcome rather than permission
	// to execute again. The result carries its durable failure status.
	return completedResult(rec, in.RequestID, receipt, false), nil
}

func validCheckPrefix(prefix string) bool {
	if !strings.HasPrefix(prefix, "delivery-check-") || len(prefix) != len("delivery-check-")+32 {
		return false
	}
	for _, r := range strings.TrimPrefix(prefix, "delivery-check-") {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
