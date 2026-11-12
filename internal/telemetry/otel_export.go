package telemetry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/grainulation/irrigator/internal/buildinfo"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/textsafe"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const otelPollInterval = 500 * time.Millisecond

// otelFlushTimeout bounds the final flush so an unreachable collector cannot
// hold an interrupted exporter open.
const otelFlushTimeout = 10 * time.Second

func OtelCLI(args []string) (int, error) {
	usage := errors.New("usage: irrigator otel export RUN [--follow] [--since N] [--endpoint URL]")
	if len(args) == 0 || args[0] != "export" {
		return 2, usage
	}
	return otelExportCLI(args[1:])
}

func otelExportCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("otel export", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "follow events until interrupted")
	since := fs.Int("since", 0, "last event sequence already exported")
	endpoint := fs.String("endpoint", "", "OTLP/HTTP collector base URL; overrides OTEL_EXPORTER_OTLP_ENDPOINT (must not contain credentials)")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 || *since < 0 {
		return 2, errors.New("usage: irrigator otel export RUN [--follow] [--since N] [--endpoint URL]")
	}
	opts, err := otelExporterOptions(*endpoint)
	if err != nil {
		return 2, err
	}
	quietOTelSDK()
	exp, err := otlptracehttp.New(context.Background(), opts...)
	if err != nil {
		return 1, errors.New(otelErrorText(fmt.Errorf("create OTLP exporter: %w", err)))
	}
	return runOtelExport(fs.Arg(0), *since, *follow, exp)
}

// quietOTelSDK stops the OpenTelemetry SDK from logging on its own. Its
// configuration parsers log the raw value of a setting they cannot parse (a header,
// a timeout, a certificate path), and its default error handler prints export errors
// unsanitised. Failures are reported through this package's redacted paths instead.
func quietOTelSDK() {
	otel.SetLogger(logr.Discard())
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintln(os.Stderr, "otel:", otelErrorText(err))
	}))
}

// otelExporterOptions validates the protocol and --endpoint override. The
// standard OTEL_EXPORTER_OTLP_* variables (endpoint, headers, timeout,
// certificates) are read by the exporter itself, so header values never pass
// through this code, argv or any message.
func otelExporterOptions(endpoint string) ([]otlptracehttp.Option, error) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		if p := os.Getenv(name); p != "" && p != "http/protobuf" {
			return nil, fmt.Errorf("%s=%q is unsupported; only http/protobuf is available", name, p)
		}
	}
	var flagURL *url.URL
	if endpoint != "" {
		u, err := checkOTLPEndpoint(endpoint)
		if err != nil {
			return nil, fmt.Errorf("--endpoint: %w", err)
		}
		flagURL = u
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		// The exporter reads these itself and attaches OTEL_EXPORTER_OTLP_HEADERS to
		// the request, so the same destination rules apply as for --endpoint. The
		// value is never echoed: it may carry credentials.
		if v := os.Getenv(name); v != "" {
			if _, err := checkOTLPEndpoint(v); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if err := checkOTLPHeaders(); err != nil {
		return nil, err
	}
	if err := checkOTLPInsecure(endpoint); err != nil {
		return nil, err
	}
	client, err := otlpHTTPClient()
	if err != nil {
		return nil, err
	}
	warnInvalidOTLPTimeouts()
	opts := []otlptracehttp.Option{otlptracehttp.WithHTTPClient(client)}
	if flagURL == nil {
		return opts, nil
	}
	u := flagURL
	// Like OTEL_EXPORTER_OTLP_ENDPOINT, the value is a base URL.
	if !strings.HasSuffix(u.Path, "/v1/traces") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/traces"
	}
	return append(opts, otlptracehttp.WithEndpointURL(u.String())), nil
}

// otlpHeaderName is the HTTP header-name grammar (RFC 9110 token). The SDK logs a
// name that does not match, and a mistyped pair often has a credential in it.
var otlpHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// checkOTLPHeaders rejects malformed OTEL_EXPORTER_OTLP_*HEADERS before the SDK
// parses them: its parser logs the offending value, which is a credential.
// Pairs are name=value with a non-empty name and a valid percent-encoded value.
func checkOTLPHeaders() error {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		for _, pair := range strings.Split(raw, ",") {
			if strings.TrimSpace(pair) == "" {
				continue
			}
			key, value, ok := strings.Cut(pair, "=")
			if _, err := url.PathUnescape(value); !ok || !otlpHeaderName.MatchString(strings.TrimSpace(key)) || err != nil {
				return fmt.Errorf("%s is malformed (expected comma-separated name=value pairs: an HTTP header name and a percent-encoded value)", name)
			}
		}
	}
	return nil
}

