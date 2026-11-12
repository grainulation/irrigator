package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

func ReconcileBilling(root string, r model.BillingReceipt) error {
	return checkpoint.Update(root, func(s *model.State) error {
		if err := checkpoint.BillingReceiptValid(s, r); err != nil {
			return err
		}
		if prior, ok := s.Execution.BillingReceipts[r.OperationID]; ok {
			if prior == r {
				return nil
			}
			return errors.New("billing receipt is immutable")
		}
		for _, prior := range s.Execution.BillingReceipts {
			if prior.ReceiptID == r.ReceiptID {
				return errors.New("billing receipt already allocated to another operation in this run")
			}
		}
		if err := ReserveBillingAllocation(s, r); err != nil {
			return err
		}
		if s.Execution.BillingReceipts == nil {
			s.Execution.BillingReceipts = map[string]model.BillingReceipt{}
		}
		s.Execution.BillingReceipts[r.OperationID] = r
		used, err := checkpoint.ManagedBudgetUsed(s.Execution)
		if err != nil {
			return err
		}
		if !checkpoint.ManagedTracksCost(s) && (r.AmountMicros > s.Execution.Reservations[r.OperationID].ReservedMicros || used > s.Execution.CeilingMicros) {
			s.Execution.Frozen = true
			s.Execution.FreezeReason = "externally reconciled billing exceeded admission reservation"
		}
		return checkpoint.ValidateBillingReceipts(s)
	})
}
func BillingCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("billing reconcile", flag.ContinueOnError)
	attest := fs.Bool("attest-external-billing", false, "attest the receipt comes from external billing evidence")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 3 || fs.Arg(0) != "reconcile" || !*attest {
		return 2, errors.New("usage: irrigator billing reconcile RUN RECEIPT.json --attest-external-billing")
	}
	raw, err := graph.ReadBoundedFile(fs.Arg(2), 8192)
	if err != nil {
		return 1, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err = jsonio.ParseStrictJSON(dec, 0); err != nil {
		return 1, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return 1, errors.New("trailing billing JSON")
	}
	var r model.BillingReceipt
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&r); err != nil {
		return 1, err
	}
	// Reject aliases accepted by encoding/json and demand exact field spelling.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	canonical, _ := json.Marshal(r)
	var expected map[string]json.RawMessage
	_ = json.Unmarshal(canonical, &expected)
	if len(fields) != len(expected) {
		return 1, errors.New("every billing field must be explicitly supplied")
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return 1, errors.New("null billing field forbidden")
		}
		if _, ok := expected[key]; !ok {
			return 1, fmt.Errorf("unknown billing field %s", key)
		}
	}
	if err = ReconcileBilling(fs.Arg(1), r); err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(r)
}
