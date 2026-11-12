package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func contextProvider(t *testing.T) model.CommandSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return model.CommandSpec{Command: []string{exe, "-test.run=^TestContextProviderHelper$"}, Env: []string{"IRRIGATOR_CONTEXT_TEST_HELPER"}, Replay: "safe"}
}

func callOutcomeExternal(t *testing.T, kind, extra, raw string) (model.WorkResponse, error) {
	t.Helper()
	script := runfixture.WriteOutcomeScript(t)
	runfixture.SetOutcomeEnv(t, extra, raw, "")
	cell := t.TempDir()
	ctx, cancel := runfixture.OutcomeLeaseContext(t, cell, 10*time.Second)
	defer cancel()
	req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: kind, RequestID: "rid", OperationID: "op", Task: model.Task{ID: "a"}, Attempt: 1, TimeoutMS: 1000}
	spec := model.CommandSpec{Command: []string{script}, Env: []string{"RECOVERY_EXTRA", "RECOVERY_RAW"}, Replay: "safe"}
	return External(ctx, cell, filepath.Join(cell, "1"), spec, req)
}

func pdiagPool(t *testing.T, mode, step string, budget float64) (*provider.ProviderPool, string) {
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
	p := &provider.ProviderPool{Opts: provider.ProviderPoolOptions{Dir: dir, Claude: script, Budget: budget}, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}, Slots: make(chan *provider.ProviderSlot, 1)}
	p.Slots <- &provider.ProviderSlot{}
	t.Cleanup(func() { (<-p.Slots).Stop() })
	return p, starts
}

func TestProviderProcessHelper(t *testing.T) {
	if os.Getenv("IRRIGATOR_TEST_PROVIDER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	count := 0
	for scanner.Scan() {
		count++
		switch os.Getenv("IRRIGATOR_TEST_PROVIDER_MODE") {
		case "eof":
			os.Exit(0)
		case "hang":
			time.Sleep(time.Minute)
		case "malformed":
			fmt.Println("{bad")
			continue
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "total_cost_usd": float64(count) * .01, "structured_output": map[string]string{"answer": fmt.Sprintf("%d:%d", os.Getpid(), count)}})
	}
	os.Exit(0)
}

func fakeProviderPool(t *testing.T) *provider.ProviderPool {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "claude")
	if err = os.WriteFile(script, []byte("#!/bin/sh\nexport IRRIGATOR_TEST_PROVIDER=1\nIRRIGATOR_TEST_PROVIDER_MODE=$(cat .test-provider-mode 2>/dev/null)\nexport IRRIGATOR_TEST_PROVIDER_MODE\nexec '"+strings.ReplaceAll(exe, "'", "'\\''")+"' -test.run=TestProviderProcessHelper -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	p := &provider.ProviderPool{Opts: provider.ProviderPoolOptions{Dir: dir, Claude: script, Budget: .5}, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}, Slots: make(chan *provider.ProviderSlot, 1)}
	p.Slots <- &provider.ProviderSlot{}
	t.Cleanup(func() { (<-p.Slots).Stop() })
	return p
}

func setFakeProviderMode(t *testing.T, p *provider.ProviderPool, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.Opts.Dir, ".test-provider-mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
}

func callHaiku(ctx context.Context, cell string, attempt int, task model.Task, inputs map[string]map[string]string, budget float64, contexts ...map[string]string) (map[string]string, float64, error) {
	return callHaikuBounded(ctx, cell, attempt, task, inputs, budget, nil, contexts...)
}

func managedWork(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding) (model.WorkResponse, error) {
	return managed.ManagedWorkWithHistory(ctx, cell, prefix, req, b, nil)
}

// externalReportedCost is the USD view of externalCostEvidence for callers that
// only need a known amount; invalid and unavailable evidence are both unknown.
func externalReportedCost(raw []byte, req model.WorkRequest) (float64, bool) {
	evidence := provider.ExternalCostEvidence(raw, req)
	if !evidence.Known {
		return 0, false
	}
	return cost.MicrosToFloat(evidence.Micros), true
}

func worker(root, id string, opts model.Options) error {
	return WorkerOperation(root, id, opts, "")
}