// checkOTLPInsecure refuses OTEL_EXPORTER_OTLP_INSECURE for anything but a
// loopback collector. The SDK applies that variable after the endpoint was
// validated and would send the credential headers over plain http.
func checkOTLPInsecure(endpoint string) error {
	insecure := false
	for _, name := range []string{"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE"} {
		if strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "true") {
			insecure = true
		}
	}
	if !insecure {
		return nil
	}
	effective := firstNonEmpty(endpoint, os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if effective == "" {
		return nil // the SDK default is a loopback collector
	}
	u, err := url.Parse(effective)
	if err != nil || !loopbackHost(u.Hostname()) {
		return errors.New("OTEL_EXPORTER_OTLP_INSECURE is only allowed with a loopback collector; it would send the credential headers over plain http")
	}
	return nil
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// otlpHTTPClient returns the exporter's HTTP client. It never follows a redirect:
// the SDK's default client would re-send custom credential headers
// (OTEL_EXPORTER_OTLP_HEADERS) to whatever host a collector redirects to.
// Supplying a client replaces the SDK's own handling of the standard variables,
// so the documented ones are honoured here: timeout, the CA certificate and the
// client certificate and key (TRACES-specific names win over the general ones).
// Proxy settings come from the environment through the default transport.
func otlpHTTPClient() (*http.Client, error) {
	timeout := 10 * time.Second
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "OTEL_EXPORTER_OTLP_TIMEOUT"} {
		// Like the SDK: the first valid value wins, an invalid one falls through,
		// and 0 means no per-request timeout.
		if ms, err := strconv.Atoi(os.Getenv(name)); err == nil && ms >= 0 {
			timeout = time.Duration(ms) * time.Millisecond
			break
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if path := firstEnv("OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", "OTEL_EXPORTER_OTLP_CERTIFICATE"); path != "" {
		pem, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("OTEL_EXPORTER_OTLP_CERTIFICATE: cannot read the certificate file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("OTEL_EXPORTER_OTLP_CERTIFICATE: no PEM certificate found")
		}
		cfg.RootCAs = pool
	}
	// The certificate and key are a pair: the traces-specific pair wins whole when
	// either of its variables is set.
	certPath := os.Getenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE")
	keyPath := os.Getenv("OTEL_EXPORTER_OTLP_CLIENT_KEY")
	if tc, tk := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE"), os.Getenv("OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY"); tc != "" || tk != "" {
		certPath, keyPath = tc, tk
	}
	if certPath != "" || keyPath != "" {
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, errors.New("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE/KEY: cannot load the client certificate and key")
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	transport.TLSClientConfig = cfg
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// warnInvalidOTLPTimeouts tells the operator that a timeout variable is not an integer
// number of milliseconds and was ignored. The SDK used to say this itself; its logger
// is silenced because it prints raw values, so the warning names the variable only.
func warnInvalidOTLPTimeouts() {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "OTEL_EXPORTER_OTLP_TIMEOUT"} {
		if v := os.Getenv(name); v != "" {
			if ms, err := strconv.Atoi(v); err != nil || ms < 0 {
				fmt.Fprintf(os.Stderr, "warning: %s is not a non-negative integer number of milliseconds; ignoring it\n", name)
			}
		}
	}
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// checkOTLPEndpoint validates a collector URL that will receive span data and
// the OTEL_EXPORTER_OTLP_HEADERS credentials: http(s) only, no embedded
// credentials, query or fragment, and plain http only for a loopback collector.
func checkOTLPEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("must be an http:// or https:// URL")
	}
	if u.User != nil {
		return nil, errors.New("must not contain credentials; set OTEL_EXPORTER_OTLP_HEADERS instead")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not contain a query or fragment")
	}
	if u.Scheme == "http" {
		if !loopbackHost(u.Hostname()) {
			return nil, errors.New("must use https (plain http is allowed only for a loopback collector)")
		}
	}
	return u, nil
}

// safeOtelError carries text that has already been redacted and made terminal-safe,
// so later presentation does not escape it a second time.
type safeOtelError struct{ msg string }

func (e *safeOtelError) Error() string { return e.msg }

// otelErrorText returns err's message ready for a terminal: unchanged when it is
// already safe, redacted and escaped otherwise.
func otelErrorText(err error) string {
	var safe *safeOtelError
	if errors.As(err, &safe) {
		return safe.msg
	}
	return RedactOtelText(err.Error())
}

// reportingExporter records the most recent export failure; the batching span
// processor otherwise hands errors to a global handler only.
type reportingExporter struct {
	sdktrace.SpanExporter
	resource *resource.Resource
	failure  error
}

// pinnedResource makes a span report only the exporter's own resource. The SDK
// merges OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME into any resource it is
// given; those variables may carry identity (for example a user email set for
// Claude Code), which this exporter must never forward.
type pinnedResource struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (p pinnedResource) Resource() *resource.Resource { return p.resource }

func (r *reportingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	pinned := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, sp := range spans {
		pinned[i] = pinnedResource{sp, r.resource}
	}
	err := r.SpanExporter.ExportSpans(ctx, pinned)
	if err != nil {
		// The batching span processor hands this error to the SDK's global handler,
		// which logs it directly. Make it safe here, before it leaves the exporter.
		err = &safeOtelError{msg: RedactOtelText(err.Error())}
		r.failure = err
	}
	return err
}

func (r *reportingExporter) take() error {
	err := r.failure
	r.failure = nil
	return err
}

func runOtelExport(root string, since int, follow bool, exp sdktrace.SpanExporter) (int, error) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	s, err := checkpoint.ReadState(root)
	if err != nil {
		return 1, err
	}
	x := newOtelExporter(exp, s)
	defer x.Shutdown()
	warned := false
	for follow {
		x.Export(s, since, false)
		if err := x.Flush(context.Background()); err != nil {
			if !warned {
				fmt.Fprintln(os.Stderr, "warning: export failed, will retry:", otelErrorText(err))
			}
			warned = true
		} else {
			warned = false
		}
		select {
		case <-signals:
			follow = false
			continue
		case <-time.After(otelPollInterval):
		}
		next, err := checkpoint.ReadState(root)
		if err != nil {
			return 1, err
		}
		s = next
		x.refresh(s)
	}
	if s, err = checkpoint.ReadState(root); err != nil {
		return 1, err
	}
	x.refresh(s)
	n := x.Export(s, since, true)
	ctx, cancel := context.WithTimeout(context.Background(), otelFlushTimeout)
	defer cancel()
	if err := x.Flush(ctx); err != nil {
		return 1, errors.New("export failed: " + otelErrorText(err))
	}
	fmt.Printf("exported %d spans through event %d; resume with --since %d\n", n, len(s.Events), len(s.Events))
	return 0, nil
}

