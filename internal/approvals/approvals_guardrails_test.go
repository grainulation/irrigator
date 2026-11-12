package approvals

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestApprovalExpiryRecheckedInsideDecisionLock(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	h := approvalHandler(t, root)
	now := time.Now()
	token, err := mintApprovalToken(h.key, h.runID, "a", "approve", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// First clock read is the HTTP precheck. The decision lock is acquired
	// before the second clock read, simulating expiry while awaiting the lock.
	calls := 0
	h.now = func() time.Time {
		calls++
		if calls == 1 {
			return now
		}
		return now.Add(2 * time.Second)
	}
	response := approvalDo(h, http.MethodPost, "/approve/"+token)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "expired") {
		t.Fatalf("expired queued decision accepted: %d", response.Code)
	}
	if approvalLedger(t, root) != "" {
		t.Fatal("expired decision recorded")
	}
}
func TestApprovalNewLinksRejectStaleAttempts(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	link, _, err := mintApprovalLink(root, "a", "approve", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Publish a legitimate reducer update using the existing checkpoint store.
	store := checkpoint.FileCheckpointStore{Root: root}
	if err := store.Update(t.Context(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error { s.Tasks["a"].Attempt++; return nil }); err != nil {
		t.Fatal(err)
	}
	h := approvalHandler(t, root)
	response := approvalDo(h, http.MethodPost, link)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "stale attempt") {
		t.Fatalf("stale link accepted: %d %s", response.Code, response.Body.String())
	}
	if approvalLedger(t, root) != "" {
		t.Fatal("stale grant recorded")
	}
}
