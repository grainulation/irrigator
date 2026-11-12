package approvals

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clock"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/terminalview"
)

const (
	ApprovalTokenSchema = "irrigator.approval.token/v1"
	approvalDirName     = "approvals"
	approvalMaxTTL      = 24 * time.Hour
	approvalKeyBytes    = 32
)

type ApprovalClaims struct {
	Schema      string `json:"s"`
	RunID       string `json:"r"`
	Request     string `json:"q"`
	Action      string `json:"a"`
	Expires     int64  `json:"x"`
	Nonce       string `json:"n"`
	OperationID string `json:"o,omitempty"`
	Generation  int    `json:"g,omitempty"`
	Attempt     int    `json:"t,omitempty"`
}

// approvalRecord is one line of used.jsonl (token consumption) or
// decisions.jsonl (run-local ledger for runs without a mailbox).
type approvalRecord struct {
	Schema   string `json:"schema"`
	RunID    string `json:"run_id"`
	Request  string `json:"request"`
	Decision string `json:"decision"`
	Nonce    string `json:"nonce"`
	At       int64  `json:"at"`
	Via      string `json:"via,omitempty"`
}

var ErrApprovalTask = errors.New("approval request is not a task of this run")

func ApprovalDir(root string) string { return filepath.Join(root, approvalDirName) }

func approvalMAC(key []byte, body string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(ApprovalTokenSchema + "\n" + body))
	return m.Sum(nil)
}

// loadApprovalKey reads the per-run key and refuses a weak or loose one.
func loadApprovalKey(root string) ([]byte, error) {
	path := filepath.Join(ApprovalDir(root), "key")
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("approval key must be a regular file with mode 0600")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != approvalKeyBytes {
		return nil, errors.New("approval key has the wrong length")
	}
	return key, nil
}

// ensureApprovalKey creates the 32-byte key exclusively with mode 0600.
func ensureApprovalKey(root string) ([]byte, error) {
	dir := ApprovalDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "key")
	key := make([]byte, approvalKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadApprovalKey(root)
	}
	if err != nil {
		return nil, err
	}
	_, werr := f.Write(key)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	syncApprovalDir(dir)
	return loadApprovalKey(root)
}

func syncApprovalDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func mintApprovalToken(key []byte, runID, request, action string, expires time.Time) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	raw, err := json.Marshal(ApprovalClaims{Schema: ApprovalTokenSchema, RunID: runID, Request: request, Action: action, Expires: expires.Unix(), Nonce: hex.EncodeToString(nonce)})
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(approvalMAC(key, body)), nil
}

// verifyApprovalToken checks signature, run, action and expiry. Reuse is
// checked separately under the decision lock.
func verifyApprovalToken(key []byte, token, runID, action string, now time.Time) (ApprovalClaims, error) {
	var c ApprovalClaims
	if len(token) > 1024 {
		return c, errors.New("malformed")
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(sig, ".") {
		return c, errors.New("malformed")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, approvalMAC(key, body)) {
		return c, errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return c, errors.New("malformed")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil || c.Schema != ApprovalTokenSchema || !checkpoint.MailboxTaskName.MatchString(c.Request) || len(c.Nonce) != 32 {
		return ApprovalClaims{}, errors.New("malformed")
	}
	if c.Action != action {
		return ApprovalClaims{}, errors.New("wrong action")
	}
	if c.RunID != runID {
		return ApprovalClaims{}, errors.New("wrong run")
	}
	if now.Unix() > c.Expires {
		return ApprovalClaims{}, errors.New("expired")
	}
	return c, nil
}

func ValidApprovalAction(a string) bool { return a == "approve" || a == "deny" }

// loopbackApprovalAddr returns a dialable loopback host:port or refuses.
func loopbackApprovalAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid --addr: %w", err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("approvals serve refuses non-loopback addresses; expose it through a tunnel instead (docs/approvals.md)")
	}
	return net.JoinHostPort(host, port), nil
}

type approvalServer struct {
	root  string
	runID string
	key   []byte
	now   func() time.Time
	mu    sync.Mutex
}

func NewApprovalServer(root string) (*approvalServer, error) {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return nil, err
	}
	// Create the key if needed, so serve can start before the first link is
	// minted (a worker mints links only when an agent asks for permission).
	key, err := ensureApprovalKey(root)
	if err != nil {
		return nil, fmt.Errorf("approval key: %w", err)
	}
	return &approvalServer{root: root, runID: s.RunID, key: key, now: clock.ActiveClock.Now}, nil
}

