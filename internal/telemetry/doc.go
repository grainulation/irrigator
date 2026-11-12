// Package telemetry exports run events as OpenTelemetry traces.
//
// # Responsibility
//
// Conversion of run events into cell-attempt spans and their OTLP export (`irrigator otel`). It
// reads run events only.
//
// # Allowed dependencies
//
// Layer 23. May import only lower layers; today it imports buildinfo, checkpoint, commandflags,
// model, textsafe. Third-party: github.com/go-logr/logr, go.opentelemetry.io/otel, go.opentelemetry.io/otel/attribute, go.opentelemetry.io/otel/codes,
// go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp,
// go.opentelemetry.io/otel/sdk/resource, go.opentelemetry.io/otel/sdk/trace,
// go.opentelemetry.io/otel/trace. The compiler rejects import cycles; the layer order is
// recorded in docs/architecture.md.
//
// # Invariants
//
//   - Export is read-only with respect to the run.
//   - The final flush is bounded so an unreachable collector cannot hold an interrupted
//     exporter open.
//   - Collector header values and URLs are redacted from any error text, and collector-supplied
//     text is made terminal-safe.
//   - Credential headers are only sent to a validated destination: no credentials in the URL,
//     https unless loopback (including OTEL_EXPORTER_OTLP_INSECURE), and redirects are not
//     followed.
//   - A span reports only the exporter's own resource.
package telemetry
