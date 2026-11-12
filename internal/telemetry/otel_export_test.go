package telemetry

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/buildinfo"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	otelPromptSecret = "PROMPT-SECRET-7f3a"
	otelOutputSecret = "OUTPUT-SECRET-91bc"
	otelErrorSecret  = "ERROR-SECRET-55de"
)

func otelTestState() *model.State {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	started := func(attempt int) map[string]any {
		return map[string]any{"attempt": float64(attempt), "pid": float64(42), "operation_id": "op-" + otelPromptSecret}
	}
	return &model.State{
		RunID: "run-123",
		Plan:  model.Plan{Name: "demo-plan", Tasks: []model.Task{{ID: "a", Prompt: otelPromptSecret}, {ID: "b", Prompt: otelPromptSecret}}},
		Tasks: map[string]*model.TaskState{"a": {Spec: model.Task{ID: "a", Prompt: otelPromptSecret}}, "b": {Spec: model.Task{ID: "b", Prompt: otelPromptSecret}}},
		Events: []model.Event{
			{Time: at(0), Kind: "created", Task: "", Detail: map[string]any{"backend": "mock"}},                                                                                       // 1
			{Time: at(1), Kind: "started", Task: "a", Detail: started(1)},                                                                                                             // 2
			{Time: at(2), Kind: "failed", Task: "a", Detail: otelErrorSecret},                                                                                                         // 3
			{Time: at(3), Kind: "retry_requested", Task: "a", Detail: map[string]any{"operation_id": "x"}},                                                                            // 4
			{Time: at(4), Kind: "notes_delivered", Task: "a", Detail: map[string]any{"attempt": float64(2), "notes": map[string]any{"k": otelOutputSecret}}},                          // 5
			{Time: at(5), Kind: "started", Task: "a", Detail: started(2)},                                                                                                             // 6
			{Time: at(6), Kind: "produced", Task: "a", Detail: map[string]any{"attempt": float64(2), "reported_cost_usd": 0.0125, "cost_unknown": false, "result": otelOutputSecret}}, // 7
			{Time: at(7), Kind: "verified", Task: "a", Detail: map[string]string{"validation": "checks", "error": ""}},                                                                // 8
			{Time: at(8), Kind: "started", Task: "b", Detail: started(1)},                                                                                                             // 9
			{Time: at(9), Kind: "produced", Task: "b", Detail: map[string]any{"attempt": float64(1), "reported_cost_usd": nil, "cost_unknown": true}},                                 // 10
			{Time: at(10), Kind: "outcome_uncertain", Task: "b", Detail: otelErrorSecret},                                                                                             // 11
			{Time: at(11), Kind: "cancelled", Task: "c", Detail: nil},                                                                                                                 // 12
			{Time: at(12), Kind: "paused_requested", Task: "", Detail: nil},                                                                                                           // 13
		},
	}
}

func exportOtelTest(t *testing.T, s *model.State, since int, final bool) (*tracetest.InMemoryExporter, int) {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	x := newOtelExporter(mem, s)
	// Shutting the in-memory exporter down clears its spans, so defer it.
	t.Cleanup(x.Shutdown)
	n := x.Export(s, since, final)
	if err := x.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	return mem, n
}

func otelAttrs(kvs []attribute.KeyValue) map[string]any {
	m := map[string]any{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.AsInterface()
	}
	return m
}

func findSpan(t *testing.T, spans tracetest.SpanStubs, cell string, attempt int) tracetest.SpanStub {
	t.Helper()
	for _, sp := range spans {
		a := otelAttrs(sp.Attributes)
		if a["irrigator.cell"] == cell && a["irrigator.attempt"] == int64(attempt) {
			return sp
		}
	}
	t.Fatalf("no span for %s attempt %d", cell, attempt)
	return tracetest.SpanStub{}
}

