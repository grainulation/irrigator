package approvals

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func notifyRun(t *testing.T) string {
	t.Helper()
	root := runfixture.ApprovalLegacyRun(t)
	if err := writePublicURL(root, "https://approve.example.test"); err != nil {
		t.Fatal(err)
	}
	return root
}

func runNotify(args ...string) (int, string, string, error) {
	var out, errb bytes.Buffer
	code, err := approvalsNotifyTo(args, &out, &errb)
	return code, out.String(), errb.String(), err
}

func TestNotifyNtfyHeadersButtonsAndNoTokenWithoutEnv(t *testing.T) {
	srv, sent := notifyServer(t, 200)
	runfixture.NotifyEnv(t, srv.URL)
	root := notifyRun(t)
	topic := "a-long-random-topic-0123456789"
	code, out, stderr, err := runNotify("--run", root, "--request", "a", "--via", "ntfy", "--topic", topic, "--title", "Deploy?", "--message", "Please decide")
	if code != 0 || err != nil || stderr != "" {
		t.Fatalf("%d %v %q", code, err, stderr)
	}
	if strings.Contains(out, "approve.example.test") || strings.Contains(out, "/approve/") {
		t.Fatalf("links printed to stdout: %q", out)
	}
	reqs := sent()
	if len(reqs) != 1 || reqs[0].method != "POST" || reqs[0].path != "/"+topic || reqs[0].body != "Please decide" {
		t.Fatalf("%+v", reqs)
	}
	h := reqs[0].header
	if h.Get("Title") != "Deploy?" || h.Get("Authorization") != "" {
		t.Fatalf("headers %v", h)
	}
	actions := strings.Split(h.Get("Actions"), "; ")
	if len(actions) != 3 ||
		!strings.HasPrefix(actions[0], "http, Approve, https://approve.example.test/approve/") || !strings.HasSuffix(actions[0], ", method=POST") ||
		!strings.HasPrefix(actions[1], "http, Deny, https://approve.example.test/deny/") || !strings.HasSuffix(actions[1], ", method=POST") ||
		!strings.HasPrefix(actions[2], "view, Open, https://approve.example.test/approve/") {
		t.Fatalf("actions %q", h.Get("Actions"))
	}
	// The minted links are live: the approve button's path decides once.
	approve := strings.TrimSuffix(strings.TrimPrefix(actions[0], "http, Approve, https://approve.example.test"), ", method=POST")
	if w := approvalDo(approvalHandler(t, root), "POST", approve); w.Code != 200 {
		t.Fatalf("button link: %d %s", w.Code, w.Body)
	}
}

func TestNotifyNtfyTokenOnlyFromEnvAndTopicFromEnv(t *testing.T) {
	srv, sent := notifyServer(t, 200)
	runfixture.NotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_NTFY_TOKEN", "tk_secret")
	t.Setenv("IRRIGATOR_NTFY_TOPIC", "env-topic-that-is-long-enough")
	root := notifyRun(t)
	if code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "ntfy"); code != 0 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
	r := sent()[0]
	if r.header.Get("Authorization") != "Bearer tk_secret" || r.path != "/env-topic-that-is-long-enough" || r.header.Get("Title") != "Approval needed: a" {
		t.Fatalf("%+v", r)
	}
	// There is no flag that could carry the token through argv.
	if code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "ntfy", "--token", "x"); code != 2 && err == nil {
		t.Fatalf("--token accepted: %d", code)
	}
}

func TestNotifyNtfyWarnsOnShortTopic(t *testing.T) {
	srv, _ := notifyServer(t, 200)
	runfixture.NotifyEnv(t, srv.URL)
	root := notifyRun(t)
	code, _, stderr, err := runNotify("--run", root, "--request", "a", "--via", "ntfy", "--topic", "short")
	if code != 0 || err != nil || !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "shorter than 20") {
		t.Fatalf("%d %v %q", code, err, stderr)
	}
}

func TestNotifyNtfyTitleNonASCIIIsEncoded(t *testing.T) {
	srv, sent := notifyServer(t, 200)
	runfixture.NotifyEnv(t, srv.URL)
	root := notifyRun(t)
	if code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "ntfy", "--topic", "a-long-random-topic-0123456789", "--title", "Déploiement ?"); code != 0 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
	if got := sent()[0].header.Get("Title"); !strings.HasPrefix(got, "=?utf-8?q?") {
		t.Fatalf("title %q", got)
	}
}

