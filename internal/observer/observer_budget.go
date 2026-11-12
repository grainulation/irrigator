package observer

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

const (
	BudgetCeilingFile     = "budget-ceiling.json"
	BudgetLedgerFile      = "ledger.json"
	budgetCeilingMaxBytes = 4 * 1024
	budgetMaxGeneration   = 4096
)

// providerReceiptAttribution derives generation by matching the request's
// OperationID; anything unverifiable stays unknown (nil).
func ProviderReceiptAttribution(req model.WorkRequest) *model.ProviderAttribution {
	if req.RunID == "" || req.OperationID == "" || req.Task.ID == "" {
		return nil
	}
	for g := 1; g <= budgetMaxGeneration; g++ {
		if model.OperationID(req.RunID, req.Task.ID, g) == req.OperationID {
			return &model.ProviderAttribution{RunID: req.RunID, TaskID: req.Task.ID, Generation: g}
		}
	}
	return nil
}

// providerCeilingRecord persists the service ceiling identity next to ledger.json.
type ProviderCeilingRecord struct {
	Version    int       `json:"version"`
	CeilingUSD float64   `json:"ceiling_usd"`
	Revision   int       `json:"revision"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// recordProviderCeiling is called by ServeProvider under provider.lock. An
// unchanged ceiling is not rewritten; a change increments the revision.
func RecordProviderCeiling(dir string, ceiling float64) error {
	path := filepath.Join(dir, BudgetCeilingFile)
	rev := 1
	if raw, _, code := BudgetReadFile(path, budgetCeilingMaxBytes); code == "" {
		var old ProviderCeilingRecord
		if json.Unmarshal(raw, &old) == nil && old.Version == 1 && old.Revision >= 1 {
			if old.CeilingUSD == ceiling {
				return nil
			}
			rev = old.Revision + 1
		}
	}
	return jsonio.AtomicJSON(path, ProviderCeilingRecord{Version: 1, CeilingUSD: ceiling, Revision: rev, UpdatedAt: time.Now().UTC()})
}

// prepareProviderBudgetIdentity runs at provider startup under provider.lock,
// before the listener opens. It (1) persists the service ceiling and (2) writes
// an explicit initialized-empty ledger.json only when none exists (an existing
// ledger is never touched), so an absent ledger can later be reported UNKNOWN.
// Any error must abort startup: continuing would let a stale budget-ceiling.json
// (old value/revision) be projected against a provider enforcing a different
// budget. An unchanged ceiling is not rewritten, so same-budget restarts do not
// require a successful write. The stale file is deliberately left as-is on failure.
func PrepareProviderBudgetIdentity(dir string, ceiling float64) error {
	if err := RecordProviderCeiling(dir, ceiling); err != nil {
		return fmt.Errorf("provider budget ceiling not recorded; refusing to start with a stale or missing %s: %w", BudgetCeilingFile, err)
	}
	path := filepath.Join(dir, BudgetLedgerFile)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if err := jsonio.AtomicJSON(path, ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}); err != nil {
			return fmt.Errorf("provider empty ledger not initialized: %w", err)
		}
	}
	return nil
}

// budgetReadFile reads a regular non-symlink file with a size cap and a
// torn-read check. Codes: missing, unreadable, not_regular, too_large, changed.
func BudgetReadFile(path string, limit int64) ([]byte, os.FileInfo, string) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, "missing"
		}
		return nil, nil, "unreadable"
	}
	if !info.Mode().IsRegular() {
		return nil, nil, "not_regular"
	}
	if info.Size() > limit {
		return nil, nil, "too_large"
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, "unreadable"
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, "unreadable"
	}
	if !fi.Mode().IsRegular() || !os.SameFile(info, fi) {
		return nil, nil, "changed"
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, "unreadable"
	}
	if int64(len(raw)) > limit {
		return nil, nil, "too_large"
	}
	after, err := f.Stat()
	if err != nil || obsCtxKeyOf(after) != obsCtxKeyOf(fi) {
		return nil, nil, "changed"
	}
	return raw, fi, ""
}