func TestOtelExportSpansAttributesAndStatus(t *testing.T) {
	mem, n := exportOtelTest(t, otelTestState(), 0, true)
	spans := mem.GetSpans()
	if n != 4 || len(spans) != 4 {
		t.Fatalf("expected 3 attempt spans and a run span, got %d/%d", n, len(spans))
	}
	a1, a2, b1 := findSpan(t, spans, "a", 1), findSpan(t, spans, "a", 2), findSpan(t, spans, "b", 1)
	if got := otelAttrs(a1.Attributes); got["irrigator.outcome"] != "failed" || got["irrigator.run_id"] != "run-123" {
		t.Fatalf("attempt 1 attributes: %v", got)
	}
	if _, ok := otelAttrs(a1.Attributes)["irrigator.cost_usd"]; ok {
		t.Fatal("cost must be absent when unknown")
	}
	if a1.Status.Code != codes.Error {
		t.Fatalf("failed attempt status %v", a1.Status)
	}
	got := otelAttrs(a2.Attributes)
	if got["irrigator.outcome"] != "verified" || got["irrigator.cost_usd"] != 0.0125 || a2.Status.Code != codes.Ok {
		t.Fatalf("attempt 2: %v %v", got, a2.Status)
	}
	if !a2.StartTime.Equal(time.Date(2026, 5, 1, 12, 0, 5, 0, time.UTC)) || !a2.EndTime.Equal(time.Date(2026, 5, 1, 12, 0, 7, 0, time.UTC)) {
		t.Fatalf("times come from events: %v - %v", a2.StartTime, a2.EndTime)
	}
	kinds := []string{}
	for _, e := range a2.Events {
		kinds = append(kinds, e.Name)
	}
	if strings.Join(kinds, ",") != "notes_delivered,started,produced,verified" {
		t.Fatalf("attempt 2 span events: %v", kinds)
	}
	if got := otelAttrs(b1.Attributes); got["irrigator.outcome"] != "uncertain" || b1.Status.Code != codes.Error {
		t.Fatalf("uncertain attempt: %v %v", got, b1.Status)
	}
	if _, ok := otelAttrs(b1.Attributes)["irrigator.cost_usd"]; ok {
		t.Fatal("unknown cost must not be exported as zero")
	}
	// Parenting: all attempts share one trace and hang off the run span.
	var root tracetest.SpanStub
	for _, sp := range spans {
		if sp.Name == "run" {
			root = sp
		}
	}
	for _, sp := range []tracetest.SpanStub{a1, a2, b1} {
		if sp.SpanContext.TraceID() != root.SpanContext.TraceID() || sp.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Fatalf("span %s is not a child of the run span", sp.Name)
		}
	}
	runKinds := []string{}
	for _, e := range root.Events {
		runKinds = append(runKinds, e.Name)
	}
	if strings.Join(runKinds, ",") != "created,cancelled,paused_requested" {
		t.Fatalf("run-level events: %v", runKinds)
	}
	res := map[string]any{}
	for _, kv := range root.Resource.Attributes() {
		res[string(kv.Key)] = kv.Value.AsInterface()
	}
	if res["service.name"] != "irrigator" || res["service.version"] != buildinfo.Version || res["irrigator.plan"] != "demo-plan" {
		t.Fatalf("resource: %v", res)
	}
}

func TestOtelExportDeterministicIDs(t *testing.T) {
	ids := func(s *model.State) map[string]string {
		mem, _ := exportOtelTest(t, s, 0, true)
		out := map[string]string{}
		for _, sp := range mem.GetSpans() {
			out[sp.Name+fmt.Sprint(otelAttrs(sp.Attributes)["irrigator.attempt"])] = sp.SpanContext.TraceID().String() + "/" + sp.SpanContext.SpanID().String()
		}
		return out
	}
	first, second := ids(otelTestState()), ids(otelTestState())
	if len(first) != 4 {
		t.Fatalf("got %v", first)
	}
	for k, v := range first {
		if second[k] != v {
			t.Fatalf("%s changed between exports: %s != %s", k, v, second[k])
		}
	}
	other := otelTestState()
	other.RunID = "run-456"
	for k, v := range ids(other) {
		if first[k] == v {
			t.Fatalf("%s must differ between runs", k)
		}
	}
	seen := map[string]bool{}
	for _, v := range first {
		if seen[v] {
			t.Fatalf("duplicate id %s", v)
		}
		seen[v] = true
	}
}

