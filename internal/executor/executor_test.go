package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

type serializedAttemptExecutor struct {
	calls  int
	change func(*attemptExecutionReceipt)
	before func()
	lost   bool
	seen   attemptExecutionRequest
}

func (e *serializedAttemptExecutor) execute(ctx context.Context, r attemptExecutionRequest) (attemptExecutionReceipt, error) {
	e.calls++
	// This peer derives the result entirely from serialized transmitted payload.
	raw, err := json.Marshal(r)
	if err != nil {
		return attemptExecutionReceipt{}, err
	}
	var peer attemptExecutionRequest
	if err = json.Unmarshal(raw, &peer); err != nil {
		return attemptExecutionReceipt{}, err
	}
	e.seen = peer
	if err = validateAttemptRequest(peer); err != nil {
		return attemptExecutionReceipt{}, err
	}
	if peer.Work.LaunchContext != nil {
		return attemptExecutionReceipt{}, errors.New("remote host capsule unsupported")
	}
	if e.before != nil {
		e.before()
	}
	if e.lost {
		return attemptExecutionReceipt{}, errors.New("reply lost after remote dispatch")
	}
	output := map[string]string{}
	for _, key := range peer.Work.Task.Requires {
		output[key] = peer.Work.Task.Prompt
	}
	response := attemptExecutionReceipt{Version: attemptExecutorVersion, Identity: peer.Identity, RequestDigest: peer.Digest, Output: output, Outcome: model.OutcomeSettled}
	if e.change != nil {
		e.change(&response)
	}
	raw, err = json.Marshal(response)
	if err != nil {
		return attemptExecutionReceipt{}, err
	}
	var received attemptExecutionReceipt
	err = json.Unmarshal(raw, &received)
	return received, err
}
func runAlternateAttempt(t *testing.T, root, id string, e *serializedAttemptExecutor) error {
	t.Helper()
	return workerOperationWithExecutor(root, id, model.Options{Timeout: time.Second, Budget: .25}, "", func(localAttemptExecutor) attemptExecutor { return e })
}
func TestExecutorSerializedPeerUsesRealWorkerAcceptanceOnce(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	e := &serializedAttemptExecutor{}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "verified" || s.Tasks["a"].Result["answer"] != e.seen.Work.Task.Prompt {
		t.Fatal("alternate executor was not accepted through worker")
	}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	if e.calls != 1 {
		t.Fatal("duplicate executor dispatch")
	}
	if e.seen.Identity.RunID != s.RunID || e.seen.Identity.OperationID != s.Tasks["a"].OperationID || e.seen.Identity.Route != "mock" {
		t.Fatal("missing scoped identity")
	}
}
func TestExecutorMismatchesAndLostRepliesStayUncertain(t *testing.T) {
	for _, name := range []string{"run", "task", "operation", "attempt", "binding", "request", "route", "digest", "oversize", "lost"} {
		t.Run(name, func(t *testing.T) {
			root := runfixture.New(t, runfixture.Task("a", 0))
			if err := checkpoint.SetControl(root, "run"); err != nil {
				t.Fatal(err)
			}
			e := &serializedAttemptExecutor{lost: name == "lost", change: func(r *attemptExecutionReceipt) {
				switch name {
				case "run":
					r.Identity.RunID = "other"
				case "task":
					r.Identity.TaskID = "other"
				case "operation":
					r.Identity.OperationID = "other"
				case "attempt":
					r.Identity.Attempt++
				case "binding":
					r.Identity.BindingDigest = "other"
				case "request":
					r.Identity.RequestID = "other"
				case "route":
					r.Identity.Route = "other"
				case "digest":
					r.RequestDigest = "other"
				case "oversize":
					r.Output["answer"] = strings.Repeat("x", model.MaxOutputBytes)
				}
			}}
			if err := runAlternateAttempt(t, root, "a", e); err != nil {
				t.Fatal(err)
			}
			s := runfixture.Read(t, root)
			if s.Tasks["a"].Status != "uncertain" || len(s.Tasks["a"].Result) != 0 {
				t.Fatal("invalid evidence accepted")
			}
			if err := runAlternateAttempt(t, root, "a", e); err != nil {
				t.Fatal(err)
			}
			if e.calls != 1 {
				t.Fatal("uncertain execution automatically replayed")
			}
		})
	}
}
func TestExecutorUnknownCostAndUsageSurviveSerialization(t *testing.T) {
	root := runfixture.UnknownCostFixture(t)
	var usage *model.TaskUsage
	e := &serializedAttemptExecutor{change: func(r *attemptExecutionReceipt) {
		r.CostUnknown = true
		r.Usage = &model.TaskUsage{InputTokens: 17, OutputTokens: 3, TotalTokens: 20}
		raw, _ := json.Marshal(r)
		var round attemptExecutionReceipt
		if err := json.Unmarshal(raw, &round); err != nil {
			t.Fatal(err)
		}
		usage = round.Usage
	}}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	row := s.Tasks["a"]
	if row.Status != "verified" || !row.CostUnknown || s.Execution.Reservations[row.OperationID].Status != "unknown_cost" {
		t.Fatalf("unknown cost discarded: %+v", row)
	}
	if usage == nil || usage.InputTokens != 17 || usage.OutputTokens != 3 {
		t.Fatal("usage disappeared in receipt JSON")
	}
}
func TestExecutorStaleAttemptCannotCommit(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	e := &serializedAttemptExecutor{before: func() {
		if err := checkpoint.Update(root, func(s *model.State) error {
			r := s.Tasks["a"]
			r.Generation++
			r.OperationID = model.OperationID(s.RunID, "a", r.Generation)
			r.Status = "pending"
			r.Attempt = 0
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}}
	if err := runAlternateAttempt(t, root, "a", e); !errors.Is(err, managed.ErrStaleCellAttempt) {
		t.Fatalf("stale acceptance: %v", err)
	}
	row := runfixture.Read(t, root).Tasks["a"]
	if row.Status != "pending" || len(row.Result) != 0 || row.Attempt != 0 {
		t.Fatal("stale result changed replacement")
	}
}

func TestExecutorOversizedOutputRetainsMatchingObservedCharge(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	e := &serializedAttemptExecutor{change: func(r *attemptExecutionReceipt) {
		r.CostUSD = .30
		r.Output["answer"] = strings.Repeat("x", model.MaxOutputBytes)
	}}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	row := runfixture.Read(t, root).Tasks["a"]
	if row.Status != "uncertain" || row.Cost != .30 || len(row.Result) != 0 {
		t.Fatalf("oversize erased observed charge: %+v", row)
	}
}

func TestExecutorLostReplyRetainsManagedReservation(t *testing.T) {
	root := runfixture.UnknownCostFixture(t)
	e := &serializedAttemptExecutor{lost: true}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	row := s.Tasks["a"]
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || row.Status != "uncertain" || !row.CostUnknown || used != 250000 {
		t.Fatalf("lost response released reservation: status=%s used=%d err=%v", row.Status, used, err)
	}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	if e.calls != 1 {
		t.Fatal("lost response automatically replayed")
	}
}

func TestExecutorAlternateLegacyCancellationDoesNotReplay(t *testing.T) {
	for _, outcome := range []string{"legacy", model.OutcomeSettled} {
		t.Run(outcome, func(t *testing.T) {
			root := runfixture.New(t, runfixture.Task("a", 0))
			if err := checkpoint.SetControl(root, "run"); err != nil {
				t.Fatal(err)
			}
			e := &serializedAttemptExecutor{change: func(r *attemptExecutionReceipt) {
				r.Output = nil
				r.Error = "remote interrupted"
				r.ErrorKind = "cancelled"
				r.Outcome = outcome
			}}
			if err := runAlternateAttempt(t, root, "a", e); err != nil {
				t.Fatal(err)
			}
			row := runfixture.Read(t, root).Tasks["a"]
			want := "uncertain"
			if outcome == model.OutcomeSettled {
				want = "failed"
			}
			if row.Status != want {
				t.Fatalf("remote cancellation authorized replay: %s", row.Status)
			}
			if err := runAlternateAttempt(t, root, "a", e); err != nil {
				t.Fatal(err)
			}
			if e.calls != 1 {
				t.Fatal("remote error automatically replayed")
			}
		})
	}
}

func TestExecutorMismatchedManagedReceiptKeepsUnknownHold(t *testing.T) {
	root := runfixture.UnknownCostFixture(t)
	e := &serializedAttemptExecutor{change: func(r *attemptExecutionReceipt) { r.Identity.RunID = "different-run"; r.CostUSD = .01 }}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	row := s.Tasks["a"]
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || row.Status != "uncertain" || !row.CostUnknown || row.Cost != 0 || used != 250000 {
		t.Fatalf("foreign receipt attributed or lost hold: row=%+v used=%d err=%v", row, used, err)
	}
}

func TestExecutorUncertaintyReasonsSurviveSafeRoundTrip(t *testing.T) {
	for _, reason := range []string{"deadline", "cancelled", "malformed_response", "invalid_outcome", "provider_reported", ""} {
		original := &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: reason, Err: errors.New("SECRET-PROVIDER-ERROR")}
		receipt := executionReceipt(attemptExecutionRequest{}, model.WorkResponse{}, original)
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var decoded attemptExecutionReceipt
		if err = json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		restored := receiptError(decoded)
		if model.OutcomeReason(restored) != reason || model.UncertainOutcomeMessage(restored) != model.UncertainOutcomeMessage(original) {
			t.Fatalf("uncertainty classification changed: %q -> %q", reason, model.OutcomeReason(restored))
		}
		if strings.Contains(model.UncertainOutcomeMessage(restored), "SECRET") {
			t.Fatal("raw provider error reached public diagnostic")
		}
	}
	receipt := executionReceipt(attemptExecutionRequest{}, model.WorkResponse{}, &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "SECRET-UNTRUSTED-REASON", Err: errors.New("hidden")})
	if receipt.OutcomeReason != "" || strings.Contains(model.UncertainOutcomeMessage(receiptError(receipt)), "SECRET") {
		t.Fatal("unknown reason leaked")
	}
}

func TestExecutorRejectsUntrustedPortableOutcomeReason(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	e := &serializedAttemptExecutor{change: func(r *attemptExecutionReceipt) {
		r.Output = nil
		r.Outcome = model.OutcomeUncertain
		r.Error = "SECRET-REMOTE-ERROR"
		r.OutcomeReason = "SECRET-REMOTE-REASON"
	}}
	if err := runAlternateAttempt(t, root, "a", e); err != nil {
		t.Fatal(err)
	}
	row := runfixture.Read(t, root).Tasks["a"]
	if row.Status != "uncertain" || strings.Contains(row.Error, "SECRET") {
		t.Fatalf("unsafe outcome reason committed: %+v", row)
	}
}
