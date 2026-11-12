package provider

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

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

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
func fakeProviderPool(t *testing.T) *ProviderPool {
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
	p := &ProviderPool{Opts: ProviderPoolOptions{Dir: dir, Claude: script, Budget: .5}, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}, Slots: make(chan *ProviderSlot, 1)}
	p.Slots <- &ProviderSlot{}
	t.Cleanup(func() { (<-p.Slots).Stop() })
	return p
}
func setFakeProviderMode(t *testing.T, p *ProviderPool, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.Opts.Dir, ".test-provider-mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProviderPoolPersistentTurnsAndDurableReplay(t *testing.T) {
	p := fakeProviderPool(t)
	a := p.Work(context.Background(), runfixture.FakeProviderRequest("a"))
	// Reuse requires the later reservation to equal remaining cumulative CLI authority.
	second := runfixture.FakeProviderRequest("b")
	second.MaxCostUSD = .09
	b := p.Work(context.Background(), second)
	if a.Error != "" || b.Error != "" {
		t.Fatalf("%+v %+v", a, b)
	}
	if strings.Split(a.Output["answer"], ":")[0] != strings.Split(b.Output["answer"], ":")[0] || !strings.HasSuffix(b.Output["answer"], ":2") {
		t.Fatalf("not one persistent process: %v %v", a, b)
	}
	if a.CostUSD != .01 || b.CostUSD != .01 {
		t.Fatalf("cost delta: %v %v", a.CostUSD, b.CostUSD)
	}
	raw, err := os.ReadFile(filepath.Join(p.Opts.Dir, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recovered observer.ProviderLedger
	if err = json.Unmarshal(raw, &recovered); err != nil {
		t.Fatal(err)
	}
	p.Ledger = recovered
	req := runfixture.FakeProviderRequest("a")
	req.RequestID = "retry"
	req.Attempt = 2
	again := p.Work(context.Background(), req)
	if again.Output["answer"] != a.Output["answer"] || again.RequestID != "retry" {
		t.Fatalf("bad replay %+v", again)
	}
	req.Task.Prompt = "different"
	if got := p.Work(context.Background(), req); !strings.Contains(got.Error, "conflict") {
		t.Fatalf("expected conflict %+v", got)
	}
}
func TestProviderPoolUncertainNotReplayed(t *testing.T) {
	for _, mode := range []string{"eof", "malformed", "hang"} {
		t.Run(mode, func(t *testing.T) {
			p := fakeProviderPool(t)
			setFakeProviderMode(t, p, mode)
			req := runfixture.FakeProviderRequest("a")
			req.TimeoutMS = 50
			got := p.Work(context.Background(), req)
			if got.Error == "" {
				t.Fatal("expected error")
			}
			if p.Ledger.Receipts["a"].Status != "uncertain" || p.Ledger.Receipts["a"].Reserved != .1 {
				t.Fatal("uncertain charge not preserved")
			}
			again := p.Work(context.Background(), req)
			if !strings.Contains(again.Error, "replay prohibited") {
				t.Fatal(again)
			}
		})
	}
}
func TestProviderPoolBudgetAndContextIsolation(t *testing.T) {
	p := fakeProviderPool(t)
	p.Opts.Budget = .15
	a := p.Work(context.Background(), runfixture.FakeProviderRequest("a"))
	r := runfixture.FakeProviderRequest("b")
	r.Context = map[string]string{"secret": "separate"}
	b := p.Work(context.Background(), r)
	if b.Error != "" {
		t.Fatal(b)
	}
	if strings.Split(a.Output["answer"], ":")[0] == strings.Split(b.Output["answer"], ":")[0] {
		t.Fatal("context leaked across processes")
	}
	p.Opts.Budget = .05
	if got := p.Work(context.Background(), runfixture.FakeProviderRequest("c")); !strings.Contains(got.Error, "budget exhausted") {
		t.Fatal(got)
	}
}
func TestProviderPoolConcurrentDuplicate(t *testing.T) {
	p := fakeProviderPool(t)
	setFakeProviderMode(t, p, "hang")
	req := runfixture.FakeProviderRequest("same")
	req.TimeoutMS = 100
	done := make(chan model.WorkResponse, 1)
	go func() { done <- p.Work(context.Background(), req) }()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		_, ok := p.Ledger.Receipts["same"]
		p.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	if got := p.Work(context.Background(), req); !strings.Contains(got.Error, "replay prohibited") {
		t.Fatal(got)
	}
	<-done
}

func TestProviderServerConnectLifecycle(t *testing.T) {
	p := fakeProviderPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	opts := p.Opts
	opts.listen = "127.0.0.1:0"
	opts.workers = 1
	go func() { done <- serveProvider(ctx, opts) }()
	config := filepath.Join(opts.Dir, "connection.json")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(config); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("server failed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(time.Millisecond)
	}
	var first string
	for _, id := range []string{"first", "second"} {
		request := runfixture.FakeProviderRequest(id)
		if id == "second" {
			request.MaxCostUSD = .09 // Exactly matches .1 initial cap minus .01 settled; no excess authority.
		}
		raw, _ := json.Marshal(request)
		var out strings.Builder
		if err := ConnectProvider(ctx, config, strings.NewReader(string(raw)), &out); err != nil {
			t.Fatal(err)
		}
		var response model.WorkResponse
		if err := json.Unmarshal([]byte(out.String()), &response); err != nil {
			t.Fatal(err)
		}
		if response.Error != "" {
			t.Fatal(response.Error)
		}
		pid := strings.Split(response.Output["answer"], ":")[0]
		if first == "" {
			first = pid
		} else if first != pid {
			t.Fatal("not persistent")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server shutdown leaked worker")
	}
}
