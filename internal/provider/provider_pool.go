package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/providerenv"
)

type ProviderPoolOptions struct {
	listen, Dir, Model, Claude string
	workers                    int
	Budget                     float64
}
type ProviderConnection struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// settleResponseCost resolves the response's reported charge. A valid cost settles
// normally. An invalid cost (negative, overflowing, non-finite, or an ambiguous
// micros/USD conflict) is unknown billing: the response is rewritten to an
// uncertain outcome with no output and an UNKNOWN cost (CostUnknown, zero
// CostUSD/CostMicros), and ok is false so the caller records the operation as
// uncertain rather than completed. The returned amount is the genuinely observed
// charge floor, which is zero here: an invalid statement proves nothing, and the
// held reservation is accounting only (providerReceipt.Reserved*), never a
// reported charge, so it is neither read nor published here. The invalid values
// themselves are never persisted or echoed.
func settleResponseCost(response model.WorkResponse) (model.WorkResponse, int64, bool) {
	amount, err := response.ResolveCost()
	if err == nil {
		return response, amount, true
	}
	response.Output = nil
	response.ContextReceipt = nil
	response.CostMicros, response.CostMicrosExplicit = 0, false
	response.CostUSD = 0
	response.CostUnknown = true
	response.CostBasis = "unknown"
	response.DiagnosticCode = ""
	response.Outcome = model.OutcomeUncertain
	response.Error = "provider reported an invalid cost; billing is unknown and the full reservation is retained; automatic replay is prohibited"
	return response, 0, false
}

// poolAdmits is the admission rule: spent plus reserved plus the request must
// not exceed the allocation. Exactly equal is admitted; one micro over is not.
func poolAdmits(usedMicros, requestMicros, allocationMicros int64) bool {
	sum, ok := cost.Add(usedMicros, requestMicros)
	return ok && sum <= allocationMicros
}

// budgetMicros is the configured service allocation in micros.
func (p *ProviderPool) budgetMicros() int64 {
	m, _ := cost.Limit(p.Opts.Budget)
	return m
}

type ProviderPool struct {
	Opts           ProviderPoolOptions
	mu             sync.Mutex
	Ledger         observer.ProviderLedger
	Slots          chan *ProviderSlot
	startupPending map[string]bool // only unfinished operations loaded at startup, never live admissions
}
type ProviderSlot struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	Lines chan []byte
	done  chan error
	key   string
	cost  int64 // cumulative session cost, micros
	cap   int64 // cumulative session cap, micros
	turns int
	apiMS int64
}

