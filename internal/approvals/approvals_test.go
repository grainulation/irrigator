package approvals

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func approvalLink(t *testing.T, root, request, action, ttl string) string {
	t.Helper()
	out := runfixture.ApprovalCapture(t, func() {
		code, err := ApprovalsCLI([]string{"link", "--run", root, "--request", request, "--action", action, "--ttl", ttl})
		if err != nil || code != 0 {
			t.Fatalf("link: %d %v", code, err)
		}
	})
	return strings.TrimSpace(out)
}

func approvalHandler(t *testing.T, root string) *approvalServer {
	t.Helper()
	s, err := NewApprovalServer(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func approvalDo(h http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func approvalLedger(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "approvals", "decisions.jsonl"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestApprovalsKeyIsPrivateAndLinkShape(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	link := approvalLink(t, root, "a", "approve", "5m")
	if !strings.HasPrefix(link, "/approve/") || strings.Count(link, ".") != 1 {
		t.Fatalf("unexpected link %q", link)
	}
	st, err := os.Stat(filepath.Join(root, "approvals", "key"))
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("key mode/size: %v %v", st, err)
	}
	if !strings.HasPrefix(approvalLink(t, root, "a", "deny", "1m"), "/deny/") {
		t.Fatal("deny link prefix")
	}
	if err = os.Chmod(filepath.Join(root, "approvals", "key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = NewApprovalServer(root); err == nil {
		t.Fatal("loose key permissions accepted")
	}
}

func TestApprovalsGetDoesNotDecide(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	link := approvalLink(t, root, "a", "approve", "5m")
	h := approvalHandler(t, root)
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		w := approvalDo(h, m, link)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v", m, w.Code, w.Header())
		}
	}
	if w := approvalDo(h, http.MethodGet, link); !strings.Contains(w.Body.String(), "<form method=post>") {
		t.Fatal("confirm page lacks POST form")
	}
	if approvalLedger(t, root) != "" {
		t.Fatal("GET recorded a decision")
	}
	// Still usable after any number of previews.
	if w := approvalDo(h, http.MethodPost, link); w.Code != 200 || !strings.Contains(approvalLedger(t, root), `"decision":"approve"`) {
		t.Fatalf("POST failed: %d %s", w.Code, w.Body)
	}
}

func TestApprovalsRejections(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	link := approvalLink(t, root, "a", "approve", "5m")
	h := approvalHandler(t, root)
	token := strings.TrimPrefix(link, "/approve/")
	body, sig, _ := strings.Cut(token, ".")
	flip := func(s string) string {
		if s[0] == 'A' {
			return "B" + s[1:]
		}
		return "A" + s[1:]
	}
	cases := map[string]string{
		"wrong action":  "/deny/" + token,
		"tampered body": "/approve/" + flip(body) + "." + sig,
		"tampered sig":  "/approve/" + body + "." + flip(sig),
		"no signature":  "/approve/" + body,
		"garbage":       "/approve/not-a-token",
		"extra path":    link + "/x",
		"unknown path":  "/status/" + token,
		"root":          "/",
	}
	for name, path := range cases {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			if w := approvalDo(h, m, path); w.Code != 403 && w.Code != 404 {
				t.Fatalf("%s %s accepted with %d", name, m, w.Code)
			}
		}
	}
	// A token signed by another run's key is rejected.
	other := runfixture.ApprovalLegacyRun(t)
	otherLink := approvalLink(t, other, "a", "approve", "5m")
	if w := approvalDo(h, http.MethodPost, otherLink); w.Code != 403 {
		t.Fatalf("foreign-run token accepted: %d", w.Code)
	}
	if w := approvalDo(h, http.MethodPut, link); w.Code != 405 {
		t.Fatalf("PUT: %d", w.Code)
	}
	if approvalLedger(t, root) != "" {
		t.Fatal("a rejected request recorded a decision")
	}
}

func TestApprovalsExpiry(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	vc := runfixture.NewFakeClock(runfixture.ClockEpoch)
	defer runfixture.SetClock(vc)()
	link := approvalLink(t, root, "a", "approve", "1m")
	h := approvalHandler(t, root)
	// Exact boundary: the token is valid through its expiry second, dead one
	// second later. A wall clock could never pin this deterministically.
	vc.Advance(time.Minute)
	if w := approvalDo(h, http.MethodGet, link); w.Code != 200 {
		t.Fatalf("link at its expiry second: %d %s", w.Code, w.Body)
	}
	vc.Advance(time.Second)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		w := approvalDo(h, m, link)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "expired") {
			t.Fatalf("%s expired link: %d %s", m, w.Code, w.Body)
		}
	}
	if approvalLedger(t, root) != "" {
		t.Fatal("expired token decided")
	}
}

