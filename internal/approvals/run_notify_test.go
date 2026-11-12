package approvals

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRunNotifyPrecedenceFlagRecordEnvOff(t *testing.T) {
	runNotifyEnv(t, "http://127.0.0.1:1")
	root := t.TempDir()
	if rec, record, err := chooseRunNotify(root, RunNotifyFlags{}); err != nil || rec.Via != "off" || record {
		t.Fatalf("default: %+v %v %v", rec, record, err)
	}
	t.Setenv("IRRIGATOR_NOTIFY_VIA", "webhook")
	if rec, record, _ := chooseRunNotify(root, RunNotifyFlags{}); rec.Via != "webhook" || !record {
		t.Fatalf("env: %+v %v", rec, record)
	}
	if rec, _, _ := chooseRunNotify(root, RunNotifyFlags{Via: "ntfy", Topic: runfixture.RunNotifyTopic}); rec.Via != "ntfy" || rec.Topic != runfixture.RunNotifyTopic {
		t.Fatalf("flag over env: %+v", rec)
	}
	if err := jsonio.AtomicJSON(filepath.Join(root, notifyRecordName), runNotifyRecord{Via: "ntfy", Topic: runfixture.RunNotifyTopic}); err != nil {
		t.Fatal(err)
	}
	if rec, record, _ := chooseRunNotify(root, RunNotifyFlags{}); rec.Via != "ntfy" || record {
		t.Fatalf("record over env: %+v %v", rec, record)
	}
	if rec, _, _ := chooseRunNotify(root, RunNotifyFlags{Via: "off"}); rec.Via != "off" {
		t.Fatalf("flag over record: %+v", rec)
	}
	if _, _, err := chooseRunNotify(root, RunNotifyFlags{Via: "pager"}); err == nil {
		t.Fatal("unknown --notify accepted")
	}
	t.Setenv("IRRIGATOR_NOTIFY_VIA", "pager")
	os.Remove(filepath.Join(root, notifyRecordName))
	if _, _, err := chooseRunNotify(root, RunNotifyFlags{}); err == nil {
		t.Fatal("unknown IRRIGATOR_NOTIFY_VIA accepted")
	}
}

func TestRunNotifyNeverRecordsTokenOrEnvTopic(t *testing.T) {
	srv, _ := notifyServer(t, 200)
	runNotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_NTFY_TOKEN", "tk_supersecret")
	t.Setenv("IRRIGATOR_NTFY_TOPIC", runfixture.RunNotifyTopic)
	t.Setenv("IRRIGATOR_NOTIFY_VIA", "ntfy")
	root := runfixture.New(t, runfixture.Task("a", 0))
	n, err := NewRunNotifier(root, RunNotifyFlags{}, os.Stderr)
	if err != nil || n == nil {
		t.Fatalf("notifier: %v %v", n, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, notifyRecordName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "supersecret") || strings.Contains(string(raw), runfixture.RunNotifyTopic) || !strings.Contains(string(raw), `"ntfy"`) {
		t.Fatalf("record %s", raw)
	}
}

func TestRunNotifyCellTransitionsAndMessageContent(t *testing.T) {
	srv, sent := notifyServer(t, 200)
	runNotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_NTFY_TOPIC", runfixture.RunNotifyTopic)
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0), runfixture.Task("c", 0))
	s := runfixture.Read(t, root)
	s.Tasks["a"].Status, s.Tasks["a"].Error = "uncertain", "SECRET provider output"
	s.Tasks["b"].Status, s.Tasks["b"].Error = "failed", "SECRET prompt text"
	s.Control = "blocked"
	n, err := NewRunNotifier(root, RunNotifyFlags{Via: "ntfy"}, os.Stderr)
	if err != nil || n == nil {
		t.Fatal(err)
	}
	n.Observe(s)
	n.Observe(s) // same transitions: nothing more
	n.Flush()
	got := sent()
	if len(got) != 3 {
		t.Fatalf("sent %d: %+v", len(got), got)
	}
	all := ""
	for _, g := range got {
		all += g.body + g.header.Get("Title")
	}
	for _, want := range []string{"cell a is uncertain", "cell b is failed", "Run " + filepath.Base(root) + " is blocked", "0/3 verified"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %q", want, all)
		}
	}
	if strings.Contains(all, "SECRET") || strings.Contains(all, "produce answer") {
		t.Fatalf("notification leaked content: %q", all)
	}
	// A restart with the same cells repeats nothing.
	n2, _ := NewRunNotifier(root, RunNotifyFlags{}, os.Stderr)
	n2.Observe(s)
	n2.Flush()
	if len(sent()) != 3 {
		t.Fatalf("restart resent: %d", len(sent()))
	}
	// A new attempt reaching uncertain while the run is blocked again sends the
	// same pair as the first time: the cell, and the run being blocked.
	s.Tasks["a"].Attempt++
	n2.Observe(s)
	n2.Flush()
	if len(sent()) != 5 {
		t.Fatalf("new attempt sent %d in total, want 5", len(sent()))
	}
}