// serveProvider owns the durable operation ledger and a bounded pool of persistent CLI conversations.
func serveProvider(ctx context.Context, opts ProviderPoolOptions) error {
	if opts.workers < 1 || opts.workers > 100 || !model.ValidCallBudget(opts.Budget) {
		return errors.New("workers must be 1..100 and budget must be finite and positive")
	}
	if opts.listen == "" {
		opts.listen = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(opts.listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("provider must listen on a literal loopback address")
	}
	if opts.Dir == "" {
		return errors.New("provider directory is required")
	}
	if opts.Model == "" {
		opts.Model = "haiku"
	}
	if opts.Claude == "" {
		opts.Claude = "claude"
	}
	if err = os.MkdirAll(opts.Dir, 0700); err != nil {
		return err
	}
	lock, err := checkpoint.FileLock(filepath.Join(opts.Dir, "provider.lock"), true)
	if err != nil {
		return err
	}
	defer checkpoint.Unlock(lock)
	p := &ProviderPool{Opts: opts, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}, Slots: make(chan *ProviderSlot, opts.workers)}
	data, err := os.ReadFile(filepath.Join(opts.Dir, "ledger.json"))
	if err == nil {
		if err = json.Unmarshal(data, &p.Ledger); err != nil {
			return err
		}
		if p.Ledger.Receipts == nil {
			return errors.New("invalid provider ledger")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for id, receipt := range p.Ledger.Receipts {
		if err := validateProviderConversation(receipt.Conversation); err != nil {
			return err
		}
		if id == "" || len(receipt.Hash) != 64 || !cost.ValidMicrosFloat(receipt.Reserved) || (receipt.Status != "pending" && receipt.Status != "uncertain" && receipt.Status != "completed") {
			return errors.New("invalid provider ledger receipt")
		}
		if receipt.CostUnknown && receipt.Status != "uncertain" {
			// Unknown billing can only belong to an unresolved operation.
			return errors.New("invalid provider ledger receipt")
		}
		if receipt.Status == "completed" {
			// A completed receipt must settle to a valid, matching cost. An invalid
			// cost is not zero: refuse the ledger rather than shrink its reservation.
			settled, costErr := receipt.Response.ResolveCost()
			if costErr != nil || receipt.ReservedAmount() != settled {
				return errors.New("invalid provider ledger receipt")
			}
		}
	}
	p.RestoreAdmissionBarrier()
	for i := 0; i < opts.workers; i++ {
		p.Slots <- &ProviderSlot{}
	}
	defer func() {
		for i := 0; i < opts.workers; i++ {
			s := <-p.Slots
			s.Stop()
		}
	}()
	if err = observer.PrepareProviderBudgetIdentity(opts.Dir, opts.Budget); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	conn := ProviderConnection{URL: "http://" + listener.Addr().String(), Token: hex.EncodeToString(token)}
	config := filepath.Join(opts.Dir, "connection.json")
	raw, _ := json.Marshal(conn)
	if err = os.WriteFile(config, raw, 0600); err != nil {
		return err
	}
	if err = os.Chmod(config, 0600); err != nil {
		return err
	}
	gate := make(chan struct{}, opts.workers*2)
	mux := http.NewServeMux()
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+conn.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		default:
			http.Error(w, "provider queue full", http.StatusTooManyRequests)
			return
		}
		var req model.WorkRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, model.MaxInputBytes))
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			http.Error(w, "trailing request data", 400)
			return
		}
		response := p.Work(r.Context(), req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	shutdownDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-shutdownDone:
		}
	}()
	defer close(shutdownDone)
	fmt.Fprintln(os.Stdout, config)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// providerSessionHeadroomCovers reports whether the remaining cumulative CLI session cap can cover
// the requested per-operation reservation. Reuse also requires that remaining
// authority does not exceed that reservation (providerSessionAuthorityFits).
func ProviderSessionHeadroomCovers(sessionCap, sessionCost, reserve int64) bool {
	return sessionCap-sessionCost >= reserve
}

// A reused cumulative cap must not grant more spending authority than the new
// operation reserved.
func providerSessionAuthorityFits(sessionCap, sessionCost, reserve int64) bool {
	return ProviderSessionHeadroomCovers(sessionCap, sessionCost, reserve) && sessionCap-sessionCost <= reserve
}

// Startup recovery does not replay unfinished calls. Metrics are an observed
// charge floor; missing metrics retain the entire reservation, not a zero invoice.
func (p *ProviderPool) RestoreAdmissionBarrier() {
	p.startupPending = map[string]bool{}
	for id, receipt := range p.Ledger.Receipts {
		if receipt.Status == "pending" {
			p.startupPending[id] = true
			p.Ledger.AdmissionBlocked = true
		}
		if receipt.Status == "uncertain" {
			// An unreadable cost never lowers the retained reservation.
			if observed, costErr := receipt.Response.ResolveCost(); costErr != nil {
				fmt.Fprintf(os.Stderr, "warning: provider operation %q recorded an invalid cost; retaining the full reservation\n", id)
				// Sanitize to unknown billing (never a charge) and remember it durably.
				receipt.Response, _, _ = settleResponseCost(receipt.Response)
				receipt.CostUnknown = true
				p.Ledger.Receipts[id] = receipt
			} else if observed > receipt.ReservedAmount() {
				receipt.SetReserved(observed)
				p.Ledger.Receipts[id] = receipt
				p.Ledger.AdmissionBlocked = true
			}
			fmt.Fprintf(os.Stderr, "warning: provider operation %q has unresolved billing; retaining $%s, not a final invoice\n", id, cost.FormatMicros(receipt.ReservedAmount()))
		}
	}
	if err := p.recoverStartupPending(); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
}

