package runops

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func callOutcomeExternal(t *testing.T, kind, extra, raw string) (model.WorkResponse, error) {
	t.Helper()
	script := runfixture.WriteOutcomeScript(t)
	runfixture.SetOutcomeEnv(t, extra, raw, "")
	cell := t.TempDir()
	ctx, cancel := runfixture.OutcomeLeaseContext(t, cell, 10*time.Second)
	defer cancel()
	req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: kind, RequestID: "rid", OperationID: "op", Task: model.Task{ID: "a"}, Attempt: 1, TimeoutMS: 1000}
	spec := model.CommandSpec{Command: []string{script}, Env: []string{"RECOVERY_EXTRA", "RECOVERY_RAW"}, Replay: "safe"}
	return executor.External(ctx, cell, filepath.Join(cell, "1"), spec, req)
}

func TestOutcomeCertaintyParsingAndLegacyAbsence(t *testing.T) {
	for _, v := range []string{"", model.OutcomeNotStarted, model.OutcomeSettled, model.OutcomeUncertain} {
		if got, err := model.ParseOutcome(v); err != nil || got != v {
			t.Fatalf("%q: %q %v", v, got, err)
		}
	}
	if model.OutcomeNotStarted != "not_started" || model.OutcomeSettled != "settled" || model.OutcomeUncertain != "uncertain" {
		t.Fatal("enum spellings are a protocol contract")
	}
	for _, v := range []string{"Uncertain", "unknown", " settled", "not-started", "null", "SECRET-VALUE"} {
		_, err := model.ParseOutcome(v)
		if err == nil || strings.Contains(err.Error(), v) {
			t.Fatalf("%q must fail closed without echo: %v", v, err)
		}
	}
	raw, _ := json.Marshal(model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: "r"})
	if strings.Contains(string(raw), "outcome") {
		t.Fatalf("absent outcome must stay omitted: %s", raw)
	}
}

func TestExternalOutcomeDecoding(t *testing.T) {
	cases := []struct {
		name, kind, extra, raw string
		fail, uncertain        bool
	}{
		{"legacy error", "task", `,"error":"boom"`, "", true, false},
		{"legacy success", "task", `,"output":{"x":"v"}`, "", false, false},
		{"settled error", "task", `,"outcome":"settled","error":"known"`, "", true, false},
		{"settled success", "task", `,"outcome":"settled","output":{"x":"v"}`, "", false, false},
		{"not started error", "task", `,"outcome":"not_started","error":"refused"`, "", true, false},
		{"not started without error", "task", `,"outcome":"not_started"`, "", true, true},
		{"uncertain error", "task", `,"outcome":"uncertain","error":"timeout"`, "", true, true},
		{"uncertain with output", "task", `,"outcome":"uncertain","output":{"x":"v"}`, "", true, true},
		{"malformed value", "task", `,"outcome":"maybe","output":{"x":"v"}`, "", true, true},
		{"malformed type", "task", `,"outcome":7`, "", true, true},
		{"garbage stdout", "task", "", `{bad`, true, true},
		{"request mismatch", "task", "", `{"protocol":"swarm.worker/v1","request_id":"other"}`, true, true},
		{"validator uncertain", "validate", `,"outcome":"uncertain","error":"lost"`, "", true, true},
		{"validator garbage", "validate", "", `{bad`, true, false},
		{"validator settled pass", "validate", `,"outcome":"settled","passed":true`, "", false, false},
		{"validator malformed value", "validate", `,"outcome":"maybe"`, "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, err := callOutcomeExternal(t, tc.kind, tc.extra, tc.raw)
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v want fail=%v", err, tc.fail)
			}
			if model.IsUncertain(err) != tc.uncertain {
				t.Fatalf("uncertain=%v want %v (%v)", model.IsUncertain(err), tc.uncertain, err)
			}
			if tc.name == "uncertain error" && response.Outcome != model.OutcomeUncertain {
				t.Fatalf("%+v", response)
			}
		})
	}
}