func TestOtelExportSinceResume(t *testing.T) {
	s := otelTestState()
	// Events 1-8 were consumed: only cell b, which ends after event 8, remains.
	mem, _ := exportOtelTest(t, s, 8, true)
	cells := []string{}
	for _, sp := range mem.GetSpans() {
		if c, ok := otelAttrs(sp.Attributes)["irrigator.cell"]; ok {
			cells = append(cells, c.(string))
		}
	}
	if strings.Join(cells, ",") != "b" {
		t.Fatalf("resume exported %v", cells)
	}
	// Resuming past every event exports no attempts.
	mem, _ = exportOtelTest(t, s, len(s.Events), true)
	for _, sp := range mem.GetSpans() {
		if sp.Name != "run" {
			t.Fatalf("unexpected attempt span %s after the last event", sp.Name)
		}
	}
	// Ids from a resumed export match the ones from a full export.
	full, _ := exportOtelTest(t, s, 0, true)
	if findSpan(t, full.GetSpans(), "b", 1).SpanContext.SpanID() != findSpan(t, mem0(t, s), "b", 1).SpanContext.SpanID() {
		t.Fatal("resumed span id differs")
	}
}

func mem0(t *testing.T, s *model.State) tracetest.SpanStubs {
	m, _ := exportOtelTest(t, s, 8, true)
	return m.GetSpans()
}

func TestOtelFollowWaitsForFinishedAttemptsAndResendsOnlyGrowth(t *testing.T) {
	s := otelTestState()
	s.Events = s.Events[:6] // cell a attempt 2 just started
	mem := tracetest.NewInMemoryExporter()
	x := newOtelExporter(mem, s)
	defer x.Shutdown()
	x.Export(s, 0, false)
	if err := x.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mem.GetSpans(); len(got) != 1 || otelAttrs(got[0].Attributes)["irrigator.attempt"] != int64(1) {
		t.Fatalf("only the finished attempt may be exported while following: %d spans", len(got))
	}
	x.Export(s, 0, false)
	x.Flush(context.Background())
	if len(mem.GetSpans()) != 1 {
		t.Fatal("an unchanged attempt must not be sent twice")
	}
	s = otelTestState()
	x.Export(s, 0, false)
	x.Flush(context.Background())
	if len(mem.GetSpans()) != 3 {
		t.Fatalf("expected attempts a/2 and b/1(open->uncertain) to join, got %d", len(mem.GetSpans()))
	}
}

func TestOtelExportOmitsPromptsOutputsAndErrors(t *testing.T) {
	s := otelTestState()
	s.Plan.Tasks[0].Prompt = otelPromptSecret
	t.Setenv("USER", "someone-private")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "user.email=private@example.com")
	mem, _ := exportOtelTest(t, s, 0, true)
	raw, err := json.Marshal(mem.GetSpans())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{otelPromptSecret, otelOutputSecret, otelErrorSecret, "someone-private", "private@example.com", "user.email", "operation_id", "op-"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("exported telemetry contains %q", forbidden)
		}
	}
}

func TestOtelEndpointValidation(t *testing.T) {
	for _, bad := range []string{"ftp://host", "http://", "https://user:pw@collector:4318", "http://collector?token=abc", "collector:4318", "http://collector.example:4318", "http://10.0.0.5:4318"} {
		_, err := otelExporterOptions(bad)
		if err == nil {
			t.Fatalf("%q accepted", bad)
		}
		for _, leak := range []string{"pw@", "token=abc"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error echoes the endpoint: %v", err)
			}
		}
	}
	for _, good := range []string{"http://127.0.0.1:4318", "https://collector.example/prefix/", "http://127.0.0.1:4318/v1/traces"} {
		if opts, err := otelExporterOptions(good); err != nil || len(opts) != 2 {
			t.Fatalf("%q: %v", good, err)
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	if _, err := otelExporterOptions(""); err == nil {
		t.Fatal("grpc protocol must be rejected")
	}
}

// The OTEL_EXPORTER_OTLP_* endpoint variables are read by the SDK, which attaches
// OTEL_EXPORTER_OTLP_HEADERS to the request, so they follow the same rules as
// --endpoint and must not be echoed when rejected.
func TestOtelEnvEndpointValidation(t *testing.T) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		for _, bad := range []string{"http://collector.example:4318", "https://user:pw@collector.example", "https://collector.example?token=abc"} {
			t.Setenv(name, bad)
			_, err := otelExporterOptions("")
			if err == nil {
				t.Fatalf("%s=%q accepted", name, bad)
			}
			for _, leak := range []string{"collector.example", "pw@", "token=abc"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error echoes the %s value: %v", name, err)
				}
			}
		}
		for _, good := range []string{"http://127.0.0.1:4318", "http://localhost:4318", "https://collector.example/v1/traces"} {
			t.Setenv(name, good)
			if _, err := otelExporterOptions(""); err != nil {
				t.Fatalf("%s=%q: %v", name, good, err)
			}
		}
		t.Setenv(name, "")
	}
}