// lockApprovals serializes decisions across goroutines and processes.
func (a *approvalServer) lockApprovals() (func(), error) {
	a.mu.Lock()
	f, err := os.OpenFile(filepath.Join(ApprovalDir(a.root), "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		a.mu.Unlock()
		return nil, err
	}
	return func() { checkpoint.Unlock(f); a.mu.Unlock() }, nil
}

func (a *approvalServer) used(nonce string) (bool, error) {
	f, err := os.Open(filepath.Join(ApprovalDir(a.root), "used.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r approvalRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			return false, errors.New("corrupt approval consumption log")
		}
		if r.Nonce == nonce {
			return true, nil
		}
	}
	return false, sc.Err()
}

// appendApprovalRecord writes one line with a single O_APPEND write, then
// syncs the file, so a record is either whole or absent.
func appendApprovalRecord(path string, r approvalRecord) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	if statErr != nil {
		syncApprovalDir(filepath.Dir(path))
	}
	return nil
}

func (a *approvalServer) mailboxMessage(c ApprovalClaims) (model.MailboxMessage, bool, error) {
	s, err := checkpoint.ReadState(a.root)
	if err != nil {
		return model.MailboxMessage{}, false, err
	}
	if !checkpoint.MailboxEnabled(s) {
		return model.MailboxMessage{}, false, nil
	}
	row := s.Tasks[c.Request]
	if row == nil {
		return model.MailboxMessage{}, true, ErrApprovalTask
	}
	// New links carry mint-time identity, so a retry between this read and
	// enqueue is rejected by the mailbox rather than retargeting the grant.
	op, generation, attempt := row.OperationID, row.Generation, row.Attempt
	if c.OperationID != "" {
		op, generation, attempt = c.OperationID, c.Generation, c.Attempt
	}
	return model.MailboxMessage{Schema: "irrigator.message/v1", Key: "approval" + c.Nonce, RunID: s.RunID, TaskID: c.Request, OperationID: op, Generation: generation, Attempt: attempt, Kind: "signal", Payload: "approval decision: " + c.Action + " request " + c.Request}, true, nil
}

// decide consumes the token and records the decision. The token is consumed
// first: if recording then fails the link is dead and a new one must be minted,
// which can never produce two decisions from one link.
func (a *approvalServer) Decide(c ApprovalClaims, via string) (int, string) {
	unlockFn, err := a.lockApprovals()
	if err != nil {
		return http.StatusInternalServerError, "decision not recorded"
	}
	defer unlockFn()
	// Queueing for the cross-process decision lock must not extend a link's
	// validity. MCP makes a synchronous authenticated decision without a link.
	if c.Expires != 0 && a.now().Unix() > c.Expires {
		return http.StatusForbidden, "expired"
	}
	if c.OperationID != "" {
		// Mailbox enqueue fences mint-time identity atomically. The approval
		// ledger needs the checkpoint lock through check and append so retry
		// cannot retarget a grant between those operations.
		pre, err := checkpoint.ReadState(a.root)
		if err != nil {
			return http.StatusInternalServerError, "decision not recorded"
		}
		if !checkpoint.MailboxEnabled(pre) {
			lease, err := checkpoint.FileLock(filepath.Join(a.root, "state.lock"), false)
			if err != nil {
				return http.StatusInternalServerError, "decision not recorded"
			}
			defer checkpoint.Unlock(lease)
		}
		if c.Expires != 0 && a.now().Unix() > c.Expires {
			return http.StatusForbidden, "expired"
		}
		s, err := checkpoint.ReadState(a.root)
		if err != nil {
			return http.StatusInternalServerError, "decision not recorded"
		}
		row := s.Tasks[c.Request]
		if s.RunID != c.RunID || row == nil || row.OperationID != c.OperationID || row.Generation != c.Generation || row.Attempt != c.Attempt {
			return http.StatusConflict, "stale attempt"
		}
	}
	if usedAlready, err := a.used(c.Nonce); err != nil {
		return http.StatusInternalServerError, "decision not recorded"
	} else if usedAlready {
		return http.StatusForbidden, "already used"
	}
	msg, mailbox, err := a.mailboxMessage(c)
	if errors.Is(err, ErrApprovalTask) {
		return http.StatusConflict, "unknown request"
	}
	if err != nil {
		return http.StatusInternalServerError, "decision not recorded"
	}
	rec := approvalRecord{"irrigator.approval/v1", a.runID, c.Request, c.Action, c.Nonce, a.now().Unix(), via}
	if err = appendApprovalRecord(filepath.Join(ApprovalDir(a.root), "used.jsonl"), rec); err != nil {
		return http.StatusInternalServerError, "decision not recorded"
	}
	if mailbox {
		_, err = messaging.EnqueueMailbox(a.root, msg)
	} else {
		err = appendApprovalRecord(filepath.Join(ApprovalDir(a.root), "decisions.jsonl"), rec)
	}
	if err != nil {
		return http.StatusInternalServerError, "decision not recorded; this link is spent, request a new one"
	}
	return http.StatusOK, a.pastTense(c.Action) + " " + c.Request
}