func TestExternalDeadlineAfterStartIsUncertainEvenWhenReplaySafe(t *testing.T) {
	for _, kind := range []string{"task", "validate"} {
		t.Run(kind, func(t *testing.T) {
			cell := t.TempDir()
			ctx, cancel := runfixture.OutcomeLeaseContext(t, cell, 150*time.Millisecond)
			defer cancel()
			req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: kind, RequestID: "rid", OperationID: "op", Task: model.Task{ID: "a"}, Attempt: 1, TimeoutMS: 100}
			_, err := executor.External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{"sleep", "5"}, Replay: "safe"}, req)
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("want deadline error: %v", err)
			}
			if got := model.IsUncertain(err); got != (kind == "task") {
				t.Fatalf("kind %s uncertain=%v (%v)", kind, got, err)
			}
			if kind == "task" && model.OutcomeReason(err) != "deadline" {
				t.Fatalf("reason %q", model.OutcomeReason(err))
			}
		})
	}
}

func TestTransportOutcomeNeverStartedAndCancellation(t *testing.T) {
	task := model.WorkRequest{Kind: "task"}
	if model.IsUncertain(model.TransportOutcome(task, model.MarkNotStarted(context.DeadlineExceeded))) {
		t.Fatal("a command that demonstrably never started is a known failure")
	}
	if !model.IsUncertain(model.TransportOutcome(task, context.DeadlineExceeded)) {
		t.Fatal("deadline after start must be uncertain")
	}
	if !model.IsUncertain(model.TransportOutcome(task, context.Canceled)) || model.OutcomeReason(model.TransportOutcome(task, context.Canceled)) != "cancelled" {
		t.Fatal("cancellation after start must be uncertain: pending reset and explicit retry change the operation generation")
	}
	if !model.IsUncertain(model.TransportOutcome(task, fmt.Errorf("wrapped: %w", context.Canceled))) {
		t.Fatal("wrapped cancellation after start must be uncertain")
	}
	if model.IsUncertain(model.TransportOutcome(task, model.MarkNotStarted(context.Canceled))) {
		t.Fatal("cancellation of a command that never started remains a known failure")
	}
	if err := errors.New("plain transport failure"); model.TransportOutcome(task, err) != err {
		t.Fatal("non-context errors are untouched")
	}
	validate := model.WorkRequest{Kind: "validate"}
	for _, e := range []error{context.Canceled, context.DeadlineExceeded} {
		if model.IsUncertain(model.TransportOutcome(validate, e)) {
			t.Fatalf("read-only validator keeps legacy transport behavior: %v", e)
		}
	}
	if !errors.Is(model.MarkNotStarted(context.Canceled), context.Canceled) {
		t.Fatal("marker must preserve error identity")
	}
}

func TestExternalCancellationAfterStartIsUncertainEvenWhenReplaySafe(t *testing.T) {
	for _, kind := range []string{"task", "validate"} {
		t.Run(kind, func(t *testing.T) {
			cell := t.TempDir()
			ctx, cancel := runfixture.OutcomeLeaseContext(t, cell, 10*time.Second)
			defer cancel()
			timer := time.AfterFunc(150*time.Millisecond, cancel)
			defer timer.Stop()
			req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: kind, RequestID: "rid", OperationID: "op", Task: model.Task{ID: "a"}, Attempt: 1, TimeoutMS: 10000}
			_, err := executor.External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{"sleep", "5"}, Replay: "safe"}, req)
			if err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("want cancellation error: %v", err)
			}
			if got := model.IsUncertain(err); got != (kind == "task") {
				t.Fatalf("kind %s uncertain=%v (%v)", kind, got, err)
			}
			if kind == "task" && model.OutcomeReason(err) != "cancelled" {
				t.Fatalf("reason %q", model.OutcomeReason(err))
			}
		})
	}
}

