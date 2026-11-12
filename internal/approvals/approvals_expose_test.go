package approvals

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/runfixture"
)

// fakeTool writes an executable shell script named name into a fresh
// directory placed first on PATH and returns the directory. $FAKE_DIR in the
// body is replaced by the directory, so a script can record argv and its pid.
func fakeTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	script := "#!/bin/sh\n" + strings.ReplaceAll(body, "$FAKE_DIR", dir) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func fakeFile(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

func approvalWaitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// approvalWaitForFast is approvalWaitFor with a 1ms poll, for waits on a
// local child that normally completes in a few milliseconds.
func approvalWaitForFast(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func processGone(t *testing.T, pidFile string) bool {
	t.Helper()
	pid, err := strconv.Atoi(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	return syscall.Kill(pid, 0) != nil
}

const fakeQuickCloudflared = `echo "$@" > $FAKE_DIR/argv
echo $$ > $FAKE_DIR/pid
echo "INF Requesting new quick Tunnel on https://api.trycloudflare.com ..." >&2
echo "INF |  https://fancy-words-here.trycloudflare.com  |" >&2
exec sleep 60`

func TestApprovalsExposeCloudflareQuickURLAndShutdown(t *testing.T) {
	dir := fakeTool(t, "", "cloudflared", fakeQuickCloudflared)
	e, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeCloudflare}, "8787")
	if err != nil {
		t.Fatal(err)
	}
	if e.url != "https://fancy-words-here.trycloudflare.com" {
		t.Fatalf("url %q (api host must be ignored)", e.url)
	}
	if got := fakeFile(t, dir, "argv"); got != "tunnel --no-autoupdate --url http://127.0.0.1:8787" {
		t.Fatalf("argv %q", got)
	}
	pid := fakeFile(t, dir, "pid")
	if processGone(t, pid) {
		t.Fatal("tunnel not running")
	}
	e.shutdown(&bytes.Buffer{})
	approvalWaitFor(t, "tunnel to stop", func() bool { return processGone(t, pid) })
}

// exposeAsync runs startApprovalExposure on a goroutine so a test can drive the
// virtual clock while it is blocked on its start deadline.
func exposeAsync(opts exposeOptions) <-chan exposeResult {
	ch := make(chan exposeResult, 1)
	go func() {
		e, err := startApprovalExposure(context.Background(), opts, "8787")
		ch <- exposeResult{e, err}
	}()
	return ch
}

type exposeResult struct {
	e   *approvalExposure
	err error
}

func TestApprovalsExposeCloudflareQuickTimeout(t *testing.T) {
	vc := runfixture.NewFakeClock(runfixture.ClockEpoch)
	defer runfixture.SetClock(vc)()
	dir := fakeTool(t, "", "cloudflared", "echo $$ > $FAKE_DIR/pid\necho 'INF starting' >&2\ntouch $FAKE_DIR/printed\nexec sleep 60")
	res := exposeAsync(exposeOptions{mode: exposeCloudflare})
	// The timer is registered after the child starts; wait until the child has
	// printed too, so the tail in the error is deterministic.
	vc.WaitForTimers(t, 1)
	approvalWaitForFast(t, "child output", func() bool { _, err := os.Stat(filepath.Join(dir, "printed")); return err == nil })
	vc.Advance(cloudflareStartTimeout - time.Nanosecond)
	// A nanosecond before the deadline the start timer must still be pending;
	// a non-blocking receive alone could pass merely because the goroutine has
	// not been scheduled yet.
	if got := vc.Pending(); got != 1 {
		t.Fatalf("pending timers %d one nanosecond before the deadline, want 1", got)
	}
	select {
	case r := <-res:
		t.Fatalf("returned before the deadline: %v", r.err)
	default:
	}
	vc.Advance(time.Nanosecond)
	r := runfixture.RecvWithin(t, res, "start result")
	if r.err == nil || !strings.Contains(r.err.Error(), "did not print a trycloudflare.com URL") || !strings.Contains(r.err.Error(), "INF starting") {
		t.Fatalf("error %v", r.err)
	}
	approvalWaitForFast(t, "timed-out tunnel to stop", func() bool { return processGone(t, fakeFile(t, dir, "pid")) })
}

func TestApprovalsExposeCloudflareQuickEarlyExit(t *testing.T) {
	fakeTool(t, "", "cloudflared", "echo 'ERR no network' >&2\nexit 3")
	_, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeCloudflare}, "8787")
	if err == nil || !strings.Contains(err.Error(), "exited before printing") || !strings.Contains(err.Error(), "no network") {
		t.Fatalf("error %v", err)
	}
}

func TestApprovalsExposeCloudflareMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeCloudflare}, "8787"); err == nil || !strings.Contains(err.Error(), "cloudflared") {
		t.Fatalf("error %v", err)
	}
}

func TestApprovalsExposeCloudflareNamed(t *testing.T) {
	vc := runfixture.NewFakeClock(runfixture.ClockEpoch)
	defer runfixture.SetClock(vc)()
	dir := fakeTool(t, "", "cloudflared", "echo \"$@\" > $FAKE_DIR/argv\necho $$ > $FAKE_DIR/pid.tmp\nmv $FAKE_DIR/pid.tmp $FAKE_DIR/pid\nexec sleep 60")
	res := exposeAsync(exposeOptions{mode: exposeCloudflare, cfTunnel: "approvals", cfHostname: "ok.example.com"})
	vc.WaitForTimers(t, 1)
	vc.Advance(namedTunnelSettle)
	r := runfixture.RecvWithin(t, res, "start result")
	if r.err != nil {
		t.Fatal(r.err)
	}
	e := r.e
	approvalWaitForFast(t, "argv", func() bool { _, err := os.Stat(filepath.Join(dir, "pid")); return err == nil })
	if e.url != "https://ok.example.com" || fakeFile(t, dir, "argv") != "tunnel run approvals" {
		t.Fatalf("url %q argv %q", e.url, fakeFile(t, dir, "argv"))
	}
	e.shutdown(&bytes.Buffer{})
	approvalWaitForFast(t, "named tunnel to stop", func() bool { return processGone(t, fakeFile(t, dir, "pid")) })
}

func TestApprovalsExposeCloudflareNamedFailures(t *testing.T) {
	// The settle window races the child's exit; the virtual clock never fires it, so an immediate failure is always reported however slowly the shell starts.
	vc := runfixture.NewFakeClock(runfixture.ClockEpoch)
	defer runfixture.SetClock(vc)()
	fakeTool(t, "", "cloudflared", "echo 'tunnel not found' >&2\nexit 1")
	if _, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeCloudflare, cfTunnel: "nope", cfHostname: "ok.example.com"}, "8787"); err == nil || !strings.Contains(err.Error(), "tunnel not found") {
		t.Fatalf("error %v", err)
	}
	for _, bad := range []exposeOptions{{cfTunnel: "x"}, {cfHostname: "h.example.com"}, {cfTunnel: "-x", cfHostname: "h.example.com"}, {cfTunnel: "x", cfHostname: "h.example.com/path"}} {
		bad.mode = exposeCloudflare
		if _, err := startApprovalExposure(context.Background(), bad, "8787"); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// fakeTailscale records every invocation. status prints the node name;
// "serve status" prints $TS_SERVE_STATUS; "off" fails when $TS_OFF_FAIL is set.
const fakeTailscale = `echo "$@" >> $FAKE_DIR/calls
case "$1 $2" in
"status --json") echo '{"Self":{"DNSName":"box.tail-1234.ts.net."}}' ;;
"serve status") echo "${TS_SERVE_STATUS:-{\}}" ;;
*)
  case "$*" in
  *off) if [ -n "$TS_OFF_FAIL" ]; then echo 'denied' >&2; exit 1; fi ;;
  esac ;;
esac`

func TestApprovalsExposeTailscaleServeAndRemoveOnlyOurHandler(t *testing.T) {
	dir := fakeTool(t, "", "tailscale", fakeTailscale)
	e, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeTailscale}, "8787")
	if err != nil {
		t.Fatal(err)
	}
	if e.url != "https://box.tail-1234.ts.net" {
		t.Fatalf("url %q", e.url)
	}
	var stderr bytes.Buffer
	e.shutdown(&stderr)
	calls := fakeFile(t, dir, "calls")
	want := "serve --bg --https=443 http://127.0.0.1:8787\nserve --https=443 off"
	if !strings.HasSuffix(calls, want) || strings.Contains(calls, "reset") || stderr.Len() != 0 {
		t.Fatalf("calls %q stderr %q", calls, stderr.String())
	}
}

