package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func pdiagPool(t *testing.T, mode, step string, budget float64) (*ProviderPool, string) {
	t.Helper()
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	// Fixture configuration belongs to this executable, not the ambient provider
	// environment. Keep the production credential allowlist under test unchanged.
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	configuration := "PDIAG_MODE=" + quote(mode) + "\nPDIAG_STEP=" + quote(step) + "\nPDIAG_STARTS=" + quote(starts) + "\n"
	body := strings.Replace(runfixture.PdiagScript, "#!/bin/sh\n", "#!/bin/sh\n"+configuration, 1)
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	p := &ProviderPool{Opts: ProviderPoolOptions{Dir: dir, Claude: script, Budget: budget}, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}, Slots: make(chan *ProviderSlot, 1)}
	p.Slots <- &ProviderSlot{}
	t.Cleanup(func() { (<-p.Slots).Stop() })
	return p, starts
}