func TestMalformedOutcomeEmptyFailedProcess(t *testing.T) {
	task, validate := model.WorkRequest{Kind: "task"}, model.WorkRequest{Kind: "validate"}
	processErr := errors.New("exit status 1")
	decodeErr := errors.New("invalid provider response: EOF")
	if got := model.MalformedOutcome(task, nil, processErr, decodeErr); !model.IsUncertain(got) || model.OutcomeReason(got) != "malformed_response" {
		t.Fatalf("empty stdout from a failed started process must be uncertain: %v", got)
	}
	if got := model.MalformedOutcome(task, []byte{}, processErr, decodeErr); !model.IsUncertain(got) {
		t.Fatalf("empty non-nil stdout must be uncertain: %v", got)
	}
	if got := model.MalformedOutcome(task, nil, nil, decodeErr); !model.IsUncertain(got) {
		t.Fatalf("empty stdout from a successful process must be uncertain: %v", got)
	}
	if got := model.MalformedOutcome(task, nil, model.MarkNotStarted(processErr), decodeErr); model.IsUncertain(got) {
		t.Fatalf("a command positively marked not started is a known failure: %v", got)
	}
	if got := model.MalformedOutcome(validate, nil, processErr, decodeErr); model.IsUncertain(got) {
		t.Fatalf("read-only validators keep legacy behavior: %v", got)
	}
}

const emptyExitScript = "#!/bin/sh\ncat >/dev/null\necho SECRET-STDERR >&2\nexit 1\n"

func TestExternalEmptyOutputWithFailedProcessIsUncertainForTask(t *testing.T) {
	script := runfixture.WriteScriptBody(t, emptyExitScript)
	for _, kind := range []string{"task", "validate"} {
		t.Run(kind, func(t *testing.T) {
			cell := t.TempDir()
			ctx, cancel := runfixture.OutcomeLeaseContext(t, cell, 10*time.Second)
			defer cancel()
			req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: kind, RequestID: "rid", OperationID: "op", Task: model.Task{ID: "a"}, Attempt: 1, TimeoutMS: 1000}
			_, err := executor.External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{script}, Replay: "safe"}, req)
			if err == nil {
				t.Fatal("empty output with exit 1 must fail")
			}
			if got := model.IsUncertain(err); got != (kind == "task") {
				t.Fatalf("kind %s uncertain=%v (%v)", kind, got, err)
			}
			if kind == "task" && model.OutcomeReason(err) != "malformed_response" {
				t.Fatalf("reason %q", model.OutcomeReason(err))
			}
		})
	}
}

func runScriptWorker(t *testing.T, body string) (string, *model.State) {
	t.Helper()
	script := runfixture.WriteScriptBody(t, body)
	plan := model.Plan{Name: "outcome", Tasks: []model.Task{{ID: "a", Prompt: "p", Requires: []string{"x"}, Provider: "pool"}}, Providers: map[string]model.CommandSpec{"pool": {Command: []string{script}, Replay: "safe"}}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(plan, root, "mock"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 10 * time.Second, Budget: 1}); err != nil {
		t.Fatal(err)
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, s
}

func TestWorkerEmptyOutputExitOneFromReplaySafeProviderIsUncertain(t *testing.T) {
	root, s := runScriptWorker(t, emptyExitScript)
	row := s.Tasks["a"]
	if !graph.ReplaySafe(s, row.Spec) {
		t.Fatal("fixture must be Replay=safe")
	}
	if row.Status != "uncertain" || row.Result != nil {
		t.Fatalf("empty stdout with exit 1 must be uncertain, never failed: %+v", row)
	}
	if strings.Contains(row.Error, "SECRET-STDERR") || !strings.Contains(row.Error, "malformed_response") {
		t.Fatalf("fixed private diagnostic expected: %q", row.Error)
	}
	if err := retryNode(root, "a", false, false); err == nil || !strings.Contains(err.Error(), "outcome uncertain") {
		t.Fatalf("retry must refuse uncertain work: %v", err)
	}
	seen := false
	for _, e := range s.Events {
		if e.Task == "a" && e.Kind == "failed" {
			t.Fatalf("unexpected failed event: %+v", e)
		}
		seen = seen || (e.Task == "a" && e.Kind == "outcome_uncertain")
	}
	if !seen {
		t.Fatal("missing outcome_uncertain event")
	}
}

func TestWorkerValidResponseWithFailedProcessKeepsLegacyPolicy(t *testing.T) {
	_, s := runScriptWorker(t, runfixture.ValidExitScript)
	if got := s.Tasks["a"].Status; got != "failed" {
		t.Fatalf("a valid response with absent outcome keeps the legacy policy: %s", got)
	}
}

func TestUncertainOutcomeBeatsCancellationAndInterruption(t *testing.T) {
	s := &model.State{Backend: "mock", Tasks: map[string]*model.TaskState{}}
	base := &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "provider_reported", Err: errors.New("SECRET-RAW-ERROR")}
	errs := map[string]error{
		"explicit": base,
		"wrapped":  fmt.Errorf("check 0: %w", base),
		"cancel":   &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "deadline", Err: context.Canceled},
	}
	for name, err := range errs {
		for _, requested := range []bool{false, true} {
			row := &model.TaskState{Spec: model.Task{ID: "a"}, Status: "running", CancelRequested: requested}
			if !runfixture.ApplyUncertainOutcome(s, "a", row, err) {
				t.Fatalf("%s: not applied", name)
			}
			if row.Status != "uncertain" || row.CancelRequested != requested || strings.Contains(row.Error, "SECRET") {
				t.Fatalf("%s: %+v", name, row)
			}
		}
	}
	row := &model.TaskState{Spec: model.Task{ID: "a"}, Status: "running"}
	if runfixture.ApplyUncertainOutcome(s, "a", row, context.Canceled) || row.Status != "running" {
		t.Fatal("non-explicit errors keep the existing policy")
	}
}

