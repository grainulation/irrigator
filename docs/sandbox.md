# Container sandbox for command providers

A command provider can optionally run inside a container instead of directly on
the host. Add a `sandbox` object to the provider in the plan; providers without
one behave exactly as before.

```json
"providers": {
  "boxed": {
    "command": ["python3", "worker.py"],
    "sandbox": {"image": "python:3.13-alpine"}
  }
}
```

## What it isolates, and what it does not

The sandbox is a **convenience boundary** for filesystem and network reach. It is
not a hardened security boundary.

- Isolated: the host filesystem outside the cell directory and the mounts you list;
  the network (default `none`); the host process table.
- Not isolated: whatever the image, mounts and network setting let the agent
  reach. A writable mount is writable; `network: "default"` is full network
  access; an environment variable you pass in is visible to the agent.
- Agents that call an API still need `network: "default"` and credentials passed
  through `env`. Without them they cannot reach the API.
- Container escapes, a malicious image, or a compromised runtime are out of scope.
  Do not rely on this to run untrusted code against secrets.

## Fields

| Field | Default | Meaning |
| --- | --- | --- |
| `runtime` | `auto` | `auto`, `container` (Apple), `docker` or `podman`. `auto` picks the first on `PATH` of `container` (macOS only), `podman`, `docker`. |
| `image` | required | Image reference. |
| `network` | `none` | `none` or `default`. |
| `mounts` | none | List of `{source, target, read_only}`. `source` is absolute or relative to the plan file (resolved when the plan is loaded). `target` is absolute. `read_only` defaults to `true`. |
| `workdir` | `/work` | Where the cell directory is mounted (writable) and the working directory. |
| `read_only_root` | `false` | Adds `--read-only` to the container root filesystem. |
| `env` | none | Variable names to pass. Passed as `-e NAME`, so the runtime reads the value from its own environment and the value never appears in argv. |

Invalid settings fail at plan load and `validate` with a message naming the
provider and field.

## Execution

Irrigator runs:

```
<runtime> run --rm -i --name irrigator-<run8>-<cell>-<attempt>-<rand> \
  --network <none|default> -v <cell dir>:<workdir> -w <workdir> \
  [-v <source>:<target>[:ro]]... [-e NAME]... [--read-only] IMAGE <command...>
```

The request goes to stdin and the response comes from stdout under the usual
[worker protocol](provider-protocol.md) and limits. The host-side environment
allowlist (`PATH`, `HOME`, `TMPDIR` plus the provider's `env`) applies to the
runtime client; names in `sandbox.env` are added so the client can forward them.

If no runtime is available the attempt fails as `not_started`. On timeout or
cancellation Irrigator also runs `<runtime> kill <name>`, because stopping the
client does not always stop the container, and `--rm` removes it afterwards. An
interrupted provider call stays **uncertain**; it is never retried automatically.

## Examples

Apple `container` (macOS 26+):

```json
"sandbox": {"runtime": "container", "image": "python:3.13-alpine"}
```

Docker with a read-only data mount:

```json
"sandbox": {"runtime": "docker", "image": "python:3.13-alpine",
            "mounts": [{"source": "data", "target": "/data"}]}
```

Podman with a read-only root and an API credential:

```json
"sandbox": {"runtime": "podman", "image": "python:3.13-alpine",
            "network": "default", "read_only_root": true, "env": ["API_KEY"]}
```

### Worked example: a tiny echo worker

`worker.py` next to the plan reads one request and answers it:

```python
import json, sys
req = json.load(sys.stdin)
print(json.dumps({"protocol": "swarm.worker/v1", "request_id": req["request_id"],
                  "output": {"answer": "hello from the sandbox"}}))
```

`plan.json`:

```json
{
  "name": "sandbox-echo",
  "providers": {
    "boxed": {
      "command": ["python3", "/work/worker.py"],
      "sandbox": {"image": "python:3.13-alpine"}
    }
  },
  "tasks": [{"id": "echo", "provider": "boxed", "prompt": "say hello", "requires": ["answer"]}]
}
```

The cell directory is mounted at `/work`, so place `worker.py` where the provider
can see it, or mount its directory read-only:
`"mounts": [{"source": ".", "target": "/app"}]` with command
`["python3", "/app/worker.py"]`. The same approach runs
`examples/monitoring/worker.py`.

Relative mount sources are made absolute against the plan file's directory when
the plan is loaded, so the admitted plan records the host path.
