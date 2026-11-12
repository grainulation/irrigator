package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
)

const (
	approvalPublicURLFile = "public_url"
	exposeNone            = "none"
	exposeCloudflare      = "cloudflare"
	exposeTailscale       = "tailscale"
)

// Tailscale CLI syntax lives here so it is easy to fix when the CLI changes.
// The off command removes only the https:443 root handler, which is the one
// the serve command adds.
var (
	tailscaleServeArgs = func(port string) []string {
		return []string{"serve", "--bg", "--https=443", "http://127.0.0.1:" + port}
	}
	tailscaleServeOffArgs = []string{"serve", "--https=443", "off"}
	tailscaleStatusArgs   = []string{"status", "--json"}
	tailscaleServeStatus  = []string{"serve", "status", "--json"}
)

// Timeouts are variables so tests can shorten them.
var (
	cloudflareStartTimeout = 30 * time.Second
	exposeCommandTimeout   = 20 * time.Second
	exposeStopGrace        = 3 * time.Second
	namedTunnelSettle      = 750 * time.Millisecond
)

var quickTunnelURL = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com`)
var dnsHostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

type exposeOptions struct {
	mode       string
	publicURL  string
	cfTunnel   string
	cfHostname string
}

// approvalExposure is a running exposure. exited is nil when nothing runs in
// the background (tailscale serve --bg).
type approvalExposure struct {
	url    string
	exited <-chan struct{}
	err    func() error
	stop   func(stderr io.Writer)
}

func (e *approvalExposure) shutdown(stderr io.Writer) {
	if e != nil && e.stop != nil {
		e.stop(stderr)
	}
}

// normalizePublicURL validates a public base URL and returns it without a
// trailing slash. Only an origin is accepted because serve handles only
// /approve/<token> and /deny/<token> at the root.
func normalizePublicURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("public URL must be an origin like https://host (no path, query, credentials or fragment)")
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Plain http only makes sense for a loopback listener (local testing).
		if ip := net.ParseIP(u.Hostname()); !(u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
			return "", errors.New("public URL must use https")
		}
	default:
		return "", errors.New("public URL must use https")
	}
	return u.Scheme + "://" + u.Host, nil
}

func publicURLPath(root string) string {
	return filepath.Join(ApprovalDir(root), approvalPublicURLFile)
}

// readPublicURL returns the recorded public base URL, or "" when none exists.
func readPublicURL(root string) (string, error) {
	raw, err := os.ReadFile(publicURLPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	base, err := normalizePublicURL(string(raw))
	if err != nil {
		return "", fmt.Errorf("%s: %w", publicURLPath(root), err)
	}
	return base, nil
}

func writePublicURL(root, base string) error {
	dir := ApprovalDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, approvalPublicURLFile+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.WriteString(base + "\n")
	cherr := f.Chmod(0o600)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, cherr, serr, cerr); err == nil {
		err = os.Rename(tmp, publicURLPath(root))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func removePublicURL(root string) {
	_ = os.Remove(publicURLPath(root))
}

// lockedBuffer is a bounded, concurrency-safe tail of child output that also
// watches for the quick tunnel URL.
type lockedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	found chan string
	sent  bool
}

func newLockedBuffer() *lockedBuffer { return &lockedBuffer{found: make(chan string, 1)} }

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 16<<10 {
		b.buf = b.buf[len(b.buf)-8<<10:]
	}
	if !b.sent {
		for _, m := range quickTunnelURL.FindAll(b.buf, -1) {
			// api.trycloudflare.com is cloudflared's control endpoint, not the tunnel.
			if !bytes.HasPrefix(m, []byte("https://api.")) {
				b.sent = true
				b.found <- string(m)
				break
			}
		}
	}
	return len(p), nil
}

func (b *lockedBuffer) tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := strings.TrimSpace(string(b.buf))
	if len(t) > 400 {
		t = "..." + t[len(t)-400:]
	}
	return t
}

// childProc is a tunnel process started by this command, in its own process
// group so its helpers are stopped with it. Only processes started here are
// ever signalled.
type childProc struct {
	cmd    *exec.Cmd
	exited chan struct{}
	err    error
}

func startChild(name string, args []string, out io.Writer) (*childProc, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &childProc{cmd: cmd, exited: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

func (p *childProc) Stop() {
	select {
	case <-p.exited:
		return
	default:
	}
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(exposeStopGrace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.exited
	}
}

func (p *childProc) exitError() error {
	if p.err != nil {
		return p.err
	}
	return errors.New("exited")
}

func startApprovalExposure(ctx context.Context, o exposeOptions, port string) (*approvalExposure, error) {
	switch o.mode {
	case exposeCloudflare:
		if o.cfTunnel != "" || o.cfHostname != "" {
			return startCloudflareNamed(o)
		}
		return startCloudflareQuick(ctx, port)
	case exposeTailscale:
		return startTailscale(ctx, port)
	}
	return nil, fmt.Errorf("unknown --expose %q (none, cloudflare or tailscale)", o.mode)
}

func childExposure(p *childProc, base string, name string) *approvalExposure {
	return &approvalExposure{url: base, exited: p.exited,
		err:  func() error { return fmt.Errorf("%s stopped: %w", name, p.exitError()) },
		stop: func(io.Writer) { p.Stop() }}
}

func startCloudflareQuick(ctx context.Context, port string) (*approvalExposure, error) {
	out := newLockedBuffer()
	p, err := startChild("cloudflared", []string{"tunnel", "--no-autoupdate", "--url", "http://127.0.0.1:" + port}, out)
	if err != nil {
		return nil, err
	}
	// The module's Go version (1.23+) collects an unreferenced timer even if it
	// is never stopped, so the deadline channel needs no explicit cancellation.
	startDeadline := clock.ActiveClock.After(cloudflareStartTimeout)
	select {
	case u := <-out.found:
		return childExposure(p, u, "cloudflared"), nil
	case <-p.exited:
		// The URL may have been printed just before a quick exit.
		select {
		case u := <-out.found:
			return childExposure(p, u, "cloudflared"), nil
		default:
		}
		return nil, fmt.Errorf("cloudflared exited before printing a trycloudflare.com URL (%v): %s", p.exitError(), out.tail())
	case <-startDeadline:
		p.Stop()
		return nil, fmt.Errorf("cloudflared did not print a trycloudflare.com URL within %s: %s", cloudflareStartTimeout, out.tail())
	case <-ctx.Done():
		p.Stop()
		return nil, ctx.Err()
	}
}

func startCloudflareNamed(o exposeOptions) (*approvalExposure, error) {
	if o.cfTunnel == "" || o.cfHostname == "" {
		return nil, errors.New("--cf-tunnel and --cf-hostname must be given together")
	}
	if !dnsHostName.MatchString(o.cfHostname) || strings.ContainsAny(o.cfTunnel, " \t\r\n") || strings.HasPrefix(o.cfTunnel, "-") {
		return nil, errors.New("invalid --cf-tunnel or --cf-hostname")
	}
	base, err := normalizePublicURL("https://" + o.cfHostname)
	if err != nil {
		return nil, err
	}
	out := newLockedBuffer()
	p, err := startChild("cloudflared", []string{"tunnel", "run", o.cfTunnel}, out)
	if err != nil {
		return nil, err
	}
	// A named tunnel prints no URL; give an immediate failure (bad name, no
	// credentials) a moment to show before claiming the hostname is live.
	select {
	case <-p.exited:
		return nil, fmt.Errorf("cloudflared tunnel run %s exited (%v): %s", o.cfTunnel, p.exitError(), out.tail())
	case <-clock.ActiveClock.After(namedTunnelSettle):
	}
	return childExposure(p, base, "cloudflared"), nil
}

func runExposeTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, exposeCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.WaitDelay = time.Second
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()+string(out)))
	}
	return out, nil
}

func tailscaleDNSName(raw []byte) (string, error) {
	var st struct {
		Self struct{ DNSName string }
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("tailscale status --json: %w", err)
	}
	name := strings.TrimSuffix(st.Self.DNSName, ".")
	if name == "" || !dnsHostName.MatchString(name) {
		return "", errors.New("tailscale status --json has no usable Self.DNSName (is Tailscale up, with MagicDNS and HTTPS enabled?)")
	}
	return name, nil
}

// tailscaleHasRootHandler reports whether an https:443 root handler already
// exists, which `serve` would overwrite. An unreadable status is treated as
// "none known" because older clients have no `serve status --json`.
func tailscaleHasRootHandler(raw []byte) bool {
	var cfg struct {
		Web map[string]struct{ Handlers map[string]any }
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	for hostport, w := range cfg.Web {
		if strings.HasSuffix(hostport, ":443") {
			if _, ok := w.Handlers["/"]; ok {
				return true
			}
		}
	}
	return false
}

func startTailscale(ctx context.Context, port string) (*approvalExposure, error) {
	raw, err := runExposeTool(ctx, "tailscale", tailscaleStatusArgs...)
	if err != nil {
		return nil, err
	}
	host, err := tailscaleDNSName(raw)
	if err != nil {
		return nil, err
	}
	if cur, err := runExposeTool(ctx, "tailscale", tailscaleServeStatus...); err == nil && tailscaleHasRootHandler(cur) {
		return nil, errors.New("tailscale serve already has an https:443 handler; refusing to replace it (remove it yourself, or use --public-url with your own exposure)")
	}
	if _, err = runExposeTool(ctx, "tailscale", tailscaleServeArgs(port)...); err != nil {
		return nil, err
	}
	base, err := normalizePublicURL("https://" + host)
	if err != nil {
		return nil, err
	}
	off := "tailscale " + strings.Join(tailscaleServeOffArgs, " ")
	return &approvalExposure{url: base, stop: func(stderr io.Writer) {
		// Never `serve reset`: that would wipe the user's other handlers.
		if _, err := runExposeTool(context.Background(), "tailscale", tailscaleServeOffArgs...); err != nil {
			fmt.Fprintf(stderr, "could not remove the tailscale serve handler (%v); remove it with: %s\n", err, off)
		}
	}}, nil
}
