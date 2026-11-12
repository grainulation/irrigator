# Managed local Ollama

`ollama_http` is a managed adapter for an already running local Ollama server
and an already installed GGUF completion model. Use `swarm/v6` plans. It never
starts a server, downloads a model, launches an executable, executes tools, or
forwards credentials. HTTP goes directly to an explicit `127.0.0.1` or `[::1]`
port; proxies, redirects and retry loops are disabled.

An adapter declares `kind: "ollama_http"`, `protocol: "swarm.worker/v1"`, and
`endpoint: "http://127.0.0.1:11434"`. It has no command or executable hash. Its
instance has an empty `credential_refs` array. Its profile declares an explicit
model tag, `model_digest` (the lowercase 64-character digest from `/api/tags`),
`effort: "none"`, empty tools, byte limits and a positive operation budget.
Capabilities are `outcome_certainty` and `structured_strings`; explicit history
is unavailable. Normal output contracts also apply.

Before generation and after completion, the adapter requires exactly one matching
inventory tag and digest, local GGUF metadata, and completion capability. Remote
model/host metadata is rejected. Generation supplies the task output JSON schema,
a system instruction, no tools, `stream: false`, `think: false`, temperature zero,
and a fixed 512-token generation maximum. Long outputs that stop at the token
limit are rejected. The HTTP request, response headers, response body and total
operation duration are bounded. Responses must have the exact requested model,
one completed stop result, nonnegative integer token usage and valid task output;
remote routing, tool calls, duplicate JSON fields and noncanonical field aliases
are rejected.

The endpoint is a configured address, **not authenticated server identity**. The
model digest is provider-reported inventory evidence before and after the call,
not proof of immutable model execution. A server or tag can change between checks;
a malicious server can misreport metadata. Operate this adapter only with a trusted
local server. Loopback alone cannot guarantee that an untrusted server will not
forward input elsewhere.

After generation is attempted, cancellation, malformed output, transport errors,
postflight changes and persistence failures remain uncertain. Successful delivery
still reports `cost_unknown: true`: token counts do not establish a dollar price,
and local compute is not declared free. Existing managed receipts bind delivery
to the profile, endpoint and model digest through the binding digest; standard
budget holds/reconciliation apply.

## Validate your configuration

The adapter's HTTP fixtures exercise request/response contracts, cancellation,
output bounds, and preflight/postflight rejection without calling a model:

```sh
go test ./... -run 'TestManagedOllama|TestOllama' -count=1
```

Those tests do not verify your installed server or model. Before an authorized
live run, inspect the explicit endpoint, model tag and digest, then retain the
actual delivery receipt and output acceptance result. Token usage still does not
establish dollar cost. See [execution profiles](execution-profiles.md) and
[billing reconciliation](billing-reconciliation.md) for authority and accounting.

The wire implementation follows [Ollama generation](https://docs.ollama.com/api/generate),
[model inventory](https://docs.ollama.com/api/tags), and the
[official API types](https://github.com/ollama/ollama/blob/main/api/types.go).
