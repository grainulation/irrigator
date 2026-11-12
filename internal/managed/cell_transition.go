package managed

import (
	"errors"

	"github.com/grainulation/irrigator/internal/model"
)

// cellFence identifies the exact execution whose evidence may change a cell.
// It is local admission metadata, never identity taken from model-authored output.
type CellFence struct {
	RunID, TaskID, OperationID string
	BindingDigest              string
	Attempt                    int
}

var ErrStaleCellAttempt = errors.New("stale worker attempt rejected")

func (f CellFence) Current(s *model.State) (*model.TaskState, error) {
	row := s.Tasks[f.TaskID]
	if s.RunID != f.RunID || row == nil || row.OperationID != f.OperationID || row.Attempt != f.Attempt || row.BindingDigest != f.BindingDigest {
		return nil, ErrStaleCellAttempt
	}
	return row, nil
}

type CellOutcome struct {
	Fence             CellFence
	Result            map[string]string
	CostUnknown       bool
	Cost              float64
	CostBasis         string
	Usage             *model.TaskUsage
	Model             string
	Err               error
	Diagnostic        string
	CleanupUnresolved bool
	CleanupError      error
}
