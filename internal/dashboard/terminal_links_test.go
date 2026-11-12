package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/approvals"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/terminalview"
)

// devNull is a character device, so it counts as a terminal for linksEnabled.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no " + os.DevNull)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestHyperlinkEnabledOnlyOnTerminals(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("IRRIGATOR_LINKS", "")
	tty := devNull(t)
	if !terminalview.LinksEnabled(tty) {
		t.Error("terminal should allow links")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if terminalview.LinksEnabled(w) {
		t.Error("pipe must not get links")
	}
	if terminalview.LinksEnabled(nil) {
		t.Error("nil file must not get links")
	}
	for _, off := range []string{"off", "OFF", "0", "false"} {
		t.Setenv("IRRIGATOR_LINKS", off)
		if terminalview.LinksEnabled(tty) {
			t.Errorf("IRRIGATOR_LINKS=%s must disable links", off)
		}
	}
	t.Setenv("IRRIGATOR_LINKS", "")
	t.Setenv("TERM", "dumb")
	if terminalview.LinksEnabled(tty) {
		t.Error("TERM=dumb must disable links")
	}
	// Links are not color: NO_COLOR does not turn them off.
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	if !terminalview.LinksEnabled(tty) {
		t.Error("NO_COLOR must not disable links")
	}
}

func TestHyperlinkPathLink(t *testing.T) {
	host, _ := os.Hostname()
	dir := filepath.Join(t.TempDir(), "my run")
	got := terminalview.PathLink(true, dir, "label")
	if !strings.HasPrefix(got, "\x1b]8;;file://"+host+"/") || !strings.Contains(got, "my%20run") || !strings.HasSuffix(got, "label\x1b]8;;\x1b\\") {
		t.Errorf("pathLink = %q", got)
	}
	if got := terminalview.PathLink(false, dir, "label"); got != "label" {
		t.Errorf("disabled pathLink = %q", got)
	}
}

func TestTerminalUILocationLinesLinkOnlyWhenEnabled(t *testing.T) {
	t.Setenv("TERM", "xterm")
	root := filepath.Join(t.TempDir(), "run")
	u := &TerminalUI{out: devNull(t), root: root, started: time.Now()}
	t.Setenv("IRRIGATOR_LINKS", "")
	got := u.locationLines()
	if strings.Count(got, "\x1b]8;;file://") != 2 || !strings.Contains(got, "run   ") || !strings.Contains(got, "cells ") {
		t.Errorf("linked lines = %q", got)
	}
	t.Setenv("IRRIGATOR_LINKS", "off")
	got = u.locationLines()
	if strings.Contains(got, "\x1b") || !strings.Contains(got, root) || !strings.Contains(got, filepath.Join(root, "cells")) {
		t.Errorf("plain lines = %q", got)
	}
}

func TestApprovalsLinkOutputIsBytePlainOffTerminal(t *testing.T) {
	root := runfixture.ApprovalLegacyRun(t)
	t.Setenv("TERM", "xterm")
	t.Setenv("IRRIGATOR_LINKS", "")
	t.Setenv("IRRIGATOR_APPROVALS_URL", "http://127.0.0.1:8787")
	// approvalCapture pipes stdout, so even with a base URL there is no escape.
	link := approvalLink(t, root, "a", "approve", "5m")
	if strings.Contains(link, "\x1b") || !strings.HasPrefix(link, "/approve/") {
		t.Errorf("piped link = %q", link)
	}
}

func approvalLink(t *testing.T, root, request, action, ttl string) string {
	t.Helper()
	out := runfixture.ApprovalCapture(t, func() {
		code, err := approvals.ApprovalsCLI([]string{"link", "--run", root, "--request", request, "--action", action, "--ttl", ttl})
		if err != nil || code != 0 {
			t.Fatalf("link: %d %v", code, err)
		}
	})
	return strings.TrimSpace(out)
}