func TestProviderPoolPostDispatchFailuresAreExplicitlyUncertain(t *testing.T) {
	for _, mode := range []string{"eof", "malformed", "hang"} {
		t.Run(mode, func(t *testing.T) {
			p := fakeProviderPool(t)
			setFakeProviderMode(t, p, mode)
			req := runfixture.FakeProviderRequest("rid")
			req.TimeoutMS = 50
			got := p.Work(context.Background(), req)
			if got.Outcome != model.OutcomeUncertain || got.Error == "" {
				t.Fatalf("%+v", got)
			}
			receipt := p.Ledger.Receipts["rid"]
			if receipt.Status != "uncertain" || receipt.Reserved != .1 || receipt.Response.Outcome != model.OutcomeUncertain {
				t.Fatalf("reservation must stay held: %+v", receipt)
			}
			again := p.Work(context.Background(), req)
			if again.Outcome != model.OutcomeUncertain || !strings.Contains(again.Error, "replay prohibited") || p.Ledger.Receipts["rid"].Reserved != .1 {
				t.Fatalf("%+v", again)
			}
			// The pooled response reaches the worker decoder as explicit uncertainty.
			raw, _ := json.Marshal(got)
			if _, err := callOutcomeExternal(t, "task", "", string(raw)); !model.IsUncertain(err) {
				t.Fatalf("pool uncertainty lost in external decoding: %v", err)
			}
		})
	}
}

func TestProviderPoolCancellationAfterDispatchStaysUncertain(t *testing.T) {
	p := fakeProviderPool(t)
	setFakeProviderMode(t, p, "hang")
	req := runfixture.FakeProviderRequest("c")
	req.TimeoutMS = 5000
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	got := p.Work(ctx, req)
	if got.Outcome != model.OutcomeUncertain || p.Ledger.Receipts["c"].Status != "uncertain" || p.Ledger.Receipts["c"].Reserved != .1 {
		t.Fatalf("%+v %+v", got, p.Ledger.Receipts["c"])
	}
}

func TestProviderPoolSettledResponseReplayCompatible(t *testing.T) {
	p := fakeProviderPool(t)
	a := p.Work(context.Background(), runfixture.FakeProviderRequest("a"))
	if a.Error != "" || a.Outcome != model.OutcomeSettled {
		t.Fatalf("%+v", a)
	}
	req := runfixture.FakeProviderRequest("a")
	req.RequestID, req.Attempt = "again", 2
	again := p.Work(context.Background(), req)
	if again.Outcome != model.OutcomeSettled || again.RequestID != "again" || again.Output["answer"] != a.Output["answer"] {
		t.Fatalf("%+v", again)
	}
	// A receipt persisted before this field existed replays with the field absent.
	receipt := p.Ledger.Receipts["a"]
	receipt.Response.Outcome = ""
	p.Ledger.Receipts["a"] = receipt
	legacy := p.Work(context.Background(), req)
	if legacy.Outcome != "" || legacy.Output["answer"] != a.Output["answer"] {
		t.Fatalf("%+v", legacy)
	}
}

