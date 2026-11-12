package approvals

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
)

var notifyTimeout = 10 * time.Second

// A notice without links (Kind set) is a run status notice from the
// controller: it carries no capability, only a title and a short message.
type approvalNotice struct {
	Run        string `json:"run"`
	Request    string `json:"request"`
	Kind       string `json:"kind,omitempty"`
	ApproveURL string `json:"approve_url,omitempty"`
	DenyURL    string `json:"deny_url,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	Title      string `json:"title"`
	Message    string `json:"message"`
}

type approvalNotifier interface {
	Name() string
	Notify(ctx context.Context, n approvalNotice) error
}

const (
	ntfyDefaultServer = "https://ntfy.sh"
	ntfyMinTopicLen   = 20
)

var ntfyTopicName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// notifyHTTPClient never follows redirects: a notice carries live
// capabilities and must only go to the address the caller named.
func notifyHTTPClient() *http.Client {
	return &http.Client{Timeout: notifyTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// notifySend performs the request and maps failures to errors that never
// include the URL (a topic or webhook URL can itself be a secret).
func notifySend(ctx context.Context, req *http.Request) error {
	resp, err := notifyHTTPClient().Do(req.WithContext(ctx))
	if err != nil {
		return notifyTransportError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Only the numeric code: the reason phrase is server-controlled text that
		// would otherwise reach terminal output and notification logs.
		return fmt.Errorf("server answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// notifyTransportError classifies a failed request into fixed text. Parser and
// transport errors can quote server-controlled bytes (a malformed status line, for
// example), and the URL can itself be a secret, so neither is passed through.
func notifyTransportError(err error) error {
	var ne net.Error
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.Is(err, context.Canceled):
		return errors.New("notification request was cancelled")
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return errors.New("notification request timed out")
	case errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid):
		return errors.New("notification server certificate was not trusted")
	case errors.As(err, new(*net.OpError)) || errors.As(err, new(*net.DNSError)):
		return errors.New("could not connect to the notification server")
	default:
		return errors.New("notification server sent an invalid response")
	}
}

type ntfyNotifier struct {
	server string
	topic  string
	token  string // only ever from IRRIGATOR_NTFY_TOKEN
}

func (ntfyNotifier) Name() string { return "ntfy" }

func (n ntfyNotifier) Notify(ctx context.Context, m approvalNotice) error {
	target, err := url.JoinPath(n.server, n.topic)
	if err != nil {
		return err
	}
	// ntfy "actions" header: http buttons POST to the link; view opens the
	// confirm page (GET never decides). The two decisions are independent.
	actions := ""
	if m.ApproveURL != "" {
		actions = fmt.Sprintf("http, Approve, %s, method=POST; http, Deny, %s, method=POST; view, Open, %s",
			m.ApproveURL, m.DenyURL, m.ApproveURL)
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(m.Message))
	if err != nil {
		return err
	}
	req.Header.Set("Title", mime.QEncoding.Encode("utf-8", m.Title))
	if actions != "" {
		req.Header.Set("Actions", actions)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	return notifySend(ctx, req)
}

type webhookNotifier struct{ url string }

func (webhookNotifier) Name() string { return "webhook" }

func (w webhookNotifier) Notify(ctx context.Context, m approvalNotice) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return notifySend(ctx, req)
}

func validNotifyURL(raw, what string, allowLoopbackHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%s must be an http(s) URL", what)
	}
	if u.User != nil {
		// Credentials belong in IRRIGATOR_NTFY_TOKEN, not in a URL that is logged and sent on.
		return "", fmt.Errorf("%s must not contain credentials in the URL", what)
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if !allowLoopbackHTTP || !(u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
			return "", fmt.Errorf("%s must use https (plain http is allowed only for loopback)", what)
		}
	}
	return u.String(), nil
}

func newNtfyNotifier(topicFlag string, stderr io.Writer) (approvalNotifier, error) {
	return newNtfyNotifierAt("", topicFlag, stderr)
}

// newNtfyNotifierAt prefers an explicit server over IRRIGATOR_NTFY_SERVER.
func newNtfyNotifierAt(serverFlag, topicFlag string, stderr io.Writer) (approvalNotifier, error) {
	server := serverFlag
	if server == "" {
		server = os.Getenv("IRRIGATOR_NTFY_SERVER")
	}
	if server == "" {
		server = ntfyDefaultServer
	}
	server, err := validNotifyURL(server, "IRRIGATOR_NTFY_SERVER", true)
	if err != nil {
		return nil, err
	}
	topic := topicFlag
	if topic == "" {
		topic = os.Getenv("IRRIGATOR_NTFY_TOPIC")
	}
	if !ntfyTopicName.MatchString(topic) {
		return nil, errors.New("ntfy needs a topic of 1-64 letters, digits, - or _ (--topic or IRRIGATOR_NTFY_TOPIC)")
	}
	if len(topic) < ntfyMinTopicLen {
		fmt.Fprintf(stderr, "warning: ntfy topic is shorter than %d characters; anyone who guesses it can read the approval links. Use a long random topic or an authenticated server.\n", ntfyMinTopicLen)
	}
	return ntfyNotifier{server: server, topic: topic, token: os.Getenv("IRRIGATOR_NTFY_TOKEN")}, nil
}

func newWebhookNotifier(urlFlag string) (approvalNotifier, error) {
	raw := urlFlag
	if raw == "" {
		raw = os.Getenv("IRRIGATOR_APPROVALS_WEBHOOK")
	}
	if raw == "" {
		return nil, errors.New("webhook needs --url or IRRIGATOR_APPROVALS_WEBHOOK")
	}
	u, err := validNotifyURL(raw, "webhook URL", true)
	if err != nil {
		return nil, err
	}
	return webhookNotifier{url: u}, nil
}

func approvalsNotify(args []string) (int, error) {
	return approvalsNotifyTo(args, os.Stdout, os.Stderr)
}

func approvalsNotifyTo(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("approvals notify", flag.ContinueOnError)
	run := fs.String("run", "", "run directory")
	request := fs.String("request", "", "blocked request id (a task id of the run)")
	ttl := fs.Duration("ttl", 5*time.Minute, "link lifetime (at most 24h)")
	via := fs.String("via", "", "notifier: ntfy or webhook")
	title := fs.String("title", "", "notification title (default names the request)")
	message := fs.String("message", "", "notification message")
	topic := fs.String("topic", "", "ntfy topic (default IRRIGATOR_NTFY_TOPIC)")
	hook := fs.String("url", "", "webhook URL (default IRRIGATOR_APPROVALS_WEBHOOK)")
	publicURL := fs.String("public-url", "", publicURLUsage)
	asJSON := fs.Bool("json", false, "print approve_url, deny_url, approve_nonce, deny_nonce and expires_at as JSON after sending (the links are capabilities; use only from a trusted caller)")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		return 2, err
	}
	if *run == "" || fs.NArg() != 0 || !checkpoint.MailboxTaskName.MatchString(*request) || *ttl <= 0 || *ttl > approvalMaxTTL || (*via != "ntfy" && *via != "webhook") {
		return 2, errors.New("usage: irrigator approvals notify --run DIR --request ID [--ttl 5m] --via ntfy|webhook [--title T] [--message M] [--topic T] [--url URL] [--public-url URL] [--json]")
	}
	if strings.ContainsAny(*title, "\r\n") {
		return 2, errors.New("--title must be a single line")
	}
	var notifier approvalNotifier
	var err error
	runRec, _ := readRunNotifyRecord(*run)
	if *via == "ntfy" {
		t := *topic
		if t == "" && runRec != nil && runRec.Topic != "" {
			t = runRec.Topic
		}
		notifier, err = newNtfyNotifier(t, stderr)
	} else {
		u := *hook
		if u == "" && runRec != nil && runRec.URL != "" {
			u = runRec.URL
		}
		notifier, err = newWebhookNotifier(u)
	}
	if err != nil {
		return 2, err
	}
	base := ""
	if *publicURL != "" {
		base, err = normalizePublicURL(*publicURL)
	} else {
		base, err = readPublicURL(*run)
	}
	if err != nil {
		return 2, err
	}
	if base == "" {
		return 2, errors.New("no public URL: start `approvals serve --expose ...` or `--public-url`, or pass --public-url")
	}
	expires := time.Now().Add(*ttl)
	approvePath, runID, err := mintApprovalLink(*run, *request, "approve", *ttl)
	if err != nil {
		return 1, err
	}
	denyPath, _, err := mintApprovalLink(*run, *request, "deny", *ttl)
	if err != nil {
		return 1, err
	}
	n := approvalNotice{Run: runID, Request: *request, ApproveURL: base + approvePath, DenyURL: base + denyPath,
		ExpiresAt: expires.UTC().Format(time.RFC3339), Title: *title, Message: *message}
	if n.Title == "" {
		n.Title = "Approval needed: " + *request
	}
	if n.Message == "" {
		n.Message = fmt.Sprintf("Request %s is blocked. Links expire at %s.", *request, n.ExpiresAt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	if err = notifier.Notify(ctx, n); err != nil {
		return 1, fmt.Errorf("notification via %s failed: %w (no link was consumed; they stay valid until %s)", notifier.Name(), err, n.ExpiresAt)
	}
	if *asJSON {
		// The caller asked for the capabilities (the ACP worker waits on the nonces).
		approveNonce, denyNonce := approvalNonce(approvePath), approvalNonce(denyPath)
		out := map[string]string{"approve_url": n.ApproveURL, "deny_url": n.DenyURL,
			"approve_nonce": approveNonce, "deny_nonce": denyNonce, "expires_at": n.ExpiresAt}
		return 0, json.NewEncoder(stdout).Encode(out)
	}
	// The links are capabilities: report the send, never the URLs.
	fmt.Fprintf(stdout, "sent approve and deny links for %s via %s (expire %s)\n", *request, notifier.Name(), n.ExpiresAt)
	return 0, nil
}

// approvalNonce reads the single-use nonce out of a freshly minted /action/token
// path. The token is our own, so the signature is not re-checked.
func approvalNonce(path string) string {
	parts := strings.Split(path, "/")
	body, _, _ := strings.Cut(parts[len(parts)-1], ".")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return ""
	}
	var c ApprovalClaims
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return c.Nonce
}