type otelEvent struct {
	seq  int
	time time.Time
	kind string
}

type otelAttempt struct {
	cell     string
	attempt  int
	start    time.Time
	end      time.Time
	startSeq int
	maxSeq   int
	outcome  string
	cost     *float64
	events   []otelEvent
}

type otelRunModel struct {
	attempts  []*otelAttempt
	runEvents []otelEvent
	runCells  map[int]string // run-level event seq -> cell id, when known
}

func (a *otelAttempt) open() bool { return a.outcome == "running" || a.outcome == "produced" }

var otelOutcomes = map[string]string{
	"verified":          "verified",
	"failed":            "failed",
	"cancelled":         "cancelled",
	"outcome_uncertain": "uncertain",
	"interrupted":       "interrupted",
	"produced":          "produced",
}

var otelEventName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func otelEventDetail(e model.Event) map[string]any {
	m, _ := e.Detail.(map[string]any)
	return m
}

func otelInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n == math.Trunc(n) && n >= 0 && n < 1<<31 {
			return int(n), true
		}
	case int:
		return n, n >= 0
	}
	return 0, false
}

// buildOtelModel groups events into cell attempts. Events after an attempt
// starts belong to it until the same cell's next start; a cell event before its
// first start (for example a cancellation) is a run-level event.
func buildOtelModel(events []model.Event) *otelRunModel {
	m := &otelRunModel{runCells: map[int]string{}}
	cells := map[string]bool{}
	for _, e := range events {
		if e.Kind == "started" && e.Task != "" {
			cells[e.Task] = true
		}
	}
	current := map[string]*otelAttempt{}
	pending := map[string][]otelEvent{}
	for i, e := range events {
		seq := i + 1
		ev := otelEvent{seq: seq, time: e.Time, kind: e.Kind}
		if !otelEventName.MatchString(ev.kind) {
			ev.kind = "event"
		}
		if e.Task == "" || !cells[e.Task] {
			m.runEvents = append(m.runEvents, ev)
			continue
		}
		cell := e.Task
		switch e.Kind {
		case "notes_delivered":
			pending[cell] = append(pending[cell], ev)
			continue
		case "started":
			n := 0
			if cur := current[cell]; cur != nil {
				n = cur.attempt
			}
			n++
			if v, ok := otelInt(otelEventDetail(e)["attempt"]); ok && v > 0 {
				n = v
			}
			a := &otelAttempt{cell: cell, attempt: n, start: e.Time, end: e.Time, startSeq: seq, maxSeq: seq, outcome: "running"}
			if p := pending[cell]; len(p) > 0 {
				a.events = append(a.events, p...)
				a.startSeq = p[0].seq
				delete(pending, cell)
			}
			a.events = append(a.events, ev)
			current[cell] = a
			m.attempts = append(m.attempts, a)
			continue
		}
		a := current[cell]
		if a == nil {
			m.runEvents = append(m.runEvents, ev)
			m.runCells[seq] = cell
			continue
		}
		a.events = append(a.events, ev)
		a.maxSeq = seq
		if e.Time.After(a.end) {
			a.end = e.Time
		}
		if outcome, ok := otelOutcomes[e.Kind]; ok {
			a.outcome = outcome
		}
		if e.Kind == "produced" {
			if c, ok := otelEventDetail(e)["reported_cost_usd"].(float64); ok && c >= 0 && !math.IsNaN(c) && !math.IsInf(c, 0) {
				a.cost = &c
			}
		}
	}
	// Notes that never led to a start stay visible at run level.
	for cell, p := range pending {
		for _, ev := range p {
			m.runEvents = append(m.runEvents, ev)
			m.runCells[ev.seq] = cell
		}
	}
	sort.SliceStable(m.runEvents, func(i, j int) bool { return m.runEvents[i].seq < m.runEvents[j].seq })
	return m
}