func TestProviderPoolKnownPredispatchFailuresAreNotStarted(t *testing.T) {
	p := fakeProviderPool(t)
	bad := runfixture.FakeProviderRequest("bad")
	bad.MaxCostUSD = 0
	if got := p.Work(context.Background(), bad); got.Outcome != model.OutcomeNotStarted || got.Error == "" || len(p.Ledger.Receipts) != 0 {
		t.Fatalf("%+v", got)
	}
	p.Opts.Budget = .05
	if got := p.Work(context.Background(), runfixture.FakeProviderRequest("big")); got.Outcome != model.OutcomeNotStarted || !strings.Contains(got.Error, "budget exhausted") || len(p.Ledger.Receipts) != 0 {
		t.Fatalf("%+v", got)
	}
	p.Opts.Budget = .5
	p.Opts.Claude = filepath.Join(t.TempDir(), "missing-claude")
	got := p.Work(context.Background(), runfixture.FakeProviderRequest("start"))
	receipt := p.Ledger.Receipts["start"]
	if got.Outcome != model.OutcomeNotStarted || got.Error == "" || receipt.Status != "completed" || receipt.Reserved != 0 {
		t.Fatalf("startup failure is a known predispatch failure: %+v %+v", got, receipt)
	}
}

func TestProviderPoolReceiptConflictAndPendingPropagateCertainty(t *testing.T) {
	p := fakeProviderPool(t)
	if done := p.Work(context.Background(), runfixture.FakeProviderRequest("c")); done.Outcome != model.OutcomeSettled {
		t.Fatalf("%+v", done)
	}
	changed := runfixture.FakeProviderRequest("c")
	changed.Task.Prompt = "different"
	if got := p.Work(context.Background(), changed); !strings.Contains(got.Error, "conflict") || got.Outcome != model.OutcomeNotStarted {
		t.Fatalf("conflict with a completed receipt dispatches nothing: %+v", got)
	}
	canonical := runfixture.FakeProviderRequest("pending")
	canonical.RequestID, canonical.Attempt = "", 0
	encoded, _ := json.Marshal(canonical)
	p.Ledger.Receipts["pending"] = model.ProviderReceipt{Hash: fmt.Sprintf("%x", sha256.Sum256(encoded)), Status: "pending", Reserved: .1}
	got := p.Work(context.Background(), runfixture.FakeProviderRequest("pending"))
	if got.Outcome != model.OutcomeUncertain || !strings.Contains(got.Error, "replay prohibited") {
		t.Fatalf("pending receipt must return uncertain: %+v", got)
	}
	if conflict := p.Work(context.Background(), func() model.WorkRequest {
		r := runfixture.FakeProviderRequest("pending")
		r.Task.Prompt = "different"
		return r
	}()); conflict.Outcome != model.OutcomeUncertain {
		t.Fatalf("conflict with an unresolved receipt stays uncertain: %+v", conflict)
	}
	if p.Ledger.Receipts["pending"].Reserved != .1 {
		t.Fatal("reservation released")
	}
}

func TestProviderPoolReceiptSaveFailureIsUncertainAndHeld(t *testing.T) {
	p := fakeProviderPool(t)
	p.Opts.Dir = filepath.Join(t.TempDir(), "missing")
	req := runfixture.FakeProviderRequest("s")
	p.Ledger.Receipts["s"] = model.ProviderReceipt{Hash: strings.Repeat("a", 64), Status: "pending", Reserved: .1}
	got := p.Finish(req, strings.Repeat("a", 64), model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: "s", Output: map[string]string{"answer": "x"}, CostUSD: .01}, true)
	receipt := p.Ledger.Receipts["s"]
	if got.Outcome != model.OutcomeUncertain || got.Output != nil || !strings.Contains(got.Error, "persistence failed") {
		t.Fatalf("%+v", got)
	}
	if receipt.Status != "uncertain" || receipt.Reserved != .1 || receipt.Response.Outcome != model.OutcomeUncertain {
		t.Fatalf("%+v", receipt)
	}
}

