package graph

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

const (
	DeliveryJournalLimit = 1 << 20
	deliveryMarkerLimit  = 64 << 10
)

type DeliveryView struct {
	Stage      string `json:"stage"`
	Acceptance string `json:"acceptance"`
	Recovery   string `json:"recovery"`
}

func deliveryUnknown() DeliveryView {
	return DeliveryView{Stage: "unknown", Acceptance: "unknown", Recovery: "inspect"}
}

// deliveryRevisionBound reports journal statuses whose evidence is only valid
// for the graph revision recorded with them.
func DeliveryRevisionBound(status string) bool {
	switch status {
	case "accepted", "exported", "checking", "check_failed":
		return true
	}
	return false
}

// readBoundedFile reads a regular file without following a final symlink and
// without blocking on special files. Missing files keep os.ErrNotExist.
func ReadBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds read bound")
	}
	return data, nil
}

// deliveryLockState probes delivery.lock with a brief shared nonblocking flock.
// It never creates the file. The probe can only be wrong in the microsecond a
// deliver starts; deliver then fails closed before changing anything.
func deliveryLockState(root string) (held, known bool) {
	f, err := os.OpenFile(filepath.Join(root, "delivery.lock"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, true
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, true
	}
	return false, false
}

// deliveryCleanupUncertain mirrors deliver's own fail-closed marker rule.
func deliveryCleanupUncertain(root string) bool {
	raw, err := ReadBoundedFile(filepath.Join(root, "delivery-check.process.json"), deliveryMarkerLimit)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	var marker model.ProcessMarker
	return json.Unmarshal(raw, &marker) != nil || marker.Status != "stopped"
}

func deliveryGraphFacts(s *model.State) (uncertain, failed int, verified bool) {
	verified = len(s.Tasks) > 0
	for _, row := range s.Tasks {
		if row == nil {
			verified = false
			continue
		}
		switch row.Status {
		case "uncertain":
			uncertain++
		case "failed", "cancelled":
			failed++
		}
		if row.Status != "verified" {
			verified = false
		}
	}
	return
}

func ProjectDelivery(root string, s *model.State) DeliveryView {
	raw, err := ReadBoundedFile(filepath.Join(root, "delivery.json"), DeliveryJournalLimit)
	if errors.Is(err, os.ErrNotExist) {
		return DeliveryView{Stage: "not_configured", Acceptance: "not_configured", Recovery: "none"}
	}
	if err != nil || s == nil {
		return deliveryUnknown()
	}
	var rec DeliveryRecord
	if json.Unmarshal(raw, &rec) != nil || rec.RunID == "" || rec.RunID != s.RunID {
		return deliveryUnknown()
	}
	checked := len(rec.Check) > 0
	pending := "not_configured"
	if checked {
		pending = "pending"
	}
	if DeliveryRevisionBound(rec.Status) && rec.GraphRevision != s.GraphRevision {
		return DeliveryView{Stage: "stale", Acceptance: "unknown", Recovery: "inspect"}
	}
	uncertain, failed, verified := deliveryGraphFacts(s)
	complete := verified && s.Control == "complete"
	if rec.Status == "running" || rec.Status == "checking" {
		held, known := deliveryLockState(root)
		if !known {
			return deliveryUnknown()
		}
		if held {
			if rec.Status == "checking" {
				return DeliveryView{Stage: "checking", Acceptance: "pending", Recovery: "none"}
			}
			return DeliveryView{Stage: "executing", Acceptance: pending, Recovery: "none"}
		}
	}
	if uncertain > 0 {
		acceptance := pending
		switch rec.Status {
		case "accepted", "checking", "check_failed":
			acceptance = "unknown"
		}
		return DeliveryView{Stage: "blocked", Acceptance: acceptance, Recovery: "reconcile_external"}
	}
	switch rec.Status {
	case "running":
		if failed > 0 {
			return DeliveryView{Stage: "failed", Acceptance: pending, Recovery: "inspect"}
		}
		if !verified {
			// Unverified nodes would dispatch (paid) model work on resume: never local-only.
			return DeliveryView{Stage: "pending", Acceptance: pending, Recovery: "inspect"}
		}
		return DeliveryView{Stage: "pending", Acceptance: pending, Recovery: "resume_local"}
	case "graph_failed":
		if failed > 0 || s.Control == "blocked" || !verified {
			return DeliveryView{Stage: "failed", Acceptance: pending, Recovery: "inspect"}
		}
		return DeliveryView{Stage: "failed", Acceptance: pending, Recovery: "resume_local"}
	case "export_failed":
		return DeliveryView{Stage: "failed", Acceptance: pending, Recovery: "inspect"}
	case "exported":
		if !complete {
			return DeliveryView{Stage: "stale", Acceptance: "unknown", Recovery: "inspect"}
		}
		return DeliveryView{Stage: "exported", Acceptance: pending, Recovery: "resume_local"}
	case "checking":
		if !checked {
			return deliveryUnknown()
		}
		if deliveryCleanupUncertain(root) {
			return DeliveryView{Stage: "blocked", Acceptance: "unknown", Recovery: "inspect"}
		}
		// No delivery lock holder: the checker died mid-run. Its outcome is unknown
		// and is only ever re-run by an explicit --recheck.
		return DeliveryView{Stage: "unknown", Acceptance: "unknown", Recovery: "explicit_recheck"}
	case "check_failed":
		if !checked {
			return deliveryUnknown()
		}
		if deliveryCleanupUncertain(root) {
			return DeliveryView{Stage: "blocked", Acceptance: "unknown", Recovery: "inspect"}
		}
		if !complete {
			return DeliveryView{Stage: "stale", Acceptance: "unknown", Recovery: "inspect"}
		}
		return DeliveryView{Stage: "failed", Acceptance: "failed", Recovery: "explicit_recheck"}
	case "accepted":
		if !complete {
			return DeliveryView{Stage: "stale", Acceptance: "unknown", Recovery: "inspect"}
		}
		if !checked {
			// deliver journals accepted when no acceptance command was configured.
			return DeliveryView{Stage: "accepted", Acceptance: "not_configured", Recovery: "none"}
		}
		return DeliveryView{Stage: "accepted", Acceptance: "passed", Recovery: "none"}
	}
	return deliveryUnknown()
}

// deliveryStatusCLI is an explicitly invoked, read-only helper:
// delivery-status RUN_DIR. Registered by the session lane; it takes no locks
// beyond the brief delivery.lock probe and prints only the three safe enums.
func DeliveryStatusCLI(args []string) (int, error) {
	if err := commandflags.CommandFlagMetadata(flag.NewFlagSet("deliveryStatusCLI", flag.ContinueOnError), args); err != nil {
		return 2, err
	}
	if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return 2, errors.New("usage: delivery-status RUN_DIR")
	}
	root, err := filepath.Abs(args[0])
	if err != nil {
		return 1, errors.New("run directory unavailable")
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return 1, errors.New("run state unreadable")
	}
	return 0, jsonio.PrintJSON(ProjectDelivery(root, s))
}