func TestApprovalsExposeTailscaleRemovalFailurePrintsCommand(t *testing.T) {
	fakeTool(t, "", "tailscale", fakeTailscale)
	t.Setenv("TS_OFF_FAIL", "1")
	e, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeTailscale}, "8787")
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	e.shutdown(&stderr)
	if !strings.Contains(stderr.String(), "remove it with: tailscale serve --https=443 off") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestApprovalsExposeTailscaleRefusesToReplaceHandler(t *testing.T) {
	dir := fakeTool(t, "", "tailscale", fakeTailscale)
	t.Setenv("TS_SERVE_STATUS", `{"Web":{"box.tail-1234.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`)
	if _, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeTailscale}, "8787"); err == nil || !strings.Contains(err.Error(), "already has") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(fakeFile(t, dir, "calls"), "--bg") {
		t.Fatal("serve ran over an existing handler")
	}
}

func TestApprovalsExposeTailscaleFailures(t *testing.T) {
	fakeTool(t, "", "tailscale", `echo 'Tailscale is stopped.' >&2; exit 1`)
	if _, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeTailscale}, "8787"); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error %v", err)
	}
	fakeTool(t, "", "tailscale", `echo '{"Self":{"DNSName":""}}'`)
	if _, err := startApprovalExposure(context.Background(), exposeOptions{mode: exposeTailscale}, "8787"); err == nil || !strings.Contains(err.Error(), "DNSName") {
		t.Fatalf("error %v", err)
	}
}

func TestApprovalsPublicURLValidation(t *testing.T) {
	for in, want := range map[string]string{
		"https://a.example.com":        "https://a.example.com",
		"https://a.example.com/":       "https://a.example.com",
		"http://127.0.0.1:9000":        "http://127.0.0.1:9000",
		" https://a.example.com:8443 ": "https://a.example.com:8443",
	} {
		if got, err := normalizePublicURL(in); err != nil || got != want {
			t.Fatalf("%q -> %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "a.example.com", "http://a.example.com", "ftp://a.example.com", "https://a.example.com/x", "https://a.example.com?q=1", "https://u:p@a.example.com", "https://a.example.com#f"} {
		if got, err := normalizePublicURL(in); err == nil {
			t.Fatalf("%q accepted as %q", in, got)
		}
	}
}

func TestApprovalsServeExposeFlagValidation(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	for _, args := range [][]string{
		{"--expose", "bogus"},
		{"--expose", "cloudflare", "--public-url", "https://a.example.com"},
		{"--expose", "cloudflare", "--cf-tunnel", "t"},
		{"--expose", "tailscale", "--cf-hostname", "h.example.com"},
		{"--cf-tunnel", "t", "--cf-hostname", "h.example.com"},
		{"--public-url", "http://a.example.com"},
	} {
		if code, err := approvalsServe(append([]string{"--run", root, "--addr", "127.0.0.1:0"}, args...)); code != 2 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
}

func TestApprovalsServePublishesAndRemovesPublicURL(t *testing.T) {
	dir := fakeTool(t, "", "cloudflared", fakeQuickCloudflared)
	root := runfixture.ApprovalLegacyRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		code, _ := serveApprovals(ctx, approvalsServeOptions{run: root, addr: "127.0.0.1:0", expose: exposeOptions{mode: exposeCloudflare}}, &stdout, &stderr)
		done <- code
	}()
	file := publicURLPath(root)
	approvalWaitFor(t, "public url file", func() bool { _, err := os.Stat(file); return err == nil })
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if got, err := readPublicURL(root); err != nil || got != "https://fancy-words-here.trycloudflare.com" {
		t.Fatalf("file %q %v", got, err)
	}
	// The tunnel is pointed at the port serve actually bound.
	if argv := fakeFile(t, dir, "argv"); !strings.HasPrefix(argv, "tunnel --no-autoupdate --url http://127.0.0.1:") || strings.HasSuffix(argv, ":0") {
		t.Fatalf("argv %q", argv)
	}
	pid := fakeFile(t, dir, "pid")
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("public url file left behind: %v", err)
	}
	approvalWaitFor(t, "tunnel to stop", func() bool { return processGone(t, pid) })
}

func TestApprovalsServeFailsWhenTunnelDies(t *testing.T) {
	fakeTool(t, "", "cloudflared", "echo 'https://dies-soon.trycloudflare.com' >&2\nsleep 0.3\nexit 1")
	root := runfixture.ApprovalLegacyRun(t)
	code, err := serveApprovals(context.Background(), approvalsServeOptions{run: root, addr: "127.0.0.1:0", expose: exposeOptions{mode: exposeCloudflare}}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "cloudflared stopped") {
		t.Fatalf("%d %v", code, err)
	}
	if _, err = os.Stat(publicURLPath(root)); !os.IsNotExist(err) {
		t.Fatal("public url file left behind")
	}
}

func TestApprovalsServeExplicitPublicURL(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = serveApprovals(ctx, approvalsServeOptions{run: root, addr: "127.0.0.1:0", expose: exposeOptions{mode: exposeNone, publicURL: "https://mine.example.com"}}, &bytes.Buffer{}, &bytes.Buffer{})
		close(done)
	}()
	approvalWaitFor(t, "public url file", func() bool { got, _ := readPublicURL(root); return got == "https://mine.example.com" })
	cancel()
	<-done
	if got, _ := readPublicURL(root); got != "" {
		t.Fatalf("file left: %q", got)
	}
}

