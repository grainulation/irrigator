package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
)

// WorkResponse.Outcome is an OPTIONAL typed certainty about whether the
// provider may have performed external effects:
//
//	"not_started"  nothing was dispatched; a known failure (must carry Error).
//	"settled"      the provider completed; Error, if any, is a known failure.
//	"uncertain"    dispatch may have happened; the outcome is unknown. The worker
//	               commits status uncertain, never retries it, never releases
//	               its reservation, and reconcile/explicit authorization is needed.
//
// An absent value is unclassified: the existing error and Replay policy
// apply unchanged. Any other value fails closed as uncertain.
const (
	OutcomeNotStarted = "not_started"
	OutcomeSettled    = "settled"
	OutcomeUncertain  = "uncertain"
)

func ParseOutcome(v string) (string, error) {
	switch v {
	case "", OutcomeNotStarted, OutcomeSettled, OutcomeUncertain:
		return v, nil
	}
	// The raw value is never echoed.
	return "", errors.New("invalid provider outcome certainty")
}

// outcomeError carries explicit (or conservatively inferred) certainty through
// ordinary error returns. Error() is the underlying text, so behavior is
// unchanged for callers that do not inspect certainty.
type OutcomeError struct {
	Outcome string
	Reason  string
	Err     error
}

func (e *OutcomeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "provider outcome " + e.Outcome
}
func (e *OutcomeError) Unwrap() error { return e.Err }

// notStartedError marks a failure that happened before the command started.
type notStartedError struct{ err error }

func (e *notStartedError) Error() string { return e.err.Error() }
func (e *notStartedError) Unwrap() error { return e.err }

func MarkNotStarted(err error) error {
	if err == nil || isNotStarted(err) {
		return err
	}
	return &notStartedError{err: err}
}
func isNotStarted(err error) bool {
	var n *notStartedError
	return errors.As(err, &n)
}

// knownNotStarted recognizes positive pre-dispatch evidence only.
func KnownNotStarted(err error) bool {
	var oe *OutcomeError
	if errors.As(err, &oe) {
		return oe.Outcome == OutcomeNotStarted
	}
	return isNotStarted(err)
}

func IsUncertain(err error) bool {
	var oe *OutcomeError
	return errors.As(err, &oe) && oe.Outcome == OutcomeUncertain
}
func OutcomeReason(err error) string {
	var oe *OutcomeError
	if errors.As(err, &oe) {
		return oe.Reason
	}
	return ""
}

// withOutcome attaches explicit certainty to an error; absent certainty is unclassified.
func WithOutcome(certainty string, err error) error {
	if certainty == "" || err == nil {
		return err
	}
	reason := ""
	if certainty == OutcomeUncertain {
		reason = "provider_reported"
	}
	return &OutcomeError{Outcome: certainty, Reason: reason, Err: err}
}

// successOutcomeError rejects explicit certainties that cannot accompany success.
func SuccessOutcomeError(certainty string) error {
	switch certainty {
	case OutcomeUncertain:
		return &OutcomeError{Outcome: OutcomeUncertain, Reason: "provider_reported", Err: errors.New("provider reported uncertain outcome")}
	case OutcomeNotStarted:
		return &OutcomeError{Outcome: OutcomeUncertain, Reason: "invalid_outcome", Err: errors.New("provider reported not_started without an error")}
	}
	return nil
}

// transportOutcome: a task deadline or cancellation after the command started
// cannot prove the effect did not happen, even for Replay=safe. The worker resets
// cancelled/interrupted work to pending and an explicit retry changes the
// operation generation, so a sticky operation ID is not a sufficient guard.
// Only a failure positively marked not-started stays an ordinary failure.
// Validators are read-only checks and keep ordinary failure semantics.
func TransportOutcome(req WorkRequest, err error) error {
	if req.Kind != "task" || isNotStarted(err) {
		return err
	}
	reason := ""
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "deadline"
	case errors.Is(err, context.Canceled):
		reason = "cancelled"
	default:
		return err
	}
	return &OutcomeError{Outcome: OutcomeUncertain, Reason: reason, Err: err}
}

// malformedOutcome: a response that cannot be decoded or trusted may be a lost
// answer to a dispatched task. An empty stdout from a process that failed after
// starting (for example exit 1) proves nothing about external effects, so it is
// uncertain too. Only a command positively marked not-started, or a read-only
// validator, keeps the plain failure.
func MalformedOutcome(req WorkRequest, raw []byte, processErr, err error) error {
	if isNotStarted(processErr) {
		return MarkNotStarted(err)
	}
	if req.Kind != "task" {
		return err
	}
	return &OutcomeError{Outcome: OutcomeUncertain, Reason: "malformed_response", Err: err}
}