func connectionFile(t *testing.T, url string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "connection.json")
	raw, _ := json.Marshal(provider.ProviderConnection{URL: url, Token: "t"})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConnectProviderLostResponseIsUncertain(t *testing.T) {
	body, _ := json.Marshal(runfixture.FakeProviderRequest("lost"))
	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	defer lost.Close()
	serverError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer serverError.Close()
	for name, url := range map[string]string{"connection dropped": lost.URL, "server error": serverError.URL} {
		var out strings.Builder
		if err := provider.ConnectProvider(context.Background(), connectionFile(t, url), strings.NewReader(string(body)), &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var response model.WorkResponse
		if err := json.Unmarshal([]byte(out.String()), &response); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if response.Outcome != model.OutcomeUncertain || response.RequestID != "lost" || response.Protocol != model.ProtocolVersion || response.Error == "" {
			t.Fatalf("%s: %+v", name, response)
		}
	}
}

func TestConnectProviderKnownNotDispatchedStaysAnError(t *testing.T) {
	body, _ := json.Marshal(runfixture.FakeProviderRequest("x"))
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "full", http.StatusTooManyRequests) }))
	defer busy.Close()
	var out strings.Builder
	if err := provider.ConnectProvider(context.Background(), connectionFile(t, busy.URL), strings.NewReader(string(body)), &out); err == nil || out.Len() != 0 {
		t.Fatalf("429 is a known rejection: %v %q", err, out.String())
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	out.Reset()
	if err := provider.ConnectProvider(context.Background(), connectionFile(t, "http://"+addr), strings.NewReader(string(body)), &out); err == nil || out.Len() != 0 {
		t.Fatalf("refused dial is a known not-started failure: %v %q", err, out.String())
	}
}