func TestApprovalsLinkPrintsFullURLWhenPublicURLKnown(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	bare := approvalLink(t, root, "a", "approve", "5m")
	if !strings.HasPrefix(bare, "/approve/") {
		t.Fatalf("without a public URL the path is printed: %q", bare)
	}
	if err := writePublicURL(root, "https://x.trycloudflare.com"); err != nil {
		t.Fatal(err)
	}
	full := approvalLink(t, root, "a", "approve", "5m")
	if !strings.HasPrefix(full, "https://x.trycloudflare.com/approve/") || strings.Contains(full, "\x1b") {
		t.Fatalf("full link %q", full)
	}
	out := runfixture.ApprovalCapture(t, func() {
		if code, err := ApprovalsCLI([]string{"link", "--run", root, "--request", "a", "--action", "deny", "--path-only"}); code != 0 || err != nil {
			t.Fatalf("%d %v", code, err)
		}
	})
	if !strings.HasPrefix(out, "/deny/") || strings.Count(out, "\n") != 1 {
		t.Fatalf("path-only %q", out)
	}
	// The printed link is accepted by the real handler, and piped output with
	// only IRRIGATOR_APPROVALS_URL set stays the bare path.
	h := approvalHandler(t, root)
	if w := approvalDo(h, "POST", strings.TrimPrefix(full, "https://x.trycloudflare.com")); w.Code != 200 {
		t.Fatalf("post %d %s", w.Code, w.Body)
	}
	removePublicURL(root)
	t.Setenv("IRRIGATOR_APPROVALS_URL", "https://env.example.com")
	if got := approvalLink(t, root, "a", "approve", "5m"); !strings.HasPrefix(got, "/approve/") {
		t.Fatalf("piped with env only: %q", got)
	}
}

func TestApprovalsLinkRejectsCorruptPublicURLFile(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	if err := os.MkdirAll(filepath.Join(root, "approvals"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicURLPath(root), []byte("javascript:alert(1)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err := ApprovalsCLI([]string{"link", "--run", root, "--request", "a", "--action", "approve"})
	if code != 1 || err == nil {
		t.Fatalf("%d %v", code, err)
	}
}
