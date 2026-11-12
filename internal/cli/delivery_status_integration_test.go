//go:build integration

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/delivery"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runops"
)

type dstatEnv struct {
	base, root, dir, plan string
	opts                  model.Options
}

func dstatSetup(t *testing.T) dstatEnv {
	t.Helper()
	base := t.TempDir()
	p := model.Plan{Name: "delivery-status", Tasks: []model.Task{{ID: "ui", Prompt: "UI", Requires: []string{"files"}, MockResult: map[string]string{"files": `{"index.html":"hello"}`}}}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(base, "plan.json")
	if err = os.WriteFile(plan, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return dstatEnv{base: base, root: filepath.Join(base, "run"), dir: filepath.Join(base, "app"), plan: plan, opts: model.Options{Workers: 2, Timeout: time.Second, Budget: .1}}
}

func (e dstatEnv) deliver(check string, recheck bool) (int, error) {
	return delivery.Deliver(e.plan, e.root, "mock", e.dir, "ui", "files", check, 5*time.Second, recheck, e.opts)
}

// dstatCheck appends one line per acceptance execution outside the run and delivery.
func dstatCheck(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal([]string{"sh", "-c", `echo ran >> "$SWARM_RUN_DIR/../check-ran"`})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (e dstatEnv) ran(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.base, "check-ran"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "ran")
}

func (e dstatEnv) journalBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.root, "delivery.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (e dstatEnv) journal(t *testing.T) graph.DeliveryRecord {
	t.Helper()
	var rec graph.DeliveryRecord
	if err := json.Unmarshal(e.journalBytes(t), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (e dstatEnv) rewrite(t *testing.T, fn func(*graph.DeliveryRecord)) {
	t.Helper()
	rec := e.journal(t)
	fn(&rec)
	if err := jsonio.AtomicJSON(filepath.Join(e.root, "delivery.json"), rec); err != nil {
		t.Fatal(err)
	}
}

func (e dstatEnv) state(t *testing.T) *model.State {
	t.Helper()
	s, err := checkpoint.ReadState(e.root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e dstatEnv) project(t *testing.T) graph.DeliveryView {
	t.Helper()
	return graph.ProjectDelivery(e.root, e.state(t))
}

func dstatWant(t *testing.T, got graph.DeliveryView, stage, acceptance, recovery string) {
	t.Helper()
	if got.Stage != stage || got.Acceptance != acceptance || got.Recovery != recovery {
		t.Fatalf("projection %+v, want %s/%s/%s", got, stage, acceptance, recovery)
	}
}

func TestDeliveryProjectionMissingAndAllVerifiedIsNotDelivered(t *testing.T) {
	e := dstatSetup(t)
	p, err := checkpoint.LoadPlan(e.plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkpoint.Initialize(p, e.root, "mock"); err != nil {
		t.Fatal(err)
	}
	if code, runErr := runops.Run(e.root, e.opts); code != 0 || runErr != nil {
		t.Fatal(code, runErr)
	}
	s := e.state(t)
	if s.Control != "complete" {
		t.Fatal(s.Control)
	}
	dstatWant(t, graph.ProjectDelivery(e.root, s), "not_configured", "not_configured", "none")
}

func TestDeliveryProjectionAcceptanceFailureNeedsExplicitRecheck(t *testing.T) {
	e := dstatSetup(t)
	failing := `["sh","-c","exit 3"]`
	if code, err := e.deliver(failing, false); code == 0 || err == nil {
		t.Fatal("accepted failing check", code, err)
	}
	view := e.project(t)
	dstatWant(t, view, "failed", "failed", "explicit_recheck")
	encoded, _ := json.Marshal(view)
	var keys map[string]string
	if err := json.Unmarshal(encoded, &keys); err != nil || len(keys) != 3 {
		t.Fatal("view must expose exactly stage/acceptance/recovery", string(encoded))
	}
	if strings.Contains(string(encoded), e.root) || strings.Contains(string(encoded), "exit 3") {
		t.Fatal("projection leaked path or command")
	}
	before, attempt := e.journalBytes(t), e.state(t).Tasks["ui"].Attempt
	if _, err := e.deliver(failing, false); err == nil || !strings.Contains(err.Error(), "--recheck") {
		t.Fatal("silently reran failed acceptance", err)
	}
	if !bytes.Equal(before, e.journalBytes(t)) || e.state(t).Tasks["ui"].Attempt != attempt {
		t.Fatal("refused deliver mutated journal or replayed work")
	}
}

func TestDeliveryProjectionNoCheckIsNotAcceptancePassed(t *testing.T) {
	e := dstatSetup(t)
	if code, err := e.deliver("", false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	dstatWant(t, e.project(t), "accepted", "not_configured", "none")
}

func TestDeliveryInterruptedCheckIsNeverAutomaticallyRerun(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	dstatWant(t, e.project(t), "accepted", "passed", "none")
	if e.ran(t) != 1 {
		t.Fatal("check count", e.ran(t))
	}
	e.rewrite(t, func(r *graph.DeliveryRecord) { r.Status = "checking" })
	dstatWant(t, e.project(t), "unknown", "unknown", "explicit_recheck")
	// A live deliver holds delivery.lock: the same journal is then in-flight.
	lock, err := checkpoint.FileLock(filepath.Join(e.root, "delivery.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	dstatWant(t, e.project(t), "checking", "pending", "none")
	checkpoint.Unlock(lock)
	dstatWant(t, e.project(t), "unknown", "unknown", "explicit_recheck")
	before := e.journalBytes(t)
	if _, err = e.deliver(check, false); err == nil || !strings.Contains(err.Error(), "--recheck") {
		t.Fatal("reran interrupted acceptance", err)
	}
	if !bytes.Equal(before, e.journalBytes(t)) || e.ran(t) != 1 {
		t.Fatal("interrupted acceptance was mutated or rerun")
	}
}

// An explicit recheck that fails before the check starts (here the controller
// lock is busy) leaves the journal unchanged, so a plain deliver does not
// rerun the interrupted acceptance.
func TestDeliveryRecheckPreCheckFailureKeepsInterruptedGate(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	e.rewrite(t, func(r *graph.DeliveryRecord) { r.Status = "checking" })
	controller, err := checkpoint.FileLock(filepath.Join(e.root, "controller.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	if code, derr := e.deliver(check, true); code == 0 || derr == nil {
		t.Fatal("recheck ran while a controller held the lock", code, derr)
	}
	checkpoint.Unlock(controller)
	if status := e.journal(t).Status; status != "checking" {
		t.Fatal("pre-check failure downgraded interrupted acceptance to", status)
	}
	if _, derr := e.deliver(check, false); derr == nil || !strings.Contains(derr.Error(), "--recheck") {
		t.Fatal("plain deliver reran interrupted acceptance", derr)
	}
	if e.ran(t) != 1 {
		t.Fatal("acceptance reran without an explicit recheck")
	}
	if code, derr := e.deliver(check, true); code != 0 || derr != nil {
		t.Fatal("explicit recheck", code, derr)
	}
	if e.ran(t) != 2 || e.journal(t).Status != "accepted" {
		t.Fatal("explicit recheck did not complete once")
	}
}

func TestDeliveryStaleRevisionIsFencedAndJournalPreserved(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	e.rewrite(t, func(r *graph.DeliveryRecord) { r.GraphRevision = 5 })
	dstatWant(t, e.project(t), "stale", "unknown", "inspect")
	before := e.journalBytes(t)
	for _, recheck := range []bool{false, true} {
		if _, err := e.deliver(check, recheck); err == nil || !strings.Contains(err.Error(), "revision") {
			t.Fatal("reused stale acceptance", recheck, err)
		}
		if !bytes.Equal(before, e.journalBytes(t)) {
			t.Fatal("stale rejection rewrote the journal")
		}
	}
	if e.ran(t) != 1 {
		t.Fatal("stale revision reran acceptance")
	}
}

func TestDeliveryLocalOnlyContinuationDoesNotReplayPaidWork(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	attempt := e.state(t).Tasks["ui"].Attempt
	// Crash after export, before the acceptance check ever started.
	e.rewrite(t, func(r *graph.DeliveryRecord) { r.Status = "exported" })
	dstatWant(t, e.project(t), "exported", "pending", "resume_local")
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal("continuation", code, err)
	}
	dstatWant(t, e.project(t), "accepted", "passed", "none")
	// Crash during the graph phase of an already verified graph.
	e.rewrite(t, func(r *graph.DeliveryRecord) { r.Status = "graph_failed" })
	dstatWant(t, e.project(t), "failed", "pending", "resume_local")
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal("continuation", code, err)
	}
	if e.state(t).Tasks["ui"].Attempt != attempt {
		t.Fatal("local continuation replayed model work")
	}
	dstatWant(t, e.project(t), "accepted", "passed", "none")
}

func TestDeliveryStatusCLI(t *testing.T) {
	if code, err := graph.DeliveryStatusCLI(nil); code != 2 || err == nil {
		t.Fatal(code, err)
	}
	if code, err := graph.DeliveryStatusCLI([]string{"--json"}); code != 2 || err == nil {
		t.Fatal(code, err)
	}
	e := dstatSetup(t)
	if code, err := e.deliver("", false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if code, err := graph.DeliveryStatusCLI([]string{e.root}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
}