func TestNotifyFailureIsNonZeroAndKeepsTokensValid(t *testing.T) {
	srv, sent := notifyServer(t, 500)
	runfixture.NotifyEnv(t, srv.URL)
	root := notifyRun(t)
	code, out, _, err := runNotify("--run", root, "--request", "a", "--via", "webhook", "--url", srv.URL+"/secret-hook")
	if code != 1 || err == nil || out != "" {
		t.Fatalf("%d %v %q", code, err, out)
	}
	if !strings.Contains(err.Error(), "notification via webhook failed") || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "secret-hook") {
		t.Fatalf("error %v", err)
	}
	var n approvalNotice
	if jerr := json.Unmarshal([]byte(sent()[0].body), &n); jerr != nil {
		t.Fatal(jerr)
	}
	h := approvalHandler(t, root)
	for _, link := range []string{n.ApproveURL, n.DenyURL} {
		path := strings.TrimPrefix(link, "https://approve.example.test")
		if w := approvalDo(h, "GET", path); w.Code != 200 {
			t.Fatalf("link dead after failed send: %d", w.Code)
		}
	}
	if w := approvalDo(h, "POST", strings.TrimPrefix(n.ApproveURL, "https://approve.example.test")); w.Code != 200 {
		t.Fatalf("post %d", w.Code)
	}
}

func TestNotifyUnreachableServerDoesNotLeakURL(t *testing.T) {
	srv, _ := notifyServer(t, 200)
	url := srv.URL
	srv.Close()
	runfixture.NotifyEnv(t, url)
	root := notifyRun(t)
	code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "ntfy", "--topic", "a-long-random-topic-0123456789")
	if code != 1 || err == nil || strings.Contains(err.Error(), "a-long-random-topic") {
		t.Fatalf("%d %v", code, err)
	}
}

func TestNotifyWebhookPayload(t *testing.T) {
	srv, sent := notifyServer(t, 204)
	runfixture.NotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_APPROVALS_WEBHOOK", srv.URL+"/hook")
	root := notifyRun(t)
	// URL comes from the environment; --public-url wins over the file.
	code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "webhook", "--ttl", "10m", "--title", "T", "--message", "M", "--public-url", "https://other.example.test")
	if code != 0 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
	r := sent()[0]
	if r.method != "POST" || r.path != "/hook" || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%+v", r)
	}
	var raw map[string]any
	if err = json.Unmarshal([]byte(r.body), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run", "request", "approve_url", "deny_url", "expires_at", "title", "message"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %s in %s", k, r.body)
		}
	}
	if len(raw) != 7 || raw["request"] != "a" || raw["title"] != "T" || raw["message"] != "M" ||
		!strings.HasPrefix(raw["approve_url"].(string), "https://other.example.test/approve/") ||
		!strings.HasPrefix(raw["deny_url"].(string), "https://other.example.test/deny/") {
		t.Fatalf("payload %s", r.body)
	}
}