func (a *approvalServer) pastTense(action string) string {
	if action == "deny" {
		return "denied"
	}
	return "approved"
}

func (a *approvalServer) reply(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(text + "\n"))
}

func (a *approvalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 || !ValidApprovalAction(parts[0]) || r.URL.Path != "/"+parts[0]+"/"+parts[1] {
		a.reply(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
		a.reply(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	action := parts[0]
	c, err := verifyApprovalToken(a.key, parts[1], a.runID, action, a.now())
	if err != nil {
		a.reply(w, http.StatusForbidden, err.Error())
		return
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		code, text := a.Decide(c, r.Header.Get("Cf-Connecting-Ip"))
		a.reply(w, code, text)
		return
	}
	// GET/HEAD never decide: link previewers and mail scanners fetch URLs on their own.
	unlockFn, err := a.lockApprovals()
	if err != nil {
		a.reply(w, http.StatusInternalServerError, "unavailable")
		return
	}
	spent, err := a.used(c.Nonce)
	unlockFn()
	if err != nil {
		a.reply(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if spent {
		a.reply(w, http.StatusForbidden, "already used")
		return
	}
	label := strings.ToUpper(action[:1]) + action[1:]
	page := "<!doctype html><meta charset=utf-8><meta name=viewport content='width=device-width'><title>" + label + "</title>" +
		"<p>" + label + " request <b>" + html.EscapeString(c.Request) + "</b>?</p>" +
		"<form method=post><button>" + label + "</button></form>"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'")
	_, _ = w.Write([]byte(page))
}

const publicURLUsage = "public https base URL of the approvals listener (origin only)"

// mintApprovalLink validates the request and returns the link path
// /<action>/<token>. Tokens are stateless, so minting has no side effect
// beyond creating the run key.
func mintApprovalLink(run, request, action string, ttl time.Duration) (path, runID string, err error) {
	s, err := checkpoint.ReadState(run)
	if err != nil {
		return "", "", err
	}
	if checkpoint.MailboxEnabled(s) && s.Tasks[request] == nil {
		return "", "", ErrApprovalTask
	}
	key, err := ensureApprovalKey(run)
	if err != nil {
		return "", "", err
	}
	token, err := mintApprovalToken(key, s.RunID, request, action, clock.ActiveClock.Now().Add(ttl))
	if err != nil {
		return "", "", err
	}
	if row := s.Tasks[request]; row != nil {
		claims, err := verifyApprovalToken(key, token, s.RunID, action, clock.ActiveClock.Now())
		if err != nil {
			return "", "", err
		}
		claims.OperationID, claims.Generation, claims.Attempt = row.OperationID, row.Generation, row.Attempt
		raw, err := json.Marshal(claims)
		if err != nil {
			return "", "", err
		}
		body := base64.RawURLEncoding.EncodeToString(raw)
		token = body + "." + base64.RawURLEncoding.EncodeToString(approvalMAC(key, body))
	}
	return fmt.Sprintf("/%s/%s", action, token), s.RunID, nil
}

func approvalsLink(args []string) (int, error) {
	fs := flag.NewFlagSet("approvals link", flag.ContinueOnError)
	run := fs.String("run", "", "run directory")
	request := fs.String("request", "", "blocked request id (a task id of the run)")
	action := fs.String("action", "", "approve or deny")
	ttl := fs.Duration("ttl", 5*time.Minute, "link lifetime (at most 24h)")
	pathOnly := fs.Bool("path-only", false, "print only /action/token even when a public URL is known (for scripts)")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		return 2, err
	}
	if *run == "" || fs.NArg() != 0 || !checkpoint.MailboxTaskName.MatchString(*request) || !ValidApprovalAction(*action) || *ttl <= 0 || *ttl > approvalMaxTTL {
		return 2, errors.New("usage: irrigator approvals link --run DIR --request ID --action approve|deny [--ttl 5m] [--path-only]")
	}
	path, _, err := mintApprovalLink(*run, *request, *action, *ttl)
	if err != nil {
		return 1, err
	}
	public, err := readPublicURL(*run)
	if err != nil {
		return 1, err
	}
	tty := terminalview.LinksEnabled(os.Stdout)
	envBase := strings.TrimRight(os.Getenv("IRRIGATOR_APPROVALS_URL"), "/")
	switch {
	case tty && envBase != "":
		// IRRIGATOR_APPROVALS_URL wins: link the printed path to it.
		fmt.Println(terminalview.Hyperlink(envBase+path, path))
	case *pathOnly:
		if tty && public != "" {
			fmt.Println(terminalview.Hyperlink(public+path, path))
		} else {
			fmt.Println(path)
		}
	case public != "":
		fmt.Println(public + path)
	default:
		// Piped output stays the bare path, byte for byte.
		fmt.Println(path)
	}
	return 0, nil
}

type approvalsServeOptions struct {
	run    string
	addr   string
	expose exposeOptions
}

func approvalsServe(args []string) (int, error) {
	fs := flag.NewFlagSet("approvals serve", flag.ContinueOnError)
	run := fs.String("run", "", "run directory")
	addr := fs.String("addr", "127.0.0.1:8787", "loopback listen address")
	expose := fs.String("expose", exposeNone, "expose the listener through none|cloudflare|tailscale")
	publicURL := fs.String("public-url", "", publicURLUsage)
	cfTunnel := fs.String("cf-tunnel", "", "run this named cloudflared tunnel instead of a quick tunnel (needs --cf-hostname)")
	cfHostname := fs.String("cf-hostname", "", "public hostname of the named tunnel")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		return 2, err
	}
	usage := errors.New("usage: irrigator approvals serve --run DIR [--addr 127.0.0.1:PORT] [--expose none|cloudflare|tailscale] [--public-url URL] [--cf-tunnel NAME --cf-hostname HOST]")
	if *run == "" || fs.NArg() != 0 {
		return 2, usage
	}
	opts := approvalsServeOptions{run: *run, addr: *addr, expose: exposeOptions{mode: *expose, publicURL: *publicURL, cfTunnel: *cfTunnel, cfHostname: *cfHostname}}
	switch *expose {
	case exposeNone:
		if *cfTunnel != "" || *cfHostname != "" {
			return 2, errors.New("--cf-tunnel and --cf-hostname need --expose cloudflare")
		}
	case exposeCloudflare:
		if *publicURL != "" {
			return 2, errors.New("--public-url cannot be combined with --expose")
		}
		if (*cfTunnel == "") != (*cfHostname == "") {
			return 2, errors.New("--cf-tunnel and --cf-hostname must be given together")
		}
	case exposeTailscale:
		if *publicURL != "" || *cfTunnel != "" || *cfHostname != "" {
			return 2, errors.New("--expose tailscale takes no --public-url, --cf-tunnel or --cf-hostname")
		}
	default:
		return 2, fmt.Errorf("unknown --expose %q (none, cloudflare or tailscale)", *expose)
	}
	if *publicURL != "" {
		base, err := normalizePublicURL(*publicURL)
		if err != nil {
			return 2, err
		}
		opts.expose.publicURL = base
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveApprovals(ctx, opts, os.Stdout, os.Stderr)
}

// serveApprovals runs the listener until ctx ends, bringing up the requested
// exposure afterwards and tearing it down (and the public URL file) on every
// exit path.
func serveApprovals(ctx context.Context, o approvalsServeOptions, stdout, stderr io.Writer) (int, error) {
	listen, err := loopbackApprovalAddr(o.addr)
	if err != nil {
		return 2, err
	}
	srv, err := NewApprovalServer(o.run)
	if err != nil {
		return 1, err
	}
	// A URL file left by a crashed serve would point links at a dead tunnel.
	removePublicURL(o.run)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return 1, err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	done := make(chan error, 1)
	go func() { done <- hs.Serve(ln) }()
	shutdown := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}
	fmt.Fprintf(stdout, "approvals listening on http://%s\n", ln.Addr())

	var exposure *approvalExposure
	base := o.expose.publicURL
	if o.expose.mode != exposeNone {
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		if exposure, err = startApprovalExposure(ctx, o.expose, port); err != nil {
			shutdown()
			return 1, err
		}
		base = exposure.url
	}
	defer exposure.shutdown(stderr)
	if base != "" {
		if err = writePublicURL(o.run, base); err != nil {
			shutdown()
			return 1, err
		}
		defer removePublicURL(o.run)
		fmt.Fprintf(stdout, "approvals public URL %s\n", base)
	}
	var exited <-chan struct{}
	if exposure != nil {
		exited = exposure.exited
	}
	select {
	case err = <-done:
		return 1, err
	case <-exited:
		shutdown()
		return 1, exposure.err()
	case <-ctx.Done():
		shutdown()
		return 0, nil
	}
}

func ApprovalsCLI(args []string) (int, error) {
	usage := errors.New("usage: irrigator approvals link|serve|notify ...")
	if len(args) == 0 {
		return 2, usage
	}
	switch args[0] {
	case "link":
		return approvalsLink(args[1:])
	case "serve":
		return approvalsServe(args[1:])
	case "notify":
		return approvalsNotify(args[1:])
	}
	return 2, usage
}
