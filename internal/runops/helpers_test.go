package runops

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

	"github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func campaignFixture(t *testing.T) (string, model.Plan, string) {
	t.Helper()
	base := runfixture.CompositionTempDir(t)
	dir := filepath.Join(base, "campaign")
	if e := campaign.CampaignInit(dir, "1"); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, runfixture.MailboxFixture(t))
	p := s.Plan
	p.Execution.RunBudgetUSD = "1"
	return dir, p, base
}

func lcGet(t *testing.T, m map[string]*observer.PanelLifecycle, id string) *observer.PanelLifecycle {
	t.Helper()
	l := m[id]
	if l == nil {
		t.Fatalf("no lifecycle for %s", id)
	}
	return l
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

func commitTurnExpansion(root, id, level string) error {
	return graph.CommitTurnExpansionFenced(root, id, level, nil)
}

func commitExpansion(root, id, level string) error {
	return graph.CommitExpansionFenced(root, id, level, nil)
}

func retryNode(root, id string, recursive, allowUncertain bool, confirmStopped ...bool) error {
	_, err := RetryNodeWithReport(root, id, recursive, allowUncertain, confirmStopped...)
	return err
}

func managedWork(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding) (model.WorkResponse, error) {
	return managed.ManagedWorkWithHistory(ctx, cell, prefix, req, b, nil)
}

func worker(root, id string, opts model.Options) error {
	return executor.WorkerOperation(root, id, opts, "")
}
