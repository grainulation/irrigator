//go:build integration

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
)

// Defect: a transient controller-lock failure on an already accepted delivery
// rewrote the journal to graph_failed, after which a plain deliver silently
// reran the acceptance check without --recheck.
func TestDeliveryStatusAcceptedSurvivesTransientControllerLock(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	before := e.journalBytes(t)
	controller, err := checkpoint.FileLock(filepath.Join(e.root, "controller.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	code, derr := e.deliver(check, false)
	checkpoint.Unlock(controller)
	if code == 0 || derr == nil {
		t.Fatal("deliver succeeded while a controller held the lock", code, derr)
	}
	if !bytes.Equal(before, e.journalBytes(t)) {
		t.Fatal("transient failure rewrote the accepted journal")
	}
	dstatWant(t, e.project(t), "accepted", "passed", "none")
	if code, derr = e.deliver(check, false); code != 0 || derr != nil {
		t.Fatal("plain deliver after lock release", code, derr)
	}
	if e.ran(t) != 1 {
		t.Fatal("acceptance reran without --recheck", e.ran(t))
	}
}

// Execution state that is no longer complete reads stale; it neither rewrites
// the accepted journal nor reruns the check once the graph is complete again.
func TestDeliveryStatusIncompleteGraphKeepsAcceptedEvidence(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	before := e.journalBytes(t)
	setControl := func(v string) {
		if err := checkpoint.Update(e.root, func(s *model.State) error { s.Control = v; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setControl("run")
	dstatWant(t, e.project(t), "stale", "unknown", "inspect")
	if _, err := e.deliver(check, false); err == nil {
		t.Fatal("delivered an incomplete graph")
	}
	if !bytes.Equal(before, e.journalBytes(t)) {
		t.Fatal("incomplete graph rewrote the accepted journal")
	}
	setControl("complete")
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if e.ran(t) != 1 {
		t.Fatal("acceptance reran without --recheck")
	}
}

// Verified, exported and accepted are distinct; uncertainty blocks all of them
// and is never retried or reported as delivered.
func TestDeliveryStatusStagesDistinctAndUncertaintyBlocks(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	e.rewrite(t, func(r *graph.DeliveryRecord) { r.Status = "exported" })
	view := e.project(t)
	dstatWant(t, view, "exported", "pending", "resume_local")
	if view.Acceptance == "passed" || view.Stage == "accepted" {
		t.Fatal("exported read as accepted")
	}
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	dstatWant(t, e.project(t), "accepted", "passed", "none")
	attempt := e.state(t).Tasks["ui"].Attempt
	checksBeforeUncertainty := e.ran(t)
	if err := checkpoint.Update(e.root, func(s *model.State) error { s.Tasks["ui"].Status = "uncertain"; return nil }); err != nil {
		t.Fatal(err)
	}
	dstatWant(t, e.project(t), "blocked", "unknown", "reconcile_external")
	before := e.journalBytes(t)
	if _, err := e.deliver(check, false); err == nil {
		t.Fatal("delivered over an uncertain node")
	}
	if !bytes.Equal(before, e.journalBytes(t)) || e.state(t).Tasks["ui"].Attempt != attempt || e.ran(t) != checksBeforeUncertainty {
		t.Fatal("uncertain node was retried, rewritten or rechecked")
	}
}

// Tampering with delivered files is never accepted and never reads as passed.
func TestDeliveryStatusTamperIsNotAccepted(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "index.html"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := e.deliver(check, false); code == 0 || err == nil {
		t.Fatal("accepted tampered delivery", code, err)
	}
	if e.project(t).Acceptance == "passed" || e.ran(t) != 1 {
		t.Fatal("tampered delivery read as passed or reran the check")
	}
}
