//go:build integration

package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func integrationCapture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	func() {
		defer func() { os.Stdout = old }()
		fn()
	}()
	w.Close()
	out := <-done
	r.Close()
	return string(out)
}

func TestDeliveryStatusIsDispatchedByCLI(t *testing.T) {
	if !strings.Contains(integrationCapture(t, func() { _, _ = runCommand([]string{"help"}) }), "delivery-status RUN_DIR") {
		t.Fatal("usage omits delivery-status")
	}
	e := dstatSetup(t)
	// No acceptance command: accepted must stay acceptance=not_configured.
	if code, err := e.deliver("", false); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	var code int
	var err error
	out := integrationCapture(t, func() { code, err = runCommand([]string{"delivery-status", e.root}) })
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	var view map[string]string
	if json.Unmarshal([]byte(out), &view) != nil || len(view) != 3 || view["stage"] != "accepted" || view["acceptance"] != "not_configured" || view["recovery"] != "none" {
		t.Fatalf("unexpected delivery-status output %q", out)
	}
	for _, args := range [][]string{{"delivery-status"}, {"delivery-status", e.root, "extra"}, {"delivery-status", "--json"}} {
		if c, cerr := runCommand(args); c != 2 || cerr == nil {
			t.Fatal(args, c, cerr)
		}
	}
	if c, cerr := runCommand([]string{"delivery-status", filepath.Join(e.base, "missing")}); c != 1 || cerr == nil {
		t.Fatal(c, cerr)
	}
}