func TestRedactOtelText(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20s3cr3t-token,x-team=blue-team-key")
	got := RedactOtelText("post https://user:pw@collector.example:4318/v1/traces?key=abc failed: Bearer s3cr3t-token blue-team-key")
	for _, leak := range []string{"s3cr3t-token", "blue-team-key", "pw", "key=abc", "/v1/traces"} {
		if strings.Contains(got, leak) {
			t.Fatalf("redacted text still contains %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "https://collector.example") {
		t.Fatalf("host should remain for diagnosis: %s", got)
	}
}

// TestOtelExportEndToEnd sends a stored run to a local OTLP/HTTP collector and
// checks that the configured header reaches the collector, is never printed, and
// that a rejected export reports a failure without echoing it.
func TestOtelExportEndToEnd(t *testing.T) {
	const token = "Bearer end-to-end-token-123"
	var gotAuth, gotPath string
	var bodyBytes int
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		bodyBytes = len(b)
		w.WriteHeader(status)
		if status != http.StatusOK {
			fmt.Fprint(w, "rejected "+r.Header.Get("Authorization"))
		}
	}))
	defer srv.Close()
	fixture := otelTestState()
	run := filepath.Join(t.TempDir(), "run")
	tasks := []model.Task{runfixture.Task("a", 0), runfixture.Task("b", 0)}
	for i := range tasks {
		tasks[i].Prompt = otelPromptSecret
	}
	if err := checkpoint.Initialize(model.Plan{Name: fixture.Plan.Name, Tasks: tasks}, run, "mock"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(run, func(s *model.State) error {
		s.RunID, s.Events = fixture.RunID, fixture.Events
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization="+strings.ReplaceAll(token, " ", "%20"))
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "2000")
	if _, err := otelExporterOptions(srv.URL); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

	stdout, _ := captureOtelOutput(t, func() {
		code, err := otelExportCLI([]string{run, "--endpoint", srv.URL})
		if code != 0 || err != nil {
			t.Fatalf("export: %d %v", code, err)
		}
	})
	if gotAuth != token || gotPath != "/v1/traces" || bodyBytes == 0 {
		t.Fatalf("collector saw auth=%q path=%q bytes=%d", gotAuth, gotPath, bodyBytes)
	}
	if !strings.Contains(stdout, "resume with --since 13") {
		t.Fatalf("summary: %q", stdout)
	}
	status = http.StatusBadRequest
	stdout, stderr := captureOtelOutput(t, func() {
		code, err := otelExportCLI([]string{run, "--endpoint", srv.URL})
		if code == 0 || err == nil {
			t.Fatal("a rejected export must fail")
		}
		fmt.Fprintln(os.Stderr, err)
	})
	if strings.Contains(stdout+stderr, "end-to-end-token") {
		t.Fatalf("header value leaked: %q %q", stdout, stderr)
	}
}

func captureOtelOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()
	read := func(f **os.File) (func() string, func()) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := *f
		*f = w
		done := make(chan string)
		go func() { b, _ := io.ReadAll(r); done <- string(b) }()
		return func() string { w.Close(); *f = old; return <-done }, func() {}
	}
	finishOut, _ := read(&os.Stdout)
	finishErr, _ := read(&os.Stderr)
	fn()
	return finishOut(), finishErr()
}

var _ sdktrace.SpanExporter = (*reportingExporter)(nil)

