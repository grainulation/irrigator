package model

import (
	"strings"
	"testing"
)

func TestStructuredOutputExhaustionDiagnostic(t *testing.T) {
	code, message := ProviderResultDiagnostic("error_max_structured_output_retries")
	if code != DiagProviderStructuredOutput || KnownDiagnosticCode(code) != code || !strings.Contains(message, "schema-valid") {
		t.Fatalf("%s: %s", code, message)
	}
	if !ProviderResultFailure(false, "error_max_structured_output_retries") {
		t.Fatal("failure subtype accepted")
	}
}
