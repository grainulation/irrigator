package checkpoint

import (
	"errors"
	"strings"

	"github.com/grainulation/irrigator/internal/model"
)

func BillingReceiptValid(s *model.State, r model.BillingReceipt) error {
	if s.Version != 5 || s.Execution == nil {
		return errors.New("billing reconciliation requires checkpoint5")
	}
	if r.Schema != "irrigator.billing/v1" || r.RunID != s.RunID || r.Currency != "USD" || r.AmountMicros < 0 || r.Attestation != "operator_verified_external_billing" {
		return errors.New("invalid billing identity, currency, amount or attestation")
	}
	if len(r.ReceiptID) < 1 || len(r.ReceiptID) > 256 || len(r.EvidenceReference) < 1 || len(r.EvidenceReference) > 1024 || strings.TrimSpace(r.EvidenceReference) != r.EvidenceReference {
		return errors.New("bounded receipt identity and evidence reference required")
	}
	if len(r.EvidenceSHA256) != 64 || strings.Trim(r.EvidenceSHA256, "0123456789abcdef") != "" {
		return errors.New("external billing evidence SHA256 required")
	}
	held, ok := s.Execution.Reservations[r.OperationID]
	if !ok || held.TaskID != r.TaskID || held.Attempt != r.Attempt || held.BindingDigest != r.BindingDigest {
		return errors.New("billing receipt fence mismatch")
	}
	if held.Status != "unknown_cost" && held.Status != "uncertain" {
		return errors.New("billing receipt requires unresolved cost; provider reports are immutable")
	}
	return nil
}
func ValidateBillingReceipts(s *model.State) error {
	if s.Execution == nil {
		return nil
	}
	seen := map[string]bool{}
	for op, r := range s.Execution.BillingReceipts {
		if op != r.OperationID || seen[r.ReceiptID] {
			return errors.New("duplicate billing receipt or operation mismatch")
		}
		if err := BillingReceiptValid(s, r); err != nil {
			return err
		}
		seen[r.ReceiptID] = true
		if !ManagedTracksCost(s) && r.AmountMicros > s.Execution.Reservations[op].ReservedMicros && !s.Execution.Frozen {
			return errors.New("billing overrun must freeze admission")
		}
	}
	return nil
}
func ManagedAccountedAmount(e *model.ManagedExecutionState, r model.ManagedReservation) (int64, bool) {
	if receipt, ok := e.BillingReceipts[r.OperationID]; ok {
		// Billing never resolves uncertainty about execution or cleanup.
		amount := receipt.AmountMicros
		if r.ReportedMicros > amount {
			amount = r.ReportedMicros
		}
		return amount, r.Status == "unknown_cost"
	}
	return r.ReportedMicros, r.Status == "settled"
}
