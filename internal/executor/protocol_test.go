package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func protocolFixture() {
	var req model.WorkRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(2)
	}
	resp := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID, Output: map[string]string{"answer": req.Context["visible"] + "|" + os.Getenv("SWARM_ALLOWED_TEST") + "|" + os.Getenv("SWARM_HIDDEN_TEST")}}
	mode := os.Args[2]
	if mode == "mismatch" {
		resp.RequestID = "stale"
	}
	if mode == "reflect" || mode == "reflect-exit" {
		resp.Error = "auth failed: " + os.Getenv("SWARM_ALLOWED_TEST") + " (short value 1 stays)"
	}
	if mode == "reject" {
		b := false
		resp.Passed = &b
	}
	json.NewEncoder(os.Stdout).Encode(resp)
	if mode == "reflect-exit" {
		os.Exit(1)
	}
	if mode == "trailing" {
		fmt.Println(`{}`)
	}
	os.Exit(0)
}

func TestProviderProtocolAndEnvironment(t *testing.T) {
	t.Setenv("SWARM_ALLOWED_TEST", "allowed")
	t.Setenv("SWARM_HIDDEN_TEST", "hidden")
	binary, _ := os.Executable()
	for _, mode := range []string{"ok", "mismatch", "trailing"} {
		t.Run(mode, func(t *testing.T) {
			cell := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{binary, "_protocol_fixture", mode}, Env: []string{"SWARM_ALLOWED_TEST"}}, model.WorkRequest{Protocol: model.ProtocolVersion, RequestID: "request", Context: map[string]string{"visible": "context"}})
			if mode == "ok" {
				if err != nil || resp.Output["answer"] != "context|allowed|" {
					t.Fatalf("unexpected protocol/env: %#v %v", resp, err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestRequestIdentityAndNarrowContext(t *testing.T) {
	a := runfixture.Task("a", 0)
	a.Context = []string{"visible"}
	p := model.Plan{Name: "context", Tasks: []model.Task{a}, Context: map[string]string{"visible": "yes", "hidden": "no"}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(p, root, "mock"); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	r1 := request(s, a, 1, nil, model.Options{})
	r2 := request(s, a, 2, nil, model.Options{})
	if r1.OperationID != r2.OperationID || r1.RequestID == r2.RequestID || len(r1.Context) != 1 || r1.Context["visible"] != "yes" {
		t.Fatal("identity/context contract broken")
	}
}

// A provider that reflects a credential it was explicitly given into its error
// text must not put that value into the run's error message.
func TestProviderErrorScrubsForwardedCredential(t *testing.T) {
	const secret = "synthetic-provider-secret-0123456789"
	t.Setenv("SWARM_ALLOWED_TEST", secret)
	binary, _ := os.Executable()
	cell := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{binary, "_protocol_fixture", "reflect"}, Env: []string{"SWARM_ALLOWED_TEST"}}, model.WorkRequest{Protocol: model.ProtocolVersion, RequestID: "request"})
	if err == nil {
		t.Fatal("provider error was not reported")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "auth failed: [redacted]") {
		t.Fatalf("error = %q", err)
	}
}

func TestForwardedSecretValuesIgnoreShortAndUnsetVariables(t *testing.T) {
	t.Setenv("SWARM_SHORT_TEST", "1")
	t.Setenv("SWARM_LONG_TEST", "long-credential-value")
	vals := forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_SHORT_TEST", "SWARM_LONG_TEST", "SWARM_UNSET_TEST"}})
	if got := scrubForwarded("flag=1 token=long-credential-value", vals); got != "flag=1 token=[redacted]" {
		t.Fatalf("scrubbed = %q", got)
	}
	// A value that contains another is replaced whole, whichever is listed first.
	t.Setenv("SWARM_PREFIX_TEST", "abcdefgh")
	t.Setenv("SWARM_FULL_TEST", "abcdefghTOPSECRET")
	both := forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_PREFIX_TEST", "SWARM_FULL_TEST"}})
	if got := scrubForwarded("key=abcdefghTOPSECRET", both); got != "key=[redacted]" {
		t.Fatalf("overlap left a suffix: %q", got)
	}
}

// Providers echo a credential in encoded form as often as verbatim.
func TestScrubForwardedCoversEncodedEchoes(t *testing.T) {
	t.Setenv("SWARM_ENC_TEST", `abc"def ghi`)
	vals := forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_ENC_TEST"}})
	for _, echoed := range []string{`abc"def ghi`, "abc%22def+ghi", "abc%22def%20ghi", "abc%22def%20ghi", "%61bc%22def%20ghi", "YWJjImRlZiBnaGk=", "YWJjImRlZiBnaGk", `abc\"def ghi`} {
		if got := scrubForwarded("auth "+echoed+" end", vals); got != "auth [redacted] end" {
			t.Fatalf("%q survived: %q", echoed, got)
		}
	}
}

// Percent escapes may use either hex-digit case and may encode characters a URL
// encoder normally leaves alone.
func TestScrubForwardedPercentEncodingVariants(t *testing.T) {
	t.Setenv("SWARM_PCT_TEST", "secret/token")
	vals := forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_PCT_TEST"}})
	for _, echoed := range []string{"secret/token", "secret%2Ftoken", "secret%2ftoken", "%73ecret%2Ftoken", "%73%65%63%72%65%74%2f%74%6f%6b%65%6e"} {
		if got := scrubForwarded("auth "+echoed+" end", vals); got != "auth [redacted] end" {
			t.Fatalf("%q survived: %q", echoed, got)
		}
	}
	if got := scrubForwarded("auth secret and token end", vals); got != "auth secret and token end" {
		t.Fatalf("unrelated text altered: %q", got)
	}
}

// The scrubbed text must be what every return path hands back, including a
// provider that exits non-zero after printing its response.
func TestProviderErrorScrubbedOnNonZeroExit(t *testing.T) {
	const secret = "synthetic-provider-secret-0123456789"
	t.Setenv("SWARM_ALLOWED_TEST", secret)
	binary, _ := os.Executable()
	cell := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{binary, "_protocol_fixture", "reflect-exit"}, Env: []string{"SWARM_ALLOWED_TEST"}}, model.WorkRequest{Protocol: model.ProtocolVersion, RequestID: "request"})
	if err == nil || strings.Contains(resp.Error, secret) || strings.Contains(err.Error(), secret) {
		t.Fatalf("resp.Error=%q err=%v", resp.Error, err)
	}
}

// A very long credential (a certificate bundle) must not turn the percent-encoding
// pattern into an expensive match; it is still matched verbatim.
func TestScrubForwardedLongCredentialStaysFast(t *testing.T) {
	long := strings.Repeat("a", 20000) + "/"
	t.Setenv("SWARM_LONG_CERT_TEST", long)
	vals := forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_LONG_CERT_TEST"}})
	start := time.Now()
	got := scrubForwarded("auth "+long+" end", vals)
	if got != "auth [redacted] end" {
		t.Fatalf("long credential survived: %.40q", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("scrub took %v", elapsed)
	}
}

// Cost must stay linear: a repetitive credential against repetitive error text was
// seconds of regular-expression matching, and the response limit is 4 MiB.
func TestScrubForwardedIsLinearOnRepetitiveInput(t *testing.T) {
	cred := strings.Repeat("a", 4095) + "b"
	t.Setenv("SWARM_REPEAT_TEST", cred)
	vals := forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_REPEAT_TEST"}})
	text := strings.Repeat("a", 4<<20)
	start := time.Now()
	if got := scrubForwarded(text, vals); got != text {
		t.Fatal("unrelated text was altered")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("scrub of a 4 MiB error took %v", elapsed)
	}
	same := strings.Repeat("a", 4096)
	t.Setenv("SWARM_REPEAT_TEST", same)
	vals = forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_REPEAT_TEST"}})
	start = time.Now()
	if got := scrubForwarded(text, vals); got == text {
		t.Fatal("repeated credential not scrubbed")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("all-repeat scrub took %v", elapsed)
	}
	t.Setenv("SWARM_REPEAT_TEST", cred)
	vals = forwardedSecretValues(model.CommandSpec{Env: []string{"SWARM_REPEAT_TEST"}})
	encoded := strings.Repeat("a", 65536) + strings.ReplaceAll(cred, "b", "%62") + strings.Repeat("a", 10)
	if got := scrubForwarded(encoded, vals); strings.Contains(got, "%62") {
		t.Fatal("percent-encoded credential survived")
	}
}
