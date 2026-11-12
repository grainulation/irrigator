package executor

import (
	"errors"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func TestCellAdmissionRunningExternalRequiresReconciliation(t *testing.T) {
	for _, provider := range []string{"haiku", "custom"} {
		t.Run(provider, func(t *testing.T) {
			s := cellTestState()
			s.Backend = provider
			row := s.Tasks["cell"]
			row.Status, row.Attempt = "running", 1
			before := cellBytes(t, s)
			_, err := beginCellAttempt(s, "cell", row.OperationID, 123, time.Unix(1, 0))
			if !errors.Is(err, launch.ErrNoWork) || cellBytes(t, s) != before {
				t.Fatalf("unreconciled external work admitted or mutated: %v", err)
			}
		})
	}
}

func TestCellAdmissionRunningReplaySafeProviderStillRecovers(t *testing.T) {
	for _, provider := range []string{"mock", "custom"} {
		t.Run(provider, func(t *testing.T) {
			s := cellTestState()
			s.Backend = provider
			s.Plan.Providers = map[string]model.CommandSpec{"custom": {Replay: "safe"}}
			// Replay capability does not authorize unbudgeted paid execution.
			// This fixture tests recovery after explicit budget admission.
			s.CallLimits = &model.SavedCallLimits{Timeout: time.Minute, Budget: .25}
			row := s.Tasks["cell"]
			row.Status, row.Attempt = "running", 1
			beforeOperation := row.OperationID
			_, err := beginCellAttempt(s, "cell", row.OperationID, 123, time.Unix(1, 0))
			if err != nil || row.Attempt != 2 || row.OperationID != beforeOperation {
				t.Fatalf("replay-safe recovery changed: %v %+v", err, row)
			}
		})
	}
}