func otelTraceID(runID string) trace.TraceID {
	sum := sha256.Sum256([]byte("irrigator/otel/trace/v1\x00" + runID))
	var id trace.TraceID
	copy(id[:], sum[:])
	if !id.IsValid() {
		id[15] = 1
	}
	return id
}

func otelSpanID(runID string, parts ...string) trace.SpanID {
	sum := sha256.Sum256([]byte("irrigator/otel/span/v1\x00" + runID + "\x00" + strings.Join(parts, "\x00")))
	var id trace.SpanID
	copy(id[:], sum[:])
	if !id.IsValid() {
		id[7] = 1
	}
	return id
}

type otelIDKey struct{}
type otelIDs struct {
	trace trace.TraceID
	span  trace.SpanID
}

// otelIDGenerator hands the SDK the ids chosen by the caller through the
// context, so re-exporting the same events yields the same trace and span ids.
type otelIDGenerator struct{}

func (otelIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	ids, _ := ctx.Value(otelIDKey{}).(otelIDs)
	return ids.trace, ids.span
}

func (otelIDGenerator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	ids, _ := ctx.Value(otelIDKey{}).(otelIDs)
	return ids.span
}

type otelExporter struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	reporter *reportingExporter
	runID    string
	traceID  trace.TraceID
	rootID   trace.SpanID
	// sent is the highest event sequence exported per attempt span, so a
	// following exporter resends a span only when new events extend it.
	sent    map[string]int
	pending map[string]int
}

func otelSafe(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "")
	}
	return s
}

func otelRunID(s *model.State) string {
	if s.RunID != "" {
		return s.RunID
	}
	return s.PlanHash
}

func newOtelExporter(exp sdktrace.SpanExporter, s *model.State) *otelExporter {
	res := resource.NewWithAttributes("",
		attribute.String("service.name", "irrigator"),
		attribute.String("service.version", buildinfo.Version),
		attribute.String("irrigator.plan", otelSafe(s.Plan.Name, 128)),
	)
	reporter := &reportingExporter{SpanExporter: exp, resource: res}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithIDGenerator(otelIDGenerator{}),
		sdktrace.WithBatcher(reporter, sdktrace.WithBlocking(), sdktrace.WithMaxExportBatchSize(256), sdktrace.WithBatchTimeout(time.Hour)),
	)
	x := &otelExporter{provider: tp, tracer: tp.Tracer("irrigator"), reporter: reporter, sent: map[string]int{}, pending: map[string]int{}}
	x.refresh(s)
	return x
}

func (x *otelExporter) refresh(s *model.State) {
	x.runID = otelRunID(s)
	x.traceID = otelTraceID(x.runID)
	x.rootID = otelSpanID(x.runID, "run")
}

func (x *otelExporter) Flush(ctx context.Context) error {
	_ = x.reporter.take() // drop failures recorded before this flush
	err := x.provider.ForceFlush(ctx)
	if f := x.reporter.take(); f != nil {
		err = f
	}
	if err == nil {
		for k, v := range x.pending {
			x.sent[k] = v
		}
	}
	x.pending = map[string]int{}
	return err
}

func (x *otelExporter) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), otelFlushTimeout)
	defer cancel()
	_ = x.provider.Shutdown(ctx)
}