func (p *ProviderPool) recoverStartupPending() error {
	for id := range p.startupPending {
		receipt := p.Ledger.Receipts[id]
		observed, found, err := p.observedOperationCost(id)
		if err != nil {
			return fmt.Errorf("provider admission blocked: malformed accounting evidence for operation %q; repair its metrics and retry: %w", id, err)
		}
		if observed > receipt.ReservedAmount() {
			receipt.SetReserved(observed)
		}
		receipt.Status = "uncertain"
		receipt.Response = model.WorkResponse{Protocol: model.ProtocolVersion, Outcome: model.OutcomeUncertain, CostUSD: cost.MicrosToFloat(observed), Error: "interrupted provider operation; billing remains uncertain and automatic replay is prohibited"}
		if found {
			// A valid durable observation (including an explicit zero) is kept as-is.
			receipt.CostUnknown = false
		} else {
			// No monetary observation: billing is unknown. The full reservation stays
			// in receipt.Reserved* only; it is neither reported nor replayed as spend.
			receipt.Response.CostUSD = 0
			receipt.Response.CostUnknown = true
			receipt.Response.CostBasis = "unknown"
			receipt.CostUnknown = true
			receipt.Response.Error += "; no durable cost observation: retained reservation is not an invoice cap"
		}
		p.Ledger.Receipts[id] = receipt
		delete(p.startupPending, id)
		fmt.Fprintf(os.Stderr, "warning: provider operation %q was interrupted; retaining $%s (durable cost observation: %t); actual invoice may exceed the declared admission budget; no automatic replay\n", id, cost.FormatMicros(receipt.ReservedAmount()), found)
	}
	return nil
}

