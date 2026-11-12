# Generation output token limits

Managed local Ollama profiles may explicitly configure a generation ceiling:

```json
"limits": {
  "input_bytes": 65536,
  "output_bytes": 65536,
  "max_output_tokens": 128
}
```

`max_output_tokens` accepts 1 through 512. It is supported only by `ollama_http`;
setting it on mock, Claude CLI or Codex CLI profiles fails catalog validation
before provider dispatch. Zero/omission defaults to 512 tokens.
Explicit capped Ollama profiles advertise `hard_output_token_limit`.

The adapter sends the exact value as Ollama's `options.num_predict`, with
`think: false`, no tools, no streaming and a pinned local completion model.
This is a generation-output limit enforced by a trusted local Ollama server,
not an aggregate input-plus-output allowance, a USD cap, a CLI-agent token cap,
or proof that an untrusted server stopped computing. The configured endpoint is
not authenticated server identity. Operate it only against a trusted local
server; fail-closed admission cannot make a malicious server honor options.

A completed response reporting more generated tokens than the configured limit
is rejected with an uncertain outcome. Its reported usage is retained as
evidence, no candidate output is accepted, and monetary cost stays unknown.
Reaching the generation ceiling with a truncated/non-stop response is also not a
successful artifact. Neither case automatically replays the request or releases
its potentially spent reservation. Existing reservation reconciliation applies.

A run-level `hard_token_limit` guardrail must be interpreted as this narrow,
explicit generation-output limit for every effective task profile. It must not
be enabled for a profile that merely reports trailing usage or has no configured
cap. It confers no other tool or runtime capability.

Verification uses local HTTP fixtures only:

```sh
go test ./internal/executor ./internal/managed -run '^Test(Ollama|OutputTokenLimit|ManagedOllama)' -count=1
```

Tests assert the outgoing exact cap, unsupported/out-of-range rejection before
inference, violating usage preservation, and limits encoding.
No test starts an Ollama server, downloads a model, or calls a real provider.
