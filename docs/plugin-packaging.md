# Plugin packaging and first use

This is the current first-use guide for the local, source-only Irrigator archive. It adds to `docs/local-release.md` (the full option and safety reference); where the two differ, the packager in `tools/package-plugin.py` is authoritative. Nothing here publishes, installs or calls a paid model.

## What the archive is

- A deterministic zip or tar.gz of manifests, Markdown skills, docs and the allowlisted verification scripts, plus `SHA256SUMS` and `release-manifest.json` next to it.
- **Source-only.** It bundles no Go runtime and no native executable. `release-manifest.json` says `runtime_binary_bundled: false`.
- All nine skills are required before a package is built: `irrigator`, `router`, `plan`, `run`, `resume`, `observe`, `budget`, `swarm` and `debug`.
- The `irrigator` CLI is the only runtime. Skills call its real commands (`plan`, `apply`, `resume`, `deliver`, `status` and so on). No separate managed launcher or auto-installer exists, and none is documented here.
- The checkout includes the [MIT license](../LICENSE); existing `LICENSE*` and `NOTICE*` files are included by the packager.

## With a local CLI build

The plugin archive and the CLI are separate artifacts. Extract a verified plugin
ZIP into a new directory and put your own built or reviewed `irrigator` on `PATH`.
Verify the supplied checksums before use; checksums establish integrity, not
publisher authenticity.

```sh
export PATH="/absolute/path/to/bin:$PATH"   # directory holding your irrigator binary
irrigator version
irrigator doctor
claude --plugin-dir /absolute/path/irrigator-plugin-VERSION
```

This PATH change applies only to the current shell. It does not install a global
binary. In Claude, invoke `/irrigator:debug /absolute/path/to/run` to diagnose an
existing run. Runtime diagnosis itself is free; a model interpreting the skill
uses the host account. New paid work needs a concrete authorized budget.

If supported by your installed Codex version, add the extracted plugin directory as a local marketplace using the
installed CLI's `codex plugin marketplace add PATH`, then
`codex plugin add irrigator@irrigator-local`; these commands intentionally change
that host's plugin configuration. For a configuration-isolated verification
instead, run `python3 -I tools/host-plugin-probe.py --root EXTRACTED_ROOT --out NEW_EVIDENCE_DIR`.

Installing skills does not start a swarm or install the CLI.

## First use, step by step

Use any directories you like. Quote paths that contain spaces; the examples use single quotes.

1. **Build the binary yourself** from the source you are shipping:

   ```sh
   package_runtime_dir=$(mktemp -d "${TMPDIR:-/tmp}/irrigator-runtime.XXXXXX")
   go build -trimpath -o "$package_runtime_dir/irrigator" .
   shasum -a 256 "$package_runtime_dir/irrigator"
   ```

   Keep this shell variable for the following steps. The executable is deliberately outside the plugin tree, as required by `--binary`. Note its digest and rebuild after any source change.

2. **Package, recording the binary identity** (optional but recommended):

   ```sh
   python3 -I tools/package-plugin.py --out '/tmp/irrigator dist' --binary "$package_runtime_dir/irrigator"
   ```

   `--binary` needs an absolute path to a regular, executable, non-symlink file outside the plugin tree. The packager hashes it and records name, SHA-256 and size. It never runs it, never copies it into the archive and never records its directory.

3. **Verify the archive**, still without extracting into your tree:

   ```sh
   python3 -I tools/package-plugin.py --verify '/tmp/irrigator dist/irrigator-plugin-VERSION.zip' --require-sidecars
   ```

4. **Extract into a new, empty directory** (never into a tree you care about) and run the consumer check, passing the binary explicitly:

   ```sh
   mkdir '/tmp/irrigator consumer'
   unzip -q '/tmp/irrigator dist/irrigator-plugin-VERSION.zip' -d '/tmp/irrigator consumer'
   python3 -I '/tmp/irrigator consumer/irrigator-plugin-VERSION/verification/plugin-consumer-test.py' \
     --root '/tmp/irrigator consumer/irrigator-plugin-VERSION' --binary "$package_runtime_dir/irrigator"
   ```

   Without `--binary` the CLI smoke is skipped, not passed.

5. **Compare identities.** The binary digest from step 1 must equal `runtime_binary.sha256` in `release-manifest.json`. A mismatch means the consumer would run a different binary than the one you recorded.

## Source identity fields

| Field | Meaning | Does not mean |
| --- | --- | --- |
| `source_tree_sha256` | Hash over the sorted digest and path lines of every archived file. `--verify` recomputes it and fails on a mismatch. | Authenticity: the sidecars are unsigned. |
| `go_source_sha256` | Hash of every `.go`, `go.mod` and `go.sum` file under the source root (only with `--binary`). | That the binary was built from those files. |
| `runtime_binary` | Name, SHA-256, size, `bundled: false`, `executed_by_packager: false`, `built_from_go_source_verified: false`. | That the binary works or is trusted. |

A manifest from an older packager lacks these fields: `--verify` warns, and `--require-sidecars` makes a missing `source_tree_sha256` an error.

## Portability

- Archive members use forward slashes, no absolute paths and a single top-level directory, so extraction works on macOS and Linux regardless of where the consumer chooses to extract.
- Shipped instructions use no developer-home paths. Spaces in the root, output or binary path are supported when quoted.
- Names that collide after case folding or Unicode normalization, traversal names, links, devices, duplicate members and anything outside the allowlist are refused by build and `--verify`.

## Tests

From a checkout, `python3 -I tools/test_parity_package.py` builds fixtures in a temporary directory and checks the skill set, spaces in paths, binary identity, deterministic output, tamper detection and hostile archives. It uses a fake binary and starts no model. This file records how to run the tests, not that they have passed.

## Limits

- The packager itself does not load a host. Run the included `tools/host-plugin-probe.py` against the extracted archive for isolated host installation/discovery; see [host integration](host-integration.md). This does not prove model skill invocation.
- Checksums give integrity, not authenticity. The content scan is a heuristic.
- Mock workflows demonstrate orchestration, not model quality. Paid runs need an explicit aggregate budget stated by the user.