// uncertainOutcomeMessage is fixed text: no raw provider error, prompt or secret.
func UncertainOutcomeMessage(err error) string {
	reason := OutcomeReason(err)
	if reason == "" {
		reason = "unspecified"
	}
	return "provider outcome uncertain (" + reason + "); reconcile external outcome before explicit replay"
}

// Provider pool helpers.
func FinishOutcome(current string, known bool) string {
	if !known {
		return OutcomeUncertain
	}
	if current == "" {
		return OutcomeSettled
	}
	return current
}
func OutcomeResponse(response WorkResponse, outcome string) WorkResponse {
	response.Outcome = outcome
	return response
}

// A payload conflict dispatches nothing, but an unresolved old receipt stays uncertain.
func WithReceiptOutcome(response WorkResponse, old ProviderReceipt) WorkResponse {
	response.Outcome = OutcomeUncertain
	if old.Status == "completed" {
		response.Outcome = OutcomeNotStarted
	}
	return response
}

// connectNeverSent: only a failed dial proves the request never reached the pool.
func ConnectNeverSent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// lostProviderResponse reports a response lost after the request may have been
// delivered as an explicit uncertain response rather than a bare transport error.
func LostProviderResponse(body []byte, output io.Writer) error {
	var identity struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(body, &identity)
	encoded, _ := json.Marshal(WorkResponse{Protocol: ProtocolVersion, RequestID: identity.RequestID, Error: "provider response lost after dispatch; operation uncertain", Outcome: OutcomeUncertain, CostUnknown: true, CostBasis: "unknown"})
	_, err := output.Write(append(encoded, '\n'))
	return err
}

// Read-only recovery diagnosis for waiting owners. Status and explicit evidence
// only: never Replay=safe, never error text. Suggestions are descriptive.
const (
	RecoveryUncertain = "uncertain_reconcile_externally"
	RecoveryFailed    = "failed_review_before_retry"
	RecoveryCancelled = "cancelled_operator_decision"
	RecoveryActive    = "active_work_remaining"
	RecoveryNoActive  = "no_active_work"
)

type LifecycleRecovery struct {
	Classification  string   `json:"classification"`
	Advice          string   `json:"advice"`
	BlockerIDs      []string `json:"blocker_ids"`
	BlockersTotal   int      `json:"blockers_total"`
	BlockersOmitted int      `json:"blockers_omitted"`
	UncertainTotal  int      `json:"uncertain_total"`
	FailedTotal     int      `json:"failed_total"`
	CancelledTotal  int      `json:"cancelled_total"`
	Activity        string   `json:"activity"`
	ActiveIDs       []string `json:"active_descendant_ids"`
	ActiveTotal     int      `json:"active_descendants_total"`
	ActiveOmitted   int      `json:"active_descendants_omitted"`
}

func LifecycleRecoveryFor(s *State, row *TaskState) *LifecycleRecovery {
	if s == nil || row == nil || row.Status != "waiting" {
		return nil
	}
	seen := map[string]bool{}
	if row.Spec.ID != "" {
		seen[row.Spec.ID] = true
	}
	stack := append([]string{}, row.Children...)
	blockers, active := []string{}, []string{}
	uncertain, failed, cancelled := 0, 0, 0
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		child := s.Tasks[id]
		if child == nil {
			continue
		}
		switch child.Status {
		case "uncertain":
			uncertain++
			blockers = append(blockers, id)
		case "failed":
			failed++
			blockers = append(blockers, id)
		case "cancelled":
			cancelled++
			blockers = append(blockers, id)
		case "pending", "running", "produced":
			active = append(active, id)
		}
		stack = append(stack, child.Children...)
	}
	if len(blockers) == 0 {
		return nil
	}
	rec := &LifecycleRecovery{UncertainTotal: uncertain, FailedTotal: failed, CancelledTotal: cancelled, Activity: RecoveryNoActive}
	switch {
	case uncertain > 0:
		rec.Classification = RecoveryUncertain
		rec.Advice = "reconcile the external outcome before any explicitly authorized replay; no automatic retry"
	case failed > 0:
		rec.Classification = RecoveryFailed
		rec.Advice = "failed status alone does not establish external effects; review before an explicit retry"
	default:
		rec.Classification = RecoveryCancelled
		rec.Advice = "cancelled by request; decide explicitly whether to retry or revise"
	}
	if len(active) > 0 {
		rec.Activity = RecoveryActive
	}
	rec.BlockerIDs, rec.BlockersTotal, rec.BlockersOmitted = LifecycleCap(blockers)
	rec.ActiveIDs, rec.ActiveTotal, rec.ActiveOmitted = LifecycleCap(active)
	return rec
}