// A collector that redirects must not receive the exporter's custom credential
// headers at the redirect target: the response is returned as it is.
func TestOtlpClientDoesNotFollowRedirects(t *testing.T) {
	var reached int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	t.Cleanup(redirector.Close)
	client, err := otlpHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, redirector.URL, strings.NewReader("x"))
	req.Header.Set("X-Api-Key", "synthetic-key")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || reached != 0 {
		t.Fatalf("status=%d, redirect target reached %d times", resp.StatusCode, reached)
	}
}

// Supplying our own client replaces the SDK's handling of the standard variables,
// so the documented timeout and certificate settings are verified here.
func TestOtlpClientHonoursTimeoutAndCertificateVariables(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	client, err := otlpHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 10*time.Second {
		t.Fatalf("default timeout = %v", client.Timeout)
	}
	if resp, err := client.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("untrusted certificate accepted without OTEL_EXPORTER_OTLP_CERTIFICATE")
	}
	certFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certFile)
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "2500")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "1500")
	client, err = otlpHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 1500*time.Millisecond {
		t.Fatalf("traces timeout must win: %v", client.Timeout)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("certificate from OTEL_EXPORTER_OTLP_CERTIFICATE rejected: %v", err)
	}
	resp.Body.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", filepath.Join(t.TempDir(), "missing.pem"))
	if _, err := otlpHTTPClient(); err == nil || strings.Contains(err.Error(), "missing.pem") {
		t.Fatalf("unreadable certificate: %v", err)
	}
}

