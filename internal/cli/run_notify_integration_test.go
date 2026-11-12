//go:build integration

package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/approvals"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func runNotifyEnv(t *testing.T, server string) {
	t.Helper()
	runfixture.NotifyEnv(t, server)
	t.Setenv("IRRIGATOR_NOTIFY_VIA", "")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	old := approvals.ActiveNotify
	t.Cleanup(func() { approvals.ActiveNotify = old })
	approvals.ActiveNotify = approvals.RunNotifyFlags{}
}

func TestRunNotifyDetachedRunSendsCompleteOnceAcrossResume(t *testing.T) {
	srv, sent := notifyServer(t, 200)
	runNotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_NTFY_TOPIC", runfixture.RunNotifyTopic)
	withController(t, "")
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0, "a"))
	approvals.ActiveNotify = approvals.RunNotifyFlags{Via: "ntfy"}
	if code, err := runops.Run(root, runOptions()); err != nil || code != 0 {
		t.Fatalf("run: %d %v", code, err)
	}
	got := sent()
	if len(got) != 1 || got[0].path != "/"+runfixture.RunNotifyTopic {
		t.Fatalf("sent %+v", got)
	}
	body := got[0].body + got[0].header.Get("Title")
	for _, want := range []string{filepath.Base(root), "2/2 verified", "complete", "irrigator status"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %q", want, body)
		}
	}
	if got[0].header.Get("Actions") != "" {
		t.Fatal("status notice carries action buttons")
	}
	approvals.
		// Resuming a finished run changes no cell: no second message.
		ActiveNotify = approvals.RunNotifyFlags{}
	if code, err := runops.Run(root, runOptions()); err != nil || code != 0 {
		t.Fatalf("resume: %d %v", code, err)
	}
	if got := sent(); len(got) != 1 {
		t.Fatalf("resume of a finished run sent %d messages in total, want 1", len(got))
	}
	// A restarted controller that sees the same cells stays silent.
	n, _ := approvals.NewRunNotifier(root, approvals.RunNotifyFlags{}, os.Stderr)
	n.Observe(runfixture.Read(t, root))
	n.Flush()
	if got := sent(); len(got) != 1 {
		t.Fatalf("same state resent: %d", len(got))
	}
	// A new attempt that ends complete again is a new transition.
	s := runfixture.Read(t, root)
	s.Tasks["b"].Attempt++
	n.Observe(s)
	n.Flush()
	if got := sent(); len(got) != 2 {
		t.Fatalf("new attempt sent %d messages in total, want 2", len(got))
	}
}

func TestRunNotifyFailureIsLoggedAndHarmless(t *testing.T) {
	srv, sent := notifyServer(t, 500)
	runNotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_NTFY_TOPIC", runfixture.RunNotifyTopic)
	withController(t, "")
	root := runfixture.New(t, runfixture.Task("a", 0))
	approvals.ActiveNotify = approvals.RunNotifyFlags{Via: "ntfy"}
	if code, err := runops.Run(root, runOptions()); err != nil || code != 0 {
		t.Fatalf("a failing notifier changed the run: %d %v", code, err)
	}
	if len(sent()) != 1 {
		t.Fatalf("sent %d", len(sent()))
	}
	logged, err := os.ReadFile(filepath.Join(root, approvals.NotifyLogName))
	if err != nil || !strings.Contains(string(logged), "failed") || strings.Contains(string(logged), runfixture.RunNotifyTopic) {
		t.Fatalf("log %q %v", logged, err)
	}
	if runfixture.Read(t, root).Tasks["a"].Status != "verified" {
		t.Fatal("task state changed")
	}
}

type sentRequest struct {
	method string
	path   string
	header http.Header
	body   string
}

func notifyServer(t *testing.T, status int) (*httptest.Server, func() []sentRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []sentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, sentRequest{r.Method, r.URL.Path, r.Header.Clone(), string(body)})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []sentRequest { mu.Lock(); defer mu.Unlock(); return append([]sentRequest(nil), got...) }
}
