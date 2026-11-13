# Offline workflow composition

`irrigator compose composition.json --out new-plan.json > provenance.json`
compiles reusable local workflows into an ordinary `swarm/v4` plan (v1) or `swarm/v6` plan (v2). It never
applies the plan, starts a provider, executes a template command, fetches a URL,
or probes credentials. The output must not exist; it is created privately with
exclusive creation. Review the plan and save the JSON provenance printed to stdout
before separately admitting it through the normal runtime. Provenance records the
configuration SHA-256, exact output-file SHA-256, template pins, expanded task IDs,
and final output field locations. A failed write may leave an incomplete output
file; inspect that failure rather than reusing its path automatically.

This command is part of the complete runtime, which requires Go 1.26.7 or newer
(the module minimum).

## Template

Save this as `answer.json` beside the composition configuration:

```json
{
  "schema": "irrigator.workflow/v1",
  "version": "1",
  "inputs": {"question": "string"},
  "nodes": [{
    "id": "answer",
    "prompt": "Answer the question in the workflow input bindings.",
    "profile": "small",
    "inputs": {"question": {"input": "question"}},
    "outputs": {"answer": "string"}
  }],
  "exports": {"answer": {"node": "answer", "port": "answer"}}
}
```

Each output becomes a required runtime result field. Version 1 preserves opaque
string ports and rejects richer contracts. Version 2 below adds runtime-validated
JSON values while preserving the string-valued worker response envelope.
Within a template, a node input may instead reference another node's output with
`{"node":"source","port":"answer"}`. This adds a dependency and an exact
`input_fields` selector. Template exports expose named node outputs.

## Composition

The configuration uses `schema: "irrigator.composition/v1"`, a nonempty `name`,
an explicit parent `execution` object using the existing managed execution
catalog schema (including `cost_policy: "retain_unknown"`, `allowed_profiles`,
and `run_budget_usd`), and positive `max_tasks`, `max_input_bytes`, and
`max_output_bytes` bounds. It also supplies `calls` and `exports`:

```json
{
  "calls": [
    {
      "id": "draft",
      "template": {"path": "answer.json", "version": "1", "sha256": "EXACT_FILE_SHA256"},
      "inputs": {"question": {"literal": "Explain this design."}}
    },
    {
      "id": "review",
      "template": {"path": "answer.json", "version": "1", "sha256": "EXACT_FILE_SHA256"},
      "inputs": {"question": {"call": "draft", "port": "answer"}}
    }
  ],
  "exports": {"result": {"call": "review", "port": "answer"}}
}
```

This fragment is combined with the required parent fields above. Compute the
SHA-256 over the exact template bytes, including whitespace. Version strings
must match exactly. Reuse invokes the entire subgraph with distinct IDs such as
`draft__answer` and `review__answer`. Inputs must bind every template input
exactly once. Literal strings are serialized as JSON data in the task prompt;
references name exact dependency artifacts. Templates and calls can appear in
any order. Explicit cycles, missing ports, and unknown fields are rejected.
Final exports are locations in the provenance receipt, not additional tasks or
filesystem writes. Identical files produce identical plan bytes and provenance;
no timestamps or random IDs are injected.

## Authority and bounds

Only the parent config carries an execution catalog, allowed profiles, and
budget. Templates select one of those profiles; they cannot introduce providers,
commands, tool grants, credentials, context files, file exports, spawning,
continuations, or helper admission. The existing runtime validates the completed
plan. Each node is a main agent with exactly one turn and zero helpers, and the
output plan's task capacity is tightened to its actual expanded task count.
The parent's task ceiling must be between 1 and 1024. Profile input/output byte
limits must fit the explicit parent ceilings; the compiled prompt also must fit
the selected profile input bound. Runtime accounting and actual payload limits
remain authoritative, including dependency artifacts added at execution time.
Compilation does not reserve money or guarantee that a runtime budget can fund
all tasks.

Each JSON file is limited to 1 MiB, with an aggregate 8 MiB bound on decoded
templates across all calls. Inputs require regular files, UTF-8, strict fields, bounded JSON nesting,
and no duplicate keys or trailing data. Template paths are relative to the
configuration's directory. Absolute paths, traversal, and symlink escapes are
rejected using a confined directory handle; symlinks remaining within that
root are permitted. Names are simple identifiers of at most 64 characters;
`__` is reserved for unambiguous composition namespaces. Templates cannot load
other templates: reusable calls are explicit in the bounded parent config.

## Version 2 typed artifacts

Use `irrigator.composition/v2` with pinned `irrigator.workflow/v2` templates.
Replace template `inputs` with `input_contracts`, and node `outputs` with
`output_contracts`. Every node input also needs an `input_contracts` entry.
A contract is a bounded Irrigator contract, **not JSON Schema**:

```json
{"type":"object","max_bytes":512,"properties":{
  "count":{"type":"integer","max_bytes":16},
  "ready":{"type":"boolean","max_bytes":5},
  "tags":{"type":"array","max_bytes":128,"max_items":2,
          "items":{"type":"string","max_bytes":32}}
}}
```

All object properties are required; additional properties and null are forbidden.
Types are string, boolean, integer, number, object, and array. Each contract has
an explicit positive `max_bytes` (at most 1 MiB). Arrays need `items` and positive
`max_items` (at most 1024). Contracts allow at most eight nested levels, 256 nodes,
and 128 properties per object. Numeric values must be finite float64-range
numbers; integer checking preserves decimal precision and bounds exponents to
±308. Nested values must satisfy their own byte bounds.

The existing `outputs: map[string]string` transport remains unchanged. A typed
report is sent as `{"outputs":{"report":"{\"count\":2,\"ready\":true,\"tags\":[\"tested\"]}"}}`.
Values must be compact JSON with sorted object keys, exactly as Go's JSON
encoder produces; whitespace or unsorted keys are rejected, not silently rewritten.
JSON numeric spelling is preserved. A typed string therefore includes JSON quotes
inside its transport string. Composition literal strings use the same encoding.
Compilation validates literal values and source-to-consumer contracts, including
cross-call exports and declared inputs unused by a template. Producer bounds may
be narrower; object keys must match exactly, and integer may flow into number.
Runtime acceptance checks the complete output before marking a task verified;
only verified dependency artifacts become downstream inputs. Duplicate keys,
wrong types, unknown properties, oversized arrays, nonfinite numbers, malformed
JSON, and null fail acceptance. Typed plans require `swarm/v6`. Parent byte/profile limits and immutable
pins, namespaces, and provenance remain authoritative.