func TestRunNotifyBadEnvironmentDisablesInsteadOfBlockingRun(t *testing.T) {
	runNotifyEnv(t, "http://127.0.0.1:1")
	t.Setenv("IRRIGATOR_NOTIFY_VIA", "ntfy") // no topic anywhere
	root := runfixture.New(t, runfixture.Task("a", 0))
	if n, err := NewRunNotifier(root, RunNotifyFlags{}, os.Stderr); n != nil || err != nil {
		t.Fatalf("env misconfiguration: %v %v", n, err)
	}
	if _, err := NewRunNotifier(root, RunNotifyFlags{Via: "ntfy"}, os.Stderr); err == nil {
		t.Fatal("explicit --notify ntfy without a topic must fail")
	}
	if _, err := os.Stat(filepath.Join(root, notifyRecordName)); err == nil {
		t.Fatal("a failed explicit choice was recorded")
	}
}

func TestRunNotifySlowServerDoesNotBlockController(t *testing.T) {
	old1, old2 := runNotifySendTimeout, runNotifyFlushTimeout
	runNotifySendTimeout, runNotifyFlushTimeout = 5*time.Second, 100*time.Millisecond
	t.Cleanup(func() { runNotifySendTimeout, runNotifyFlushTimeout = old1, old2 })
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	runNotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_NTFY_TOPIC", runfixture.RunNotifyTopic)
	root := runfixture.New(t, runfixture.Task("a", 0))
	n, err := NewRunNotifier(root, RunNotifyFlags{Via: "ntfy"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	s.Control = "complete"
	start := time.Now()
	n.Observe(s)
	n.Flush()
	if time.Since(start) > 2*time.Second {
		t.Fatalf("controller waited %s", time.Since(start))
	}
}

func TestNotifyJSONPrintsLinksAndNonces(t *testing.T) {
	srv, sent := notifyServer(t, 200)
	runfixture.NotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_NTFY_TOPIC", runfixture.RunNotifyTopic)
	root := notifyRun(t)
	code, out, _, err := runNotify("--run", root, "--request", "a", "--via", "ntfy", "--json")
	if err != nil || code != 0 {
		t.Fatalf("notify: %d %v", code, err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	for _, k := range []string{"approve_url", "deny_url", "approve_nonce", "deny_nonce", "expires_at"} {
		if got[k] == "" {
			t.Fatalf("missing %s in %v", k, got)
		}
	}
	if got["approve_nonce"] == got["deny_nonce"] || !strings.HasPrefix(got["approve_url"], "https://approve.example.test/approve/") {
		t.Fatalf("bad payload %v", got)
	}
	if len(sent()) != 1 || !strings.Contains(sent()[0].header.Get("Actions"), got["approve_url"]) {
		t.Fatalf("the sent notice does not carry the printed links")
	}
}

func runNotifyEnv(t *testing.T, server string) {
	t.Helper()
	runfixture.NotifyEnv(t, server)
	t.Setenv("IRRIGATOR_NOTIFY_VIA", "")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	old := ActiveNotify
	t.Cleanup(func() { ActiveNotify = old })
	ActiveNotify = RunNotifyFlags{}
}