func TestApprovalsSingleUseSurvivesRestart(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	link := approvalLink(t, root, "a", "deny", "5m")
	if w := approvalDo(approvalHandler(t, root), http.MethodPost, link); w.Code != 200 {
		t.Fatalf("first use: %d", w.Code)
	}
	// A fresh server (restart) shares nothing in memory with the first.
	h2 := approvalHandler(t, root)
	for _, m := range []string{http.MethodPost, http.MethodGet} {
		w := approvalDo(h2, m, link)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "already used") {
			t.Fatalf("%s reuse after restart: %d %s", m, w.Code, w.Body)
		}
	}
	if n := strings.Count(approvalLedger(t, root), "\n"); n != 1 {
		t.Fatalf("ledger has %d lines, want 1", n)
	}
}

func TestApprovalsConcurrentPostsDecideOnce(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	link := approvalLink(t, root, "a", "approve", "5m")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Separate servers model separate processes sharing the file lock.
			if approvalDo(approvalHandler(t, root), http.MethodPost, link).Code == 200 {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 || strings.Count(approvalLedger(t, root), "\n") != 1 {
		t.Fatalf("%d successes, ledger %q", ok, approvalLedger(t, root))
	}
}

func TestApprovalsMailboxRunEnqueuesSignal(t *testing.T) {
	root := runfixture.MailboxFixture(t)
	link := approvalLink(t, root, "a", "approve", "5m")
	h := approvalHandler(t, root)
	if w := approvalDo(h, http.MethodPost, link); w.Code != 200 {
		t.Fatalf("POST:%d %s", w.Code, w.Body)
	}
	s := runfixture.Read(t, root)
	if len(s.Mailbox.Entries) != 1 || s.Mailbox.Entries[0].Message.Kind != "signal" || s.Mailbox.Entries[0].Message.TaskID != "a" || !strings.Contains(s.Mailbox.Entries[0].Message.Payload, "approve") {
		t.Fatalf("mailbox entries: %+v", s.Mailbox.Entries)
	}
	if s.Tasks["a"].Status != "pending" || s.Tasks["a"].CancelRequested {
		t.Fatal("a deny/approve signal changed task execution")
	}
	if approvalLedger(t, root) != "" {
		t.Fatal("mailbox run also wrote the run-local decision ledger")
	}
	if w := approvalDo(approvalHandler(t, root), http.MethodPost, link); w.Code != 403 {
		t.Fatalf("reuse: %d", w.Code)
	}
	if err := messaging.ProcessMailbox(root, 4); err != nil {
		t.Fatal(err)
	}
	if got := runfixture.Read(t, root).Mailbox.Outbox; len(got) != 1 || got[0].Status != "applied" {
		t.Fatalf("outbox: %+v", got)
	}
}

func TestApprovalsUnknownRequestIsRefused(t *testing.T) {
	root := runfixture.MailboxFixture(t)
	code, err := ApprovalsCLI([]string{"link", "--run", root, "--request", "ghost", "--action", "approve"})
	if err == nil || code == 0 {
		t.Fatal("link for a request that is not a task was minted")
	}
}

func TestApprovalsLinkValidation(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	for _, args := range [][]string{
		{"link", "--run", root, "--request", "a", "--action", "maybe"},
		{"link", "--run", root, "--request", "a b", "--action", "approve"},
		{"link", "--run", root, "--request", "a", "--action", "approve", "--ttl", "0s"},
		{"link", "--run", root, "--request", "a", "--action", "approve", "--ttl", "48h"},
		{"link", "--request", "a", "--action", "approve"},
		{"nope"},
		{},
	} {
		if code, err := ApprovalsCLI(args); err == nil || code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestApprovalsRefusesNonLoopback(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	approvalLink(t, root, "a", "approve", "5m")
	for _, addr := range []string{"0.0.0.0:8787", ":8787", "192.168.1.5:8787", "[::]:8787", "example.com:80", "8787", "10.0.0.1:1"} {
		code, err := ApprovalsCLI([]string{"serve", "--run", root, "--addr", addr})
		if err == nil || code == 0 {
			t.Fatalf("serve accepted %q", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		if _, err := loopbackApprovalAddr(addr); err != nil {
			t.Fatalf("%q refused: %v", addr, err)
		}
	}
}

// serve must be able to start before any link exists: workers mint links only
// when an agent asks for permission.
func TestApprovalsServeBeforeFirstLink(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	h := approvalHandler(t, root) // no key yet
	st, err := os.Lstat(filepath.Join(root, "approvals", "key"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("serve did not create a private key: %v %v", st, err)
	}
	link := approvalLink(t, root, "a", "approve", "5m")
	if w := approvalDo(h, http.MethodPost, link); w.Code != http.StatusOK {
		t.Fatalf("link minted after serve started was refused: %d %s", w.Code, w.Body)
	}
}
