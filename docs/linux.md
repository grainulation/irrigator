# Linux: terminal dashboard and skills

Linux users can run and observe swarms directly in their terminal; macOS uses
the same interface. Windows is not currently supported;
deferred/planned. See [platform support](platform-support.md) for exact tested architectures
and the distinction between builds, execution checks and graphical acceptance.

## Build and try it

Install Go 1.26 or newer and use a POSIX shell. The release checks pin Go 1.27.2.
From the directory containing `go.mod`:

```sh
make build
./irrigator doctor
./irrigator demo --run ./runs/first-demo
```

Choose a fresh run directory if `runs/first-demo` already exists. The terminal
dashboard appears automatically on an interactive terminal.
The demo uses mock workers and does not need model credentials. Its outputs show
execution semantics, not evidence of real model inference.

Install into a user-owned prefix only if desired:

```sh
make install PREFIX="$HOME/.local"
```

Add `$HOME/.local/bin` to your shell's PATH. No system service, global agent-host
configuration, model download or automatic startup is installed by these commands.
For verification, `make smoke`, `make demo`, and `make check` exercise first use,
revision/recovery behavior, the race suite and static analysis.


## Terminal controls

The dashboard shows stable slots for nodes, state counts, reported cost, and
priority details for errors and active work. Running nodes pulse; produced output
and uncertain outcomes have distinct static markers. Large graphs show the number
of slots omitted from the compact view.

```sh
./irrigator status ./runs/first-demo --watch
IRRIGATOR_UI=static ./irrigator demo --run ./runs/static-demo
IRRIGATOR_UI=plain ./irrigator demo --run ./runs/plain-demo
NO_COLOR=1 ./irrigator demo --run ./runs/no-color-demo
```

`static` keeps the dashboard updating without animation. `plain` uses normal
logs. Redirected output and `TERM=dumb` also use plain logs. Ctrl-C while watching
stops the observer; Ctrl-C during execution requests a safe pause.

## Use from an agent host

The skills in [skills/](../skills) instruct a compatible host to run the CLI.
Installing skills does not install the executable or grant execution permission.
Use the host's supported plugin mechanism and verify that it can execute
`irrigator version` and `irrigator doctor` in its actual environment. Host-specific
installation/inventory evidence is separate from Linux runtime acceptance; see
[host integration](host-integration.md).

Real providers additionally need their configured CLI or local server, model,
and authentication. Do not copy another machine's credential files into this
repository or a container image. The Linux verification uses mock providers and
owned local transport fixtures; it does not establish a paid account login on
Linux. See [execution profiles](execution-profiles.md) and
[local Ollama](managed-ollama.md).

## Storage and release boundaries

Keep run directories on a filesystem that provides the locking, atomic replacement
and permission semantics used by the runtime. A Linux process accessing an unusual
network or host-shared filesystem is a separate storage configuration to validate.
The recorded container checks use Linux writable temporary storage, not a writable
mount of your existing runs.

Use the terminal dashboard and skills.