func TestNotifyWebhookFromRunRecord(t *testing.T) {
	srv, sent := notifyServer(t, 204)
	runfixture.NotifyEnv(t, srv.URL)
	t.Setenv("IRRIGATOR_APPROVALS_WEBHOOK", "")
	root := notifyRun(t)
	if err := os.WriteFile(filepath.Join(root, "notify.json"), []byte(`{"via":"webhook","url":"`+srv.URL+`/from-run"}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "webhook", "--ttl", "10m", "--public-url", "https://other.example.test")
	if code != 0 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
	r := sent()[0]
	if r.method != "POST" || r.path != "/from-run" {
		t.Fatalf("expected POST to /from-run, got %+v", r)
	}
}

func TestNotifyRequiresPublicURLAndValidInput(t *testing.T) {
	srv, sent := notifyServer(t, 200)
	runfixture.NotifyEnv(t, srv.URL)
	root := runfixture.ApprovalLegacyRun(t) // no public URL file
	for _, args := range [][]string{
		{"--run", root, "--request", "a", "--via", "webhook", "--url", srv.URL},               // no public URL
		{"--run", root, "--request", "a", "--via", "carrier-pigeon"},                          // unknown notifier
		{"--run", root, "--request", "a", "--via", "webhook"},                                 // no webhook URL
		{"--run", root, "--request", "a", "--via", "ntfy"},                                    // no topic
		{"--run", root, "--request", "a", "--via", "ntfy", "--topic", "bad topic!"},           // bad topic
		{"--run", root, "--request", "a", "--via", "webhook", "--url", "http://example.test"}, // plain http, non-loopback
		{"--run", root, "--request", "a", "--via", "webhook", "--url", srv.URL, "--ttl", "25h", "--public-url", "https://p.example.test"},
		{"--run", root, "--request", "a", "--via", "webhook", "--url", srv.URL, "--title", "a\nb", "--public-url", "https://p.example.test"},
	} {
		if code, _, _, err := runNotify(args...); code != 2 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	if code, _, _, err := runNotify("--run", root, "--request", "nope!", "--via", "webhook", "--url", srv.URL, "--public-url", "https://p.example.test"); code != 2 || err == nil {
		t.Fatalf("bad request id: %d %v", code, err)
	}
	if len(sent()) != 0 {
		t.Fatal("something was sent for invalid input")
	}
}

func TestNotifyNtfyRefusesPlainHTTPToRemoteServer(t *testing.T) {
	runfixture.NotifyEnv(t, "http://ntfy.example.test")
	t.Setenv("IRRIGATOR_NTFY_TOKEN", "tk_secret")
	root := notifyRun(t)
	if code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "ntfy", "--topic", "a-long-random-topic-0123456789"); code != 2 || err == nil || strings.Contains(err.Error(), "tk_secret") {
		t.Fatalf("%d %v", code, err)
	}
}

func TestNotifyDoesNotFollowRedirects(t *testing.T) {
	target, sent := notifyServer(t, 200)
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	t.Cleanup(redirect.Close)
	runfixture.NotifyEnv(t, redirect.URL)
	root := notifyRun(t)
	if code, _, _, err := runNotify("--run", root, "--request", "a", "--via", "webhook", "--url", redirect.URL); code != 1 || err == nil {
		t.Fatalf("%d %v", code, err)
	}
	if len(sent()) != 0 {
		t.Fatal("redirect was followed")
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

func TestNotifyRejectsCredentialsEmbeddedInURL(t *testing.T) {
	for _, raw := range []string{"https://user:pass@ntfy.example.test", "https://token@ntfy.example.test:8443/x"} {
		_, err := validNotifyURL(raw, "IRRIGATOR_NTFY_SERVER", true)
		if err == nil {
			t.Fatalf("%q accepted", raw)
		}
		if strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "token@") {
			t.Fatalf("error echoes the URL: %v", err)
		}
	}
}

// The reason phrase of a failed response is server-controlled; only the numeric
// code may reach the error (and from there terminal output and notification logs).
func TestNotifyFailureReportsStatusCodeNotReasonPhrase(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(c, 1))
		_, _ = c.Write([]byte("HTTP/1.1 400 echoed-synthetic-secret\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
	}()
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String(), strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	err = notifySend(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "echoed-synthetic-secret") {
		t.Fatalf("got %v", err)
	}
}

// Transport and parser errors can quote server bytes (a malformed status line) or
// the URL, so they are reported as fixed text.
func TestNotifyTransportErrorsAreClassifiedNotQuoted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(c, 1))
		_, _ = c.Write([]byte("HTTP/1.1 reflected-synthetic-secret Nope\r\nContent-Length: 0\r\n\r\n"))
	}()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/topic-secret", strings.NewReader("x"))
	err = notifySend(context.Background(), req)
	ln.Close()
	if err == nil || strings.Contains(err.Error(), "reflected-synthetic-secret") || strings.Contains(err.Error(), "topic-secret") {
		t.Fatalf("malformed response: %v", err)
	}
	req, _ = http.NewRequest(http.MethodPost, "http://"+addr+"/topic-secret", strings.NewReader("x"))
	err = notifySend(context.Background(), req)
	if err == nil || strings.Contains(err.Error(), addr) || strings.Contains(err.Error(), "topic-secret") {
		t.Fatalf("refused connection: %v", err)
	}
}

func TestNotifyTransportErrorCategories(t *testing.T) {
	cases := map[string]error{
		"notification request was cancelled":              context.Canceled,
		"notification request timed out":                  context.DeadlineExceeded,
		"notification server certificate was not trusted": x509.UnknownAuthorityError{},
		"could not connect to the notification server":    &net.OpError{Op: "dial", Err: errors.New("refused")},
		"notification server sent an invalid response":    errors.New("malformed HTTP status code \"x\""),
	}
	for want, in := range cases {
		if got := notifyTransportError(in); got == nil || got.Error() != want {
			t.Fatalf("%v -> %v, want %q", in, got, want)
		}
	}
}