func (x *otelExporter) Start(ctx context.Context, ids otelIDs, name string, at time.Time, attrs []attribute.KeyValue, parent bool) (context.Context, trace.Span) {
	if parent {
		ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: x.traceID, SpanID: x.rootID, TraceFlags: trace.FlagsSampled}))
	}
	ctx = context.WithValue(ctx, otelIDKey{}, ids)
	return x.tracer.Start(ctx, name, trace.WithTimestamp(at), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
}

// export hands spans to the SDK and returns how many it produced. Attempts
// whose newest event is at or before since are skipped (resume); unfinished
// attempts wait unless final is set. The run span is written only when final.
func (x *otelExporter) Export(s *model.State, since int, final bool) int {
	model := buildOtelModel(s.Events)
	ctx := context.Background()
	count := 0
	for _, a := range model.attempts {
		key := fmt.Sprintf("%s\x00%d", a.cell, a.attempt)
		if a.maxSeq <= since || a.maxSeq <= x.sent[key] || (a.open() && !final) {
			continue
		}
		attrs := []attribute.KeyValue{
			attribute.String("irrigator.run_id", otelSafe(x.runID, 128)),
			attribute.String("irrigator.cell", otelSafe(a.cell, 128)),
			attribute.Int("irrigator.attempt", a.attempt),
			attribute.String("irrigator.outcome", a.outcome),
		}
		if a.cost != nil {
			attrs = append(attrs, attribute.Float64("irrigator.cost_usd", *a.cost))
		}
		ids := otelIDs{x.traceID, otelSpanID(x.runID, "cell", a.cell, fmt.Sprint(a.attempt))}
		_, span := x.Start(ctx, ids, "cell "+otelSafe(a.cell, 128), a.start, attrs, true)
		for _, ev := range a.events {
			span.AddEvent(ev.kind, trace.WithTimestamp(ev.time), trace.WithAttributes(attribute.Int("irrigator.seq", ev.seq)))
		}
		switch a.outcome {
		case "verified":
			span.SetStatus(codes.Ok, "")
		case "failed":
			span.SetStatus(codes.Error, "failed")
		case "uncertain":
			span.SetStatus(codes.Error, "outcome uncertain")
		}
		span.End(trace.WithTimestamp(a.end))
		x.pending[key] = a.maxSeq
		count++
	}
	if final && len(s.Events) > 0 {
		attrs := []attribute.KeyValue{attribute.String("irrigator.run_id", otelSafe(x.runID, 128))}
		first, last := s.Events[0].Time, s.Events[len(s.Events)-1].Time
		_, span := x.Start(ctx, otelIDs{x.traceID, x.rootID}, "run", first, attrs, false)
		for _, ev := range model.runEvents {
			if ev.seq <= since {
				continue
			}
			evAttrs := []attribute.KeyValue{attribute.Int("irrigator.seq", ev.seq)}
			if cell, ok := model.runCells[ev.seq]; ok {
				evAttrs = append(evAttrs, attribute.String("irrigator.cell", otelSafe(cell, 128)))
			}
			span.AddEvent(ev.kind, trace.WithTimestamp(ev.time), trace.WithAttributes(evAttrs...))
		}
		span.End(trace.WithTimestamp(last))
		count++
	}
	return count
}

var otelURLText = regexp.MustCompile(`https?://[^\s"'/]*(?:/[^\s"']*)?`)

// redactOtelText removes collector header values and reduces any URL to its
// scheme and host, so neither credentials nor tokens in paths reach the terminal.
func RedactOtelText(msg string) string {
	var secrets []string
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
		for _, pair := range strings.Split(os.Getenv(name), ",") {
			_, value, ok := strings.Cut(pair, "=")
			value = strings.TrimSpace(value)
			if !ok || value == "" {
				continue
			}
			secrets = append(secrets, value)
			// The SDK percent-decodes and then trims, so the transmitted value is the
			// trimmed decoded one.
			if decoded, err := url.PathUnescape(value); err == nil && decoded != value && decoded != "" {
				secrets = append(secrets, decoded)
				if trimmed := strings.TrimSpace(decoded); trimmed != "" && trimmed != decoded {
					secrets = append(secrets, trimmed)
				}
			}
		}
	}
	// Longest first, so a value that contains another never leaves a suffix.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	msg = otelURLText.ReplaceAllStringFunc(msg, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "[url]"
		}
		return u.Scheme + "://" + u.Hostname()
	})
	// Collector responses are remote-controlled: make control and bidi characters
	// inert before the text reaches a terminal.
	return textsafe.Terminal(msg)
}
