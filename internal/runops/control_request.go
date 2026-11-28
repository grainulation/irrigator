package runops

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

const maxControlRequests = 65536

var controlRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ControlRequest uses durable identity rather than a transport request number.
// Stop requests cancel active work cooperatively; they never kill persisted PIDs
// or claim cleanup is complete. Cancellation includes descendants of Cell.
type ControlRequest struct {
	RequestID        string `json:"request_id"`
	ExpectedRunID    string `json:"expected_run_id"`
	ExpectedPlanHash string `json:"expected_plan_sha256"`
	ExpectedRevision int    `json:"expected_graph_revision"`
	Cell             string `json:"cell,omitempty"`
	OperationID      string `json:"expected_operation_id,omitempty"`
}

// RequestControl atomically records either cancel_cell or stop_run. A repeated
// request returns its original receipt without another publication or event.
// Reusing its ID with different arguments fails even after the graph advances.
func RequestControl(ctx context.Context, root, kind string, in ControlRequest) (model.ControlRequestReceipt, error) {
	var receipt model.ControlRequestReceipt
	if !controlRequestID.MatchString(in.RequestID) || in.ExpectedRunID == "" || len(in.ExpectedPlanHash) != 64 || in.ExpectedRevision < 0 {
		return receipt, errors.New("request_id and the inspected run, plan hash and graph revision are required")
	}
	if kind != "cancel_cell" && kind != "stop_run" {
		return receipt, errors.New("unsupported control request")
	}
	if kind == "cancel_cell" && (in.Cell == "" || in.OperationID == "") {
		return receipt, errors.New("cell and expected_operation_id are required")
	}
	if kind == "stop_run" && (in.Cell != "" || in.OperationID != "") {
		return receipt, errors.New("stop_run does not accept a cell operation")
	}
	raw, _ := json.Marshal(struct {
		Kind string
		ControlRequest
	}{kind, in})
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	preflight, err := checkpoint.ReadState(root)
	if err != nil {
		return receipt, err
	}
	if prior, ok := preflight.ControlRequests[in.RequestID]; ok {
		if prior.Digest != digest {
			return receipt, errors.New("request_id already binds different control arguments")
		}
	} else {
		if preflight.RunID != in.ExpectedRunID || preflight.PlanHash != in.ExpectedPlanHash || preflight.GraphRevision != in.ExpectedRevision {
			return receipt, errors.New("run or graph identity changed; inspect before requesting control")
		}
		if len(preflight.ControlRequests) >= maxControlRequests {
			return receipt, errors.New("control request receipt capacity exhausted")
		}
		if kind == "cancel_cell" {
			row := preflight.Tasks[in.Cell]
			if row == nil || row.OperationID != in.OperationID {
				return receipt, errors.New("cell operation changed; inspect before cancelling")
			}
		}
	}
	// Migration is authorized only after request validation. It takes the normal
	// state/authority locks itself; the transaction below rechecks all fences.
	// Its schema-3 marker prevents older writers dropping request receipts.
	if err := checkpoint.EnableJournal(root); err != nil {
		return receipt, err
	}
	err = (checkpoint.FileCheckpointStore{Root: root}).Update(ctx, checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error {
		if prior, ok := s.ControlRequests[in.RequestID]; ok {
			if prior.Digest != digest {
				return errors.New("request_id already binds different control arguments")
			}
			receipt = prior
			return checkpoint.ErrUnchanged
		}
		if s.RunID != in.ExpectedRunID || s.PlanHash != in.ExpectedPlanHash || s.GraphRevision != in.ExpectedRevision {
			return errors.New("run or graph identity changed; inspect it before submitting a new request")
		}
		if len(s.ControlRequests) >= maxControlRequests {
			return errors.New("control request receipt capacity exhausted; request IDs cannot be recycled")
		}
		receipt = model.ControlRequestReceipt{Digest: digest, Kind: kind, Cell: in.Cell}
		if kind == "cancel_cell" {
			row := s.Tasks[in.Cell]
			if row == nil || row.OperationID != in.OperationID {
				return errors.New("cell operation changed; inspect it before cancelling")
			}
			var err error
			receipt.Changed, err = cancelNodes(s, in.Cell, map[string]string{"request_id": in.RequestID})
			if err != nil {
				return err
			}
		} else if s.Control != "down" && s.Control != "complete" {
			s.Control = "down"
			s.Event("down_requested", "", map[string]string{"request_id": in.RequestID})
			receipt.Changed = true
		}
		if s.ControlRequests == nil {
			s.ControlRequests = map[string]model.ControlRequestReceipt{}
		}
		s.ControlRequests[in.RequestID] = receipt
		return nil
	})
	return receipt, err
}

func cancelNodes(s *model.State, id string, detail any) (bool, error) {
	if s.Tasks[id] == nil {
		return false, errors.New("unknown node")
	}
	changed := false
	for _, node := range managed.Descendants(s, id) {
		r := s.Tasks[node]
		if r.Status == "verified" || (r.CancelRequested && (r.Status == "running" || r.Status == "uncertain" || r.Status == "cancelled")) {
			continue
		}
		r.CancelRequested = true
		if r.Status != "running" && r.Status != "uncertain" {
			r.Status = "cancelled"
		}
		s.Event("cancel_requested", node, detail)
		changed = true
	}
	return changed, nil
}