func runOutcomeWorker(t *testing.T, extra, raw, validator string, withValidator bool) (string, *model.State) {
	t.Helper()
	script := runfixture.WriteOutcomeScript(t)
	runfixture.SetOutcomeEnv(t, extra, raw, validator)
	task := model.Task{ID: "a", Prompt: "p", Requires: []string{"x"}, Provider: "pool"}
	if withValidator {
		task.Checks = []model.Check{{Kind: "command", Command: []string{script}, Env: []string{"VALIDATOR_EXTRA"}}}
	}
	plan := model.Plan{Name: "outcome", Tasks: []model.Task{task}, Providers: map[string]model.CommandSpec{"pool": {Command: []string{script}, Env: []string{"RECOVERY_EXTRA", "RECOVERY_RAW"}, Replay: "safe"}}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(plan, root, "mock"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 10 * time.Second, Budget: 1}); err != nil {
		t.Fatal(err)
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, s
}

func TestWorkerCommitsExplicitUncertaintyNotFailedForReplaySafeProvider(t *testing.T) {
	root, s := runOutcomeWorker(t, `,"outcome":"uncertain","error":"SECRET-RAW-ERROR"`, "", "", false)
	row := s.Tasks["a"]
	if !graph.ReplaySafe(s, row.Spec) {
		t.Fatal("fixture must be Replay=safe")
	}
	if row.Status != "uncertain" || row.Result != nil {
		t.Fatalf("explicit uncertainty must never be failed/produced/pending/cancelled: %+v", row)
	}
	// resume --retry-failed only admits status failed; plain retry also refuses.
	if row.Status == "failed" {
		t.Fatal("retry-failed could admit this node")
	}
	if err := retryNode(root, "a", false, false); err == nil || !strings.Contains(err.Error(), "outcome uncertain") {
		t.Fatalf("retry must refuse uncertain work: %v", err)
	}
	raw, _ := json.Marshal(s.Events)
	if strings.Contains(string(raw), "SECRET-RAW-ERROR") || strings.Contains(row.Error, "SECRET-RAW-ERROR") {
		t.Fatal("raw provider error leaked into state events")
	}
	seen := false
	for _, e := range s.Events {
		if e.Task == "a" && e.Kind == "failed" {
			t.Fatalf("unexpected failed event: %+v", e)
		}
		seen = seen || (e.Task == "a" && e.Kind == "outcome_uncertain")
	}
	if !seen {
		t.Fatal("missing outcome_uncertain event")
	}
}

func TestWorkerKnownAndLegacyOutcomesKeepExistingPolicy(t *testing.T) {
	for name, tc := range map[string]struct{ extra, status string }{
		"legacy error":      {`,"error":"plain failure"`, "failed"},
		"settled error":     {`,"outcome":"settled","error":"known"`, "failed"},
		"not started error": {`,"outcome":"not_started","error":"refused"`, "failed"},
		"legacy success":    {`,"output":{"x":"v"}`, "verified"},
		"settled success":   {`,"outcome":"settled","output":{"x":"v"}`, "verified"},
		"malformed value":   {`,"outcome":"maybe","output":{"x":"v"}`, "uncertain"},
		"uncertain output":  {`,"outcome":"uncertain","output":{"x":"v"}`, "uncertain"},
	} {
		t.Run(name, func(t *testing.T) {
			_, s := runOutcomeWorker(t, tc.extra, "", "", false)
			if got := s.Tasks["a"].Status; got != tc.status {
				t.Fatalf("status %s want %s", got, tc.status)
			}
		})
	}
	if _, s := runOutcomeWorker(t, "", `{bad`, "", false); s.Tasks["a"].Status != "uncertain" {
		t.Fatalf("malformed provider output must be uncertain: %s", s.Tasks["a"].Status)
	}
}

func TestWorkerValidatorUncertaintyIsPreservedOnError(t *testing.T) {
	_, s := runOutcomeWorker(t, `,"outcome":"settled","output":{"x":"v"}`, "", `,"outcome":"uncertain","error":"SECRET-VALIDATOR"`, true)
	row := s.Tasks["a"]
	if row.Status != "uncertain" || strings.Contains(row.Error, "SECRET-VALIDATOR") {
		t.Fatalf("%+v", row)
	}
	_, s = runOutcomeWorker(t, `,"outcome":"settled","output":{"x":"v"}`, "", `,"error":"nope"`, true)
	if s.Tasks["a"].Status != "failed" {
		t.Fatalf("legacy validator failure stays failed: %s", s.Tasks["a"].Status)
	}
	_, s = runOutcomeWorker(t, `,"outcome":"settled","output":{"x":"v"}`, "", `,"passed":true`, true)
	if s.Tasks["a"].Status != "verified" {
		t.Fatalf("%s", s.Tasks["a"].Status)
	}
}

func recLeaf(id, parent, status string) *model.TaskState {
	r := runfixture.LcRow(id, parent, status, "", nil)
	r.Error = "SECRET-RAW-ERROR"
	r.Spec.Prompt = "SECRET-PROMPT"
	return r
}

func TestLifecycleRecoveryDiagnosisTransitiveAndPrivate(t *testing.T) {
	r := runfixture.LcRow("r", "", "waiting", "task-checks", nil, "r__m", "r__s")
	m := runfixture.LcRow("r__m", "r", "waiting", "structural", nil, "r__m__a", "r__m__b", "r__m__c", "r__m__d", "r__m__e")
	s := runfixture.LcState(r, m, recLeaf("r__s", "r", "pending"), recLeaf("r__m__e", "r__m", "verified"), recLeaf("r__m__d", "r__m", "running"),
		recLeaf("r__m__c", "r__m", "cancelled"), recLeaf("r__m__b", "r__m", "uncertain"), recLeaf("r__m__a", "r__m", "failed"))
	s.Backend = "mock"
	lc := observer.ProjectLifecycles(s)
	for _, id := range []string{"r", "r__m"} {
		g := lcGet(t, lc, id)
		rec := g.Recovery
		if rec == nil || g.ExecutionStatus != "waiting" {
			t.Fatalf("%s must stay waiting with a diagnosis: %+v", id, g)
		}
		if rec.Classification != model.RecoveryUncertain || !reflect.DeepEqual(rec.BlockerIDs, []string{"r__m__a", "r__m__b", "r__m__c"}) || rec.BlockersTotal != 3 || rec.BlockersOmitted != 0 || rec.UncertainTotal != 1 || rec.FailedTotal != 1 || rec.CancelledTotal != 1 {
			t.Fatalf("%s: %+v", id, rec)
		}
	}
	root := lcGet(t, lc, "r").Recovery
	if root.Activity != model.RecoveryActive || !reflect.DeepEqual(root.ActiveIDs, []string{"r__m__d", "r__s"}) || root.ActiveTotal != 2 {
		t.Fatalf("active siblings are distinct from terminal blockers: %+v", root)
	}
	if mid := lcGet(t, lc, "r__m").Recovery; !reflect.DeepEqual(mid.ActiveIDs, []string{"r__m__d"}) {
		t.Fatalf("%+v", mid)
	}
	for _, id := range []string{"r__m__a", "r__m__e", "r__s"} {
		if lcGet(t, lc, id).Recovery != nil {
			t.Fatalf("%s is not a waiting owner", id)
		}
	}
	if r.Status != "waiting" || m.Status != "waiting" {
		t.Fatal("projection must not force parent failure")
	}
	raw, _ := json.Marshal(lc)
	for _, banned := range []string{"SECRET-RAW-ERROR", "SECRET-PROMPT"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("observer leaked %s", banned)
		}
	}
}

