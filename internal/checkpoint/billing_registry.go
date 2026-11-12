package checkpoint

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func BillingAllocationName(r model.BillingReceipt) string {
	return launch.ExecutionDigest(struct{ ReceiptID string }{r.ReceiptID}) + ".json"
}

func ValidateBillingAuthority(s *model.State) error {
	if s.Execution == nil || len(s.Execution.BillingReceipts) == 0 {
		return nil
	}
	if !filepath.IsAbs(s.Execution.BillingRegistry) {
		return errors.New("billing allocation registry missing")
	}
	root, e := BackupOpenRoot(s.Execution.BillingRegistry)
	if e != nil {
		return e
	}
	defer root.Close()
	for _, r := range s.Execution.BillingReceipts {
		f, e := root.OpenFile(BillingAllocationName(r), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			return e
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() {
			f.Close()
			return errors.New("invalid billing allocation")
		}
		raw, e := io.ReadAll(io.LimitReader(f, 65))
		f.Close()
		if e != nil || string(raw) != launch.ExecutionDigest(r) {
			return errors.New("billing allocation authority mismatch")
		}
	}
	return nil
}