func (p *ProviderPool) observedOperationCost(id string) (int64, bool, error) {
	path := filepath.Join(p.Opts.Dir, fmt.Sprintf("operation-%x.metrics.json", sha256.Sum256([]byte(id))))
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > model.MaxOutputBytes {
		return 0, false, errors.New("metrics must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, model.MaxOutputBytes+1))
	if err != nil {
		return 0, false, err
	}
	if len(raw) > model.MaxOutputBytes {
		return 0, false, errors.New("metrics exceed size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := jsonio.ParseStrictJSON(decoder, 0)
	if err != nil {
		return 0, false, errors.New("malformed metrics JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return 0, false, errors.New("trailing metrics JSON")
	}
	object, ok := value.(map[string]any)
	if !ok || object["operation_id"] != id {
		return 0, false, errors.New("metrics operation identity mismatch")
	}
	number, ok := object["cost_usd"].(json.Number)
	if !ok {
		return 0, false, errors.New("metrics require an explicit numeric cost")
	}
	cost, err := cost.ParseMicros(number.String())
	if err != nil {
		return 0, false, errors.New("invalid metrics cost")
	}
	return cost, true, nil
}

// Called under p.mu. A transient publication failure clears only after a complete
// repaired ledger is durable. Live pending operations remain pending throughout.
func (p *ProviderPool) repairPublication() error {
	if !p.Ledger.AdmissionBlocked {
		return nil
	}
	if err := p.recoverStartupPending(); err != nil {
		return err
	}
	p.Ledger.AdmissionBlocked = false
	if err := p.save(); err != nil {
		p.Ledger.AdmissionBlocked = true
		return fmt.Errorf("provider admission blocked: ledger repair failed; restore writable storage and retry (cached results remain available): %w", err)
	}
	fmt.Fprintln(os.Stderr, "warning: provider receipt storage recovered; unrelated admissions may continue within remaining reserved budget; uncertain operations will not be replayed")
	return nil
}

// reservedCost is spent plus reserved across the ledger, in micros. It
// saturates rather than wraps so a corrupt ledger can only refuse admission.
func (p *ProviderPool) ReservedCost() int64 {
	var used int64
	for _, receipt := range p.Ledger.Receipts {
		sum, ok := cost.Add(used, receipt.ReservedAmount())
		if !ok {
			return math.MaxInt64
		}
		used = sum
	}
	return used
}

func (p *ProviderPool) save() error {
	return jsonio.AtomicJSON(filepath.Join(p.Opts.Dir, "ledger.json"), p.Ledger)
}
func (p *ProviderPool) Work(ctx context.Context, req model.WorkRequest) model.WorkResponse {
	started := time.Now()
	response := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID}
	fail := func(err error) model.WorkResponse {
		response.Error = err.Error()
		response.Outcome = model.OutcomeNotStarted
		return response
	}
	if req.Protocol != model.ProtocolVersion || req.Kind != "task" || req.OperationID == "" || req.RequestID == "" || !model.ValidCallBudget(req.MaxCostUSD) || req.TimeoutMS <= 0 || req.TimeoutMS > 3600000 {
		return fail(errors.New("invalid provider request identity, budget, or timeout"))
	}
	if err := launch.ValidateWorkerLaunchContext(req); err != nil {
		return fail(err)
	}
	// Validate the output contract before reserving budget or launching a model.
	contract := req.Task
	contract.Turn = nil // Turn identity was validated at runtime admission, not this standalone output contract.
	contract.DependsOn, contract.InputFields, contract.Parent, contract.Provider = nil, nil, "", ""
	if err := checkpoint.Validate(model.Plan{Tasks: []model.Task{contract}, Context: req.Context}); err != nil {
		return fail(fmt.Errorf("invalid provider task: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	// Request IDs and attempt numbers may change on retries; execution semantics must not.
	canonical := req
	canonical.RequestID = ""
	canonical.Attempt = 0
	// Delivery metadata is per attempt, not a change in execution semantics.
	canonical.LaunchContext = nil
	encoded, _ := json.Marshal(canonical)
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	attribution := observer.ProviderReceiptAttribution(req)
	p.mu.Lock()
	if old, ok := p.Ledger.Receipts[req.OperationID]; ok {
		p.mu.Unlock()
		if old.Hash != digest {
			return model.WithReceiptOutcome(fail(errors.New("operation ID payload conflict")), old)
		}
		if old.Status != "completed" {
			replay := model.OutcomeResponse(fail(errors.New("operation pending or uncertain; automatic replay prohibited")), model.OutcomeUncertain)
			// An unknown invoice stays unknown on replay: never a known zero.
			replay.CostUnknown = old.CostUnknown || old.Response.CostUnknown
			// Genuinely observed monetary evidence (a positive floor or an explicit
			// zero) survives replay separately from the held reservation, which is
			// never a charge. Invalid stored costs stay unknown; raw values are not copied.
			if amount, err := old.Response.ResolveCost(); err != nil {
				replay.CostUnknown = true
			} else if !replay.CostUnknown && (amount > 0 || old.Response.CostMicrosExplicit) {
				replay.CostMicros, replay.CostMicrosExplicit = amount, old.Response.CostMicrosExplicit
				replay.CostUSD, replay.CostBasis = cost.MicrosToFloat(amount), "reported"
			}
			if replay.CostUnknown {
				replay.CostBasis = "unknown"
			}
			return replay
		}
		old.Response.RequestID = req.RequestID
		// A cached result did not consume this attempt's new context.
		if old.Response.ContextReceipt != nil && (req.LaunchContext == nil || old.Response.ContextReceipt.Digest != req.LaunchContext.Digest) {
			old.Response.ContextReceipt = nil
		}
		return old.Response
	}
	if err := p.repairPublication(); err != nil {
		p.mu.Unlock()
		return fail(err)
	}
	conversation, err := p.PrepareConversation(req)
	if err != nil {
		p.mu.Unlock()
		return fail(err)
	}
	prompt, err := ProviderTaskPrompt(req, conversation)
	if err != nil {
		p.mu.Unlock()
		return fail(err)
	}
	requestMicros, _ := cost.MicrosFromFloat(req.MaxCostUSD)
	if !poolAdmits(p.ReservedCost(), requestMicros, p.budgetMicros()) {
		p.mu.Unlock()
		return fail(errors.New("provider budget exhausted by spent or reserved operations"))
	}
	p.Ledger.Receipts[req.OperationID] = model.ProviderReceipt{Hash: digest, Status: "pending", Reserved: cost.MicrosToFloat(requestMicros), ReservedMicros: requestMicros, Attribution: attribution, Conversation: conversation}
	if err := p.save(); err != nil {
		delete(p.Ledger.Receipts, req.OperationID)
		p.mu.Unlock()
		return fail(err)
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
	defer cancel()
	var slot *ProviderSlot
	select {
	case slot = <-p.Slots:
	case <-ctx.Done():
		return p.Finish(req, digest, fail(ctx.Err()), true)
	}
	defer func() { p.Slots <- slot }()
	// A queued request rechecks the barrier when it acquires a worker slot.
	p.mu.Lock()
	repairErr := p.repairPublication()
	overBudget := p.ReservedCost() > p.budgetMicros()
	p.mu.Unlock()
	if repairErr != nil {
		return p.Finish(req, digest, fail(repairErr), true)
	}
	if overBudget {
		return p.Finish(req, digest, fail(errors.New("provider budget exhausted after accounting repair")), true)
	}

	schema := artifactstore.ModelSchemaForRequest(req)
	schemaJSON, _ := json.Marshal(schema)
	partition, _ := json.Marshal([]any{req.RunID, req.Context, req.Dependencies})
	if req.RunID == "" {
		partition = []byte(req.OperationID)
	}
	key := string(partition) + string(schemaJSON) + "/" + req.Task.Effort
	if req.Task.Turn != nil {
		key += "/turn/" + req.OperationID
	}
	// V2 has explicit, durable history. Fresh sessions prevent implicit stale or
	// cross-lane transcript retention and preserve per-operation spending caps.
	if slot.cmd != nil && (req.Task.Turn != nil || slot.key != key || slot.turns >= 8 || !providerSessionAuthorityFits(slot.cap, slot.cost, requestMicros)) {
		slot.Stop()
	}
	if slot.cmd == nil {
		if err = slot.Start(p.Opts, key, string(schemaJSON), req.Task.Effort, requestMicros); err != nil {
			return p.Finish(req, digest, fail(err), true)
		}
	}
	message := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}}
	payload, _ := json.Marshal(message)
	payload = append(payload, '\n')
	written := make(chan error, 1)
	go func() { _, e := slot.in.Write(payload); written <- e }()
	select {
	case err = <-written:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		slot.Stop()
		return p.Finish(req, digest, fail(err), false)
	}
	for {
		select {
		case <-ctx.Done():
			slot.Stop()
			return p.Finish(req, digest, fail(ctx.Err()), false)
		case line, ok := <-slot.Lines:
			if !ok {
				slot.Stop()
				return p.Finish(req, digest, fail(errors.New("provider stream ended before result; operation uncertain")), false)
			}
			var event struct {
				Type        string          `json:"type"`
				Error       bool            `json:"is_error"`
				Result      string          `json:"result"`
				Structured  json.RawMessage `json:"structured_output"`
				Cost        json.RawMessage `json:"total_cost_usd"`
				SessionID   string          `json:"session_id"`
				Usage       json.RawMessage `json:"usage"`
				ModelUsage  json.RawMessage `json:"modelUsage"`
				DurationAPI int64           `json:"duration_api_ms"`
			}
			if err = json.Unmarshal(line, &event); err != nil {
				slot.Stop()
				return p.Finish(req, digest, fail(errors.New("malformed provider stream; operation uncertain")), false)
			}
			if event.Type != "result" {
				continue
			}
			cumulative, costErr := cost.ParseMicros(string(event.Cost))
			if costErr != nil || cumulative < slot.cost {
				slot.Stop()
				return p.Finish(req, digest, fail(errors.New("invalid cumulative provider cost")), false)
			}
			response.CostUSD = cost.MicrosToFloat(cumulative - slot.cost)
			var resultMeta struct {
				Subtype string `json:"subtype"`
			}
			_ = json.Unmarshal(line, &resultMeta)
			slot.cost = cumulative
			slot.turns++
			metricPath := filepath.Join(p.Opts.Dir, fmt.Sprintf("operation-%x.metrics.json", sha256.Sum256([]byte(req.OperationID))))
			metricErr := jsonio.AtomicJSON(metricPath, map[string]any{"operation_id": req.OperationID, "session_id": event.SessionID, "pid": slot.cmd.Process.Pid, "session_turn": slot.turns, "elapsed_ms": time.Since(started).Milliseconds(), "duration_api_ms": event.DurationAPI - slot.apiMS, "cumulative_duration_api_ms": event.DurationAPI, "cost_usd": response.CostUSD, "cumulative_session_cost_usd": cost.MicrosToFloat(slot.cost), "usage": event.Usage, "cumulative_model_usage": event.ModelUsage})
			slot.apiMS = event.DurationAPI
			if model.ProviderResultFailure(event.Error, resultMeta.Subtype) {
				response.DiagnosticCode, response.Error = model.ProviderResultDiagnostic(resultMeta.Subtype)
				if response.DiagnosticCode == model.DiagProviderSessionBudget {
					slot.Stop() // the cumulative CLI session cap is spent; never reuse this session
				}
			} else {
				raw := event.Structured
				if len(raw) == 0 {
					raw = json.RawMessage(event.Result)
				}
				response.Output, err = artifactstore.NormalizeModelOutput(req.Task, raw)
				if err != nil {
					response.Error = err.Error()
				}
			}
			if metricErr != nil {
				response.Error = "provider metrics persistence failed: " + metricErr.Error()
				response.DiagnosticCode = ""
				response.Output = nil
			}
			if cost.CostExceeds(response.CostUSD, req.MaxCostUSD) {
				response.Error = "provider exceeded reserved per-operation budget"
				response.DiagnosticCode = model.DiagProviderOperationBudget
				slot.Stop()
			}
			if req.LaunchContext != nil {
				response.ContextReceipt = &model.ContextReceipt{Schema: launch.ContextReceiptSchema, Digest: req.LaunchContext.Digest, Consumer: "irrigator.provider-adapter"}
			}
			return p.Finish(req, digest, response, true)
		}
	}
}
func (p *ProviderPool) Finish(req model.WorkRequest, hash string, response model.WorkResponse, known bool) model.WorkResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.Ledger.Receipts[req.OperationID]
	receipt := previous
	receipt.Hash = hash
	// Resolve the charge before settling: an invalid cost must keep the held
	// reservation and become uncertain with UNKNOWN cost, never a zero-cost
	// completion and never a charge equal to the reservation.
	response, settled, costValid := settleResponseCost(response)
	if !costValid {
		known = false
	}
	// Billing certainty is distinct from execution certainty. After handoff
	// (known=false) with no monetary observation (no positive charge and no
	// explicit zero), the numeric zero is absence of evidence, not a known-zero
	// invoice. A preexisting CostUnknown marker is always honored. Pre-dispatch
	// failures (known=true) and genuine observations are untouched.
	unknownBilling := !costValid || response.CostUnknown
	if !known && !response.CostMicrosExplicit && response.CostMicros == 0 && response.CostUSD == 0 {
		unknownBilling = true
	}
	if unknownBilling {
		// Unknown billing carries no numeric charge; the held reservation stays
		// only in receipt.Reserved*, never as spend.
		response.CostUnknown = true
		response.CostBasis = "unknown"
		response.CostMicros, response.CostMicrosExplicit, response.CostUSD = 0, false, 0
		settled = 0
		known = false
	}
	receipt.CostUnknown = unknownBilling
	response.Outcome = model.FinishOutcome(response.Outcome, known)
	receipt.Response = response
	if known && response.Error == "" && response.Outcome != model.OutcomeUncertain && receipt.Conversation != nil {
		c := *receipt.Conversation
		c.Entries = append([]model.ProviderConversationEntry(nil), c.Entries...)
		c.Entries[len(c.Entries)-1].Output = response.Output
		c.Digest = ConversationDigest(c)
		if err := validateProviderConversation(&c); err != nil {
			// A result may be larger than the history budget. Keep the durable
			// operation result, but fail closed for future history inheritance.
			receipt.Conversation = nil
		} else {
			receipt.Conversation = &c
		}
	}
	if known {
		receipt.Status = "completed"
		receipt.SetReserved(settled)
	} else {
		receipt.Status = "uncertain"
		if settled > receipt.ReservedAmount() {
			receipt.SetReserved(settled)
		}
	}
	p.Ledger.Receipts[req.OperationID] = receipt
	if err := p.save(); err != nil {
		response.Output = nil
		response.Outcome = model.OutcomeUncertain
		response.Error = "provider receipt persistence failed: " + err.Error() + "; operation remains uncertain; unrelated work resumes only after durable ledger repair"
		fmt.Fprintln(os.Stderr, "warning:", response.Error)
		p.Ledger.AdmissionBlocked = true
		previous.Status = "uncertain"
		previous.CostUnknown = unknownBilling
		if settled > previous.ReservedAmount() {
			previous.SetReserved(settled)
		}
		previous.Response = response
		p.Ledger.Receipts[req.OperationID] = previous
	}
	return response
}
func (s *ProviderSlot) Start(opts ProviderPoolOptions, key, schema, effort string, cap int64) error {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--safe-mode", "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence", "--model", opts.Model, "--json-schema", schema, "--max-budget-usd", cost.FormatMicrosTrim(cap), "--system-prompt", "You execute independent coding tasks and return structured artifacts. Use only supplied context. Tools are disabled."}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	cmd := exec.Command(opts.Claude, args...)
	cmd.Env = providerenv.Claude()
	cmd.Dir = opts.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return err
	}
	s.cmd = cmd
	s.in = in
	s.key = key
	s.cost = 0
	s.cap = cap
	s.turns = 0
	s.apiMS = 0
	s.Lines = make(chan []byte, 8)
	s.done = make(chan error, 1)
	commandflags.GoLabeled(func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), model.MaxOutputBytes)
		for scanner.Scan() {
			s.Lines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(s.Lines)
		s.done <- cmd.Wait()
	}, "irrigator.role", "provider-reader", "provider.model", opts.Model)
	return nil
}
func (s *ProviderSlot) Stop() {
	if s.cmd == nil {
		return
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	_ = s.in.Close()
	for range s.Lines {
	}
	<-s.done //nolint:errcheck // stopping: wait for the process to exit; its status is not needed
	s.cmd = nil
	s.cost = 0
}
func ConnectProvider(ctx context.Context, path string, input io.Reader, output io.Writer) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("provider connection file must be private (0600)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var conn ProviderConnection
	if err = json.Unmarshal(data, &conn); err != nil {
		return err
	}
	endpoint, parseErr := url.Parse(conn.URL)
	if parseErr != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.Port() == "" || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("provider URL must be a plain loopback HTTP origin")
	}
	port, portErr := strconv.Atoi(endpoint.Port())
	if portErr != nil || port < 1 || port > 65535 {
		return errors.New("invalid provider port")
	}
	ip := net.ParseIP(endpoint.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("provider URL must be loopback")
	}
	body, err := io.ReadAll(io.LimitReader(input, model.MaxInputBytes+1))
	if err != nil {
		return err
	}
	if len(body) > model.MaxInputBytes {
		return errors.New("request too large")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", conn.URL+"/work", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Hour, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect prohibited") }}
	res, err := client.Do(req)
	if err != nil && !model.ConnectNeverSent(err) {
		return model.LostProviderResponse(body, output)
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if res.StatusCode >= 500 {
			return model.LostProviderResponse(body, output)
		}
		return fmt.Errorf("provider HTTP status %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, model.MaxOutputBytes+1))
	if err != nil {
		return model.LostProviderResponse(body, output)
	}
	if err != nil {
		return err
	}
	if len(raw) > model.MaxOutputBytes {
		return model.LostProviderResponse(body, output)
	}
	_, err = output.Write(raw)
	return err
}