func TestLifecycleRecoveryBoundsAreDeterministic(t *testing.T) {
	build := func(reverse bool) *model.State {
		var failed, pending []string
		for i := 0; i < 12; i++ {
			failed = append(failed, "w__f"+string(rune('a'+i)))
		}
		for i := 0; i < 9; i++ {
			pending = append(pending, "w__p"+string(rune('a'+i)))
		}
		children := append(append([]string{}, failed...), pending...)
		rows := []*model.TaskState{}
		for _, id := range failed {
			rows = append(rows, runfixture.LcRow(id, "w", "failed", "", nil))
		}
		for _, id := range pending {
			rows = append(rows, runfixture.LcRow(id, "w", "pending", "", nil))
		}
		if reverse {
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
			for i, j := 0, len(children)-1; i < j; i, j = i+1, j-1 {
				children[i], children[j] = children[j], children[i]
			}
		}
		rows = append(rows, runfixture.LcRow("w", "", "waiting", "structural", nil, children...))
		return runfixture.LcState(rows...)
	}
	a := lcGet(t, observer.ProjectLifecycles(build(false)), "w").Recovery
	b := lcGet(t, observer.ProjectLifecycles(build(true)), "w").Recovery
	if a == nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("not deterministic:\n%+v\n%+v", a, b)
	}
	if a.Classification != model.RecoveryFailed || a.BlockersTotal != 12 || len(a.BlockerIDs) != model.LifecycleMaxIDs || a.BlockersOmitted != 4 || a.BlockerIDs[0] != "w__fa" || a.BlockerIDs[7] != "w__fh" {
		t.Fatalf("%+v", a)
	}
	if a.ActiveTotal != 9 || len(a.ActiveIDs) != model.LifecycleMaxIDs || a.ActiveOmitted != 1 || a.Activity != model.RecoveryActive {
		t.Fatalf("%+v", a)
	}
	if strings.Contains(a.Advice, "safe") {
		t.Fatalf("failed status must not be described as safely replayable: %s", a.Advice)
	}
}

func TestLifecycleRecoveryClassificationAndAbsence(t *testing.T) {
	cases := []struct {
		name, ownerStatus, childStatus, want string
	}{
		{"failed only", "waiting", "failed", model.RecoveryFailed},
		{"cancelled only", "waiting", "cancelled", model.RecoveryCancelled},
		{"uncertain", "waiting", "uncertain", model.RecoveryUncertain},
		{"verified children", "waiting", "verified", ""},
		{"active only", "waiting", "running", ""},
		{"not waiting owner", "pending", "failed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := runfixture.LcState(runfixture.LcRow("o", "", tc.ownerStatus, "structural", nil, "o__c"), runfixture.LcRow("o__c", "o", tc.childStatus, "", nil))
			s.Backend = "mock"
			rec := lcGet(t, observer.ProjectLifecycles(s), "o").Recovery
			if tc.want == "" {
				if rec != nil {
					t.Fatalf("unexpected diagnosis: %+v", rec)
				}
				return
			}
			if rec == nil || rec.Classification != tc.want || rec.Activity != model.RecoveryNoActive {
				t.Fatalf("%+v", rec)
			}
		})
	}
}