// The SDK applies OTEL_EXPORTER_OTLP_INSECURE after the endpoint was validated and
// would send the credential headers over plain http to an https endpoint's host.
func TestOtelInsecureOnlyAllowedForLoopback(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	for _, endpoint := range []string{"https://collector.example:4318"} {
		if _, err := otelExporterOptions(endpoint); err == nil {
			t.Fatalf("insecure accepted with %q", endpoint)
		}
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
		if _, err := otelExporterOptions(""); err == nil {
			t.Fatalf("insecure accepted with env endpoint %q", endpoint)
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	for _, ok := range []string{"", "http://127.0.0.1:4318", "http://localhost:4318"} {
		if _, err := otelExporterOptions(ok); err != nil {
			t.Fatalf("loopback with insecure rejected (%q): %v", ok, err)
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "false")
	if _, err := otelExporterOptions("https://collector.example:4318"); err != nil {
		t.Fatal(err)
	}
}

// The SDK's header parser logs the offending value, which is a credential, so a
// malformed value is rejected first with fixed text.
func TestOtelMalformedHeadersRejectedWithoutEcho(t *testing.T) {
	for _, bad := range []string{"x-api-key=SYNTHETIC-CREDENTIAL%ZZ", "novalue", "=onlyvalue", "ok=1,SYNTHETIC-CREDENTIAL", "Bearer SYNTHETIC-CREDENTIAL=value", "bad name=1", "na(me=1"} {
		for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
			t.Setenv(name, bad)
			_, err := otelExporterOptions("")
			if err == nil {
				t.Fatalf("%s=%q accepted", name, bad)
			}
			if strings.Contains(err.Error(), "SYNTHETIC-CREDENTIAL") || strings.Contains(err.Error(), "onlyvalue") {
				t.Fatalf("error echoes the value: %v", err)
			}
			t.Setenv(name, "")
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20abc, x-team=blue,X_Api.Key-2=v")
	if _, err := otelExporterOptions(""); err != nil {
		t.Fatalf("valid headers rejected: %v", err)
	}
}

func TestRedactOtelTextOverlapAndTerminalSafety(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "a=abcdef,b=abcdefTOPSECRET")
	got := RedactOtelText("key abcdefTOPSECRET failed")
	if strings.Contains(got, "TOPSECRET") {
		t.Fatalf("overlap left a suffix: %q", got)
	}
	got = RedactOtelText("400 REMOTE-REASON body: \x1b]52;c;UFlMT0FE\a end")
	if strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("control bytes reached the output: %q", got)
	}
}

func TestOtlpTimeoutFollowsSDKSemantics(t *testing.T) {
	cases := []struct {
		traces, general string
		want            time.Duration
	}{
		{"", "", 10 * time.Second},
		{"", "2500", 2500 * time.Millisecond},
		{"bad", "2500", 2500 * time.Millisecond},
		{"-1", "2500", 2500 * time.Millisecond},
		{"0", "2500", 0},
		{"1500", "2500", 1500 * time.Millisecond},
		{"bad", "also-bad", 10 * time.Second},
	}
	for _, c := range cases {
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", c.traces)
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", c.general)
		client, err := otlpHTTPClient()
		if err != nil {
			t.Fatal(err)
		}
		if client.Timeout != c.want {
			t.Fatalf("traces=%q general=%q: timeout %v, want %v", c.traces, c.general, client.Timeout, c.want)
		}
	}
}

// The batching span processor passes export errors to the SDK's global handler,
// which logs them directly, so the exporter must hand back already-safe text.
func TestReportingExporterSanitizesErrorsItReturns(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=SYNTHETIC-SECRET")
	inner := failingExporter{err: errors.New("failed to send to https://collector.example/v1/traces: 400 (body: SYNTHETIC-SECRET \x1b]52;c;UEFZ\a)")}
	exp := &reportingExporter{SpanExporter: inner}
	err := exp.ExportSpans(context.Background(), nil)
	if err == nil {
		t.Fatal("error was swallowed")
	}
	for _, got := range []string{err.Error(), exp.failure.Error()} {
		if strings.Contains(got, "SYNTHETIC-SECRET") || strings.ContainsAny(got, "\x1b\a") || strings.Contains(got, "/v1/traces") {
			t.Fatalf("unsafe error text: %q", got)
		}
	}
}

type failingExporter struct{ err error }

func (f failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return f.err }
func (f failingExporter) Shutdown(context.Context) error                             { return nil }

// The SDK percent-decodes and then trims a header value, so the value on the wire is
// the trimmed one and that is what must be redacted.
func TestRedactOtelTextCoversTrimmedDecodedHeader(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=%20SYNTHETIC-SECRET%20")
	if got := RedactOtelText("denied SYNTHETIC-SECRET"); strings.Contains(got, "SYNTHETIC-SECRET") {
		t.Fatalf("trimmed value survived: %q", got)
	}
}

// Text that was already made safe is not escaped again (visible \e would become \\e).
func TestOtelErrorTextDoesNotDoubleEscape(t *testing.T) {
	safe := &safeOtelError{msg: RedactOtelText("body " + string(rune(0x1b)) + "[31m")}
	if got := otelErrorText(safe); got != safe.msg {
		t.Fatalf("already-safe text changed: %q -> %q", safe.msg, got)
	}
	if got := otelErrorText(errors.New("raw " + string(rune(0x1b)))); strings.ContainsRune(got, 0x1b) {
		t.Fatalf("raw error not escaped: %q", got)
	}
}

// The --endpoint value is validated before any credential-bearing setting is looked at.
func TestOtelEndpointValidatedBeforeHeaders(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "bad header name=1")
	_, err := otelExporterOptions("ftp://invalid.example")
	if err == nil || !strings.Contains(err.Error(), "--endpoint") {
		t.Fatalf("got %v", err)
	}
}

// The SDK's own logger prints the raw value of a setting it cannot parse; after
// quietOTelSDK nothing from it reaches the terminal, and its error handler is sanitized.
func TestQuietOTelSDKSanitizesHandlerOutput(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=SYNTHETIC-SECRET")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	quietOTelSDK()
	otel.Handle(errors.New("export failed: SYNTHETIC-SECRET " + string(rune(0x1b)) + "]52;c;x"))
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if strings.Contains(string(out), "SYNTHETIC-SECRET") || strings.ContainsRune(string(out), 0x1b) || !strings.Contains(string(out), "otel:") {
		t.Fatalf("handler output: %q", out)
	}
}

func TestInvalidOTLPTimeoutIsReportedWithoutItsValue(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "10s-SYNTHETIC")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	_, optErr := otelExporterOptions("")
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if optErr != nil {
		t.Fatal(optErr)
	}
	if !strings.Contains(string(out), "OTEL_EXPORTER_OTLP_TIMEOUT") || strings.Contains(string(out), "SYNTHETIC") {
		t.Fatalf("warning = %q", out)
	}
}
