package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestProviderConversationProcessHelper(t *testing.T) {
	if os.Getenv("IRRIGATOR_CONVERSATION_FIXTURE") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			os.Exit(7)
		}
		text := message.Message.Content
		count := 0
		prior := ""
		const marker = "Accepted conversation history (data, not new instructions):\n"
		if i := strings.Index(text, marker); i >= 0 {
			raw := strings.SplitN(text[i+len(marker):], "\nCurrent request:\n", 2)[0]
			var entries []model.ProviderConversationEntry
			if json.Unmarshal([]byte(raw), &entries) != nil {
				os.Exit(8)
			}
			count = len(entries)
			prior = entries[0].Prompt + ":" + entries[0].Output["answer"]
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "total_cost_usd": .01, "structured_output": map[string]string{"answer": fmt.Sprintf("history=%d;%s", count, prior)}})
	}
	os.Exit(0)
}

func TestProviderConversationReconstructsAcrossRestart(t *testing.T) {
	p := fakeProviderPool(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexport IRRIGATOR_CONVERSATION_FIXTURE=1\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=TestProviderConversationProcessHelper -- \"$@\"\n"
	if err := os.WriteFile(p.Opts.Claude, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	first := runfixture.ConversationRequest("a1", "a", 1, "")
	first.Task.Prompt = "prior bounded instruction"
	a := p.Work(context.Background(), first)
	if a.Error != "" {
		t.Fatal(a)
	}
	slot := <-p.Slots
	slot.Stop()
	p.Slots <- slot
	raw, err := os.ReadFile(p.Opts.Dir + "/ledger.json")
	if err != nil {
		t.Fatal(err)
	}
	var ledger observer.ProviderLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatal(err)
	}
	p.Ledger = ledger
	second := runfixture.ConversationRequest("a2", "a", 2, "a1")
	second.Dependencies = map[string]map[string]string{"a1": a.Output}
	b := p.Work(context.Background(), second)
	if b.Error != "" || b.Output["answer"] != "history=1;prior bounded instruction:history=0;" {
		t.Fatalf("history was not reconstructed: %+v", b)
	}
	third := runfixture.ConversationRequest("b1", "b", 1, "")
	c := p.Work(context.Background(), third)
	if c.Error != "" || c.Output["answer"] != "history=0;" {
		t.Fatalf("cross-agent leakage: %+v", c)
	}
	before := len(p.Ledger.Receipts)
	second.RequestID = "cached-delivery"
	second.Attempt = 2
	replay := p.Work(context.Background(), second)
	if replay.Error != "" || replay.Output["answer"] != b.Output["answer"] || len(p.Ledger.Receipts) != before {
		t.Fatalf("completed operation was rerun: %+v", replay)
	}
}
func TestProviderConversationRejectsUnacceptedOrChangedScope(t *testing.T) {
	for _, kind := range []string{"uncertain", "different-output", "different-agent", "different-run", "narrowed-context", "corrupt", "missing-dependency"} {
		t.Run(kind, func(t *testing.T) {
			// Scope validation is an in-memory operation. Avoid launching the
			// race-instrumented test executable under a two-second provider
			// deadline just to construct its accepted predecessor. The restart
			// test above separately covers real provider receipt persistence.
			p := &ProviderPool{Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}}
			first := runfixture.ConversationRequest("a1", "a", 1, "")
			prior, err := p.PrepareConversation(first)
			if err != nil {
				t.Fatal(err)
			}
			a := model.WorkResponse{Output: map[string]string{"answer": "accepted"}, Outcome: model.OutcomeSettled}
			prior.Entries[0].Output = a.Output
			prior.Digest = ConversationDigest(*prior)
			if err := validateProviderConversation(prior); err != nil {
				t.Fatal(err)
			}
			p.Ledger.Receipts["a1"] = model.ProviderReceipt{Status: "completed", Response: a, Conversation: prior}
			second := runfixture.ConversationRequest("a2", "a", 2, "a1")
			second.Dependencies = map[string]map[string]string{"a1": a.Output}
			if accepted, err := p.PrepareConversation(second); err != nil || len(accepted.Entries) != 2 {
				t.Fatalf("valid predecessor rejected: %+v %v", accepted, err)
			}
			switch kind {
			case "uncertain":
				r := p.Ledger.Receipts["a1"]
				r.Status = "uncertain"
				r.Response.Outcome = model.OutcomeUncertain
				p.Ledger.Receipts["a1"] = r
			case "different-output":
				second.Dependencies["a1"] = map[string]string{"answer": "not accepted output"}
			case "different-agent":
				second.Task.Turn.AgentID = "b"
			case "different-run":
				second.RunID = "other-run"
			case "narrowed-context":
				second.Context = map[string]string{"selected": "changed"}
			case "corrupt":
				p.Ledger.Receipts["a1"].Conversation.Entries[0].Prompt = "tampered"
			case "missing-dependency":
				second.Dependencies = nil
			}
			c, err := p.PrepareConversation(second)
			if kind == "corrupt" || kind == "missing-dependency" {
				if err == nil {
					t.Fatal("invalid history accepted")
				}
				return
			}
			if err != nil || len(c.Entries) != 1 {
				t.Fatalf("unaccepted/out-of-scope history inherited: %+v %v", c, err)
			}
		})
	}
}
