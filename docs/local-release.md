# Local release packaging

`tools/package-plugin.py` builds a reproducible local archive of the Irrigator plugin and skill bundle, and audits archives it built. Everything stays on this machine. No push, no publish step and no host install exists in this tool, and it does not select or alter licensing.

For the Go CLI archives (macOS and Linux builds signed in the update format), see the [release pipeline](release-pipeline.md).

## Scope

- Local only. The tool opens no network connection, runs no version-control command and never writes into a host plugin directory.
- It packages manifests, Markdown skills, docs and the verification scripts. It does not build or bundle the Go runtime. Build that yourself with `make build`.
- It creates no license file. A `LICENSE*` or `NOTICE*` file is included only if one already exists in the tree. The project includes its [MIT license](../LICENSE).
- The archive is not an installer. It makes no claim about a public listing, a marketplace entry or host endorsement.
- The packager runs no MCP server. Only `.claude-plugin/plugin.json` may register one, and only the read-only `irrigator mcp` server (see [MCP](mcp.md)).

## What is packaged

Only an allowlist is packaged. Everything else is ignored.

| Entry | Notes |
| --- | --- |
| `.claude-plugin/`, `.codex-plugin/`, `skills/`, `docs/` | Text files only (`.md`, `.json`, `.py`, `.txt`, `.sh`, `.toml`, `.yaml`, `.yml`). |
| `plugin.json`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CHANGELOG.md`, `CODE_OF_CONDUCT.md` | Root files. |
| `verification/plugin-contract-test.py`, `verification/plugin-validator-regression.py`, `verification/plugin-consumer-test.py`, `tools/package-plugin.py`, `tools/test_parity_package.py`, `tools/host-plugin-probe.py`, `tools/host-agent-events.py`, `verification/host-agent-events-test.py` | Fixed list, so the archive can check itself and consumer docs can point at these scripts. |
| `verification/cli-contract.json`, `docs/plan.schema.json` | Generated command and editor contracts. |
| `LICENSE*`, `NOTICE*` | Only if already present. |

Every directory under `skills/` is discovered. All nine skills are required (`irrigator`, `router`, `plan`, `run`, `resume`, `observe`, `budget`, `swarm`, `debug`); each is validated (frontmatter, name equals directory). `docs/plugin-packaging.md` is part of the `docs/` directory and therefore ships in the consumer archive; see it for first use and for `--binary`.

Excluded: `.git`, `dist`, `reviews`, `__pycache__`, `node_modules`, `.venv`, `.DS_Store`, compiled files and any file over 5 MiB. A native executable or NUL-bearing file in the package is an error.

Refused with an error, never silently skipped: a symlink, or any file reached through a symlinked parent directory (this applies to the fixed root files and to `verification/` and `tools/` too), and a file named like a secret store: `.env*`, `credentials*` (for example `credentials.json`, `.credentials`) or `settings.local*`. A `.env` file at the tree root is outside the allowlist, is not packaged and is not inspected.

## Commands

Run these commands from the source root; from another directory, use the script’s absolute path. `-I` stops Python from importing modules planted next to the script.

```sh
python3 -I tools/package-plugin.py --check
python3 -I tools/package-plugin.py --list
python3 -I tools/package-plugin.py --out /tmp/irrigator-dist
python3 -I tools/package-plugin.py --out /tmp/irrigator-dist-tgz --format tar.gz
python3 -I tools/package-plugin.py --verify /tmp/irrigator-dist/ARCHIVE
```

| Option | Meaning |
| --- | --- |
| `--root DIR` | Plugin tree. Default is the parent of `tools/`. |
| `--out DIR` | Output directory. Default is `ROOT/dist`. It must not sit inside a packaged directory or be a symlink. |
| `--format zip` or `tar.gz` | Archive format. Default `zip`. |
| `--version V` | Version label. It must equal the manifest version. |
| `--check` / `--list` | Validate only, or also print the file list. Nothing is written. |
| `--verify ARCHIVE` | Audit an existing archive. Reads the archive and uses a private temporary directory only. |
| `--require-sidecars` | With `--verify` only. A missing `SHA256SUMS` or `release-manifest.json` becomes an error. Without it a missing sidecar stays a warning. |
| `--force` | Replace existing final artifacts in `--out`. Without it the tool refuses. It never reuses a temporary `.part` path. |
| `--strict` | Warnings fail too. Review and resolve any warnings before a strict build. |
| `--binary ABS_PATH` | Record the name, SHA-256 and size of a runtime binary you built, plus a hash of the Go sources. The binary is never executed, bundled or located by directory. Not allowed with `--verify`. |

## Output

Three files are written to the output directory. Each is first created exclusively under a `.part` name (no symlink is followed and nothing is overwritten) and then renamed. If any `.part` path already exists, even as a symlink or a stale file, the tool exits with status 2, removes only the `.part` files it created itself and leaves the existing path alone, with or without `--force`:

- `irrigator-plugin-VERSION.zip` or `.tar.gz`, with one top-level directory of the same name.
- `release-manifest.json`: per-file SHA-256 and size, the archive digest, the format and the host manifest flags.
- `SHA256SUMS`: digests of the archive and of `release-manifest.json`.

The release manifest states the scope. Abbreviated example:

```json
{
  "name": "irrigator",
  "scope": "local-only; not published, not installed",
  "host_install_tested": false,
  "runtime_binary_bundled": false,
  "mcp_server": null,
  "license_file_included": true,
  "files": [{"path": "skills/plan/SKILL.md", "sha256": "...", "bytes": 0}]
}
```

After writing, the tool reads the new archive back and compares every member with the bytes it hashed. A mismatch removes the archive and exits with status 2.

## Validation before packaging

- Manifests: valid JSON, `name` is `irrigator`, a semver `version` that agrees across `plugin.json`, `.codex-plugin/plugin.json`, `.claude-plugin/plugin.json` and the marketplace entry.
- No `permissions`, pricing or `userConfig` keys, and no `mcpServers` key except the read-only `irrigator mcp` registration (no `--allow-control`) in `.claude-plugin/plugin.json`. No license key unless a license file exists. Homepage and repository style fields produce a warning.
- Every `./` path in a manifest exists, stays inside the plugin root and does not pass through a symlink.
- Manifest fields that name a location (keys ending in path, file, dir, icon, logo, image, screenshot, skill, agent, hook, entry, source, binary or executable) must not hold an absolute, `~`, `$VAR`, drive-letter, backslash, `file:` or `..` path. Descriptions, prompts and public `https://` URLs are not treated as paths.
- All nine skills exist: `irrigator`, `router`, `plan`, `run`, `resume`, `observe`, `budget`, `swarm`, `debug`. Each has frontmatter whose `name` equals its directory name.
- Content scan: private key blocks and a few common token shapes are errors. Machine-specific absolute paths are warnings. See the next section for what this scan cannot do.

## Host manifest compatibility

Irrigator needs the local `irrigator` executable, so the Codex manifests should say so. The [official local-execution plugin guidance](https://developers.openai.com/plugins/deploy/desktop-only) describes two placements:

- Portable root `plugin.json`: `extensions.com.openai.requires_local_executor` set to `true`.
- Legacy `.codex-plugin/plugin.json`: a top-level `requires_local_executor` set to `true`.

The two files are therefore not byte-identical copies. The root file carries the portable schema and keeps OpenAI presentation under `extensions.com.openai`. The legacy file keeps its flat layout and `skills` of `./skills/`. Resources resolve from the plugin root, not from `.codex-plugin/`.

The packager warns when either flag is missing, and warns when the two files are byte-identical. It does not rewrite manifests. Local checks do not establish eligibility for any public directory, and do not establish model invocation. Use [host integration](host-integration.md) to check your installed host.

## Secret handling and scan limits

- Known secret filenames (`.env*`, `credentials*`, `settings.local*`) are rejected by name, whatever they contain.
- The content scan is a coarse heuristic. It looks only for PEM private key headers and the shapes of AWS access key ids, GitHub tokens, `sk-` style keys and Slack tokens. A clean result does not prove the package holds no secrets: passwords, JWTs, other vendors' tokens, split, encoded or obfuscated values and secrets in files that are not scanned all pass.
- A hit is reported by file and kind only; the matched text is never printed.
- Review `--list` output and file contents yourself before sharing an archive. Treat anything that may have leaked as compromised and rotate it.

## Archive safety checks

The build refuses, and `--verify` reports, each of the following:

- Absolute paths, backslashes, drive letters, control characters, empty, `.` or `..` components.
- Directories, symlinks, hard links, devices and encrypted members.
- Duplicate names and names that collide after case folding or Unicode normalization (NFKC), invisible formatting or bidirectional control characters in names, a name that is both a file and a directory prefix, more than 500 members, files over 5 MiB, more than 25 MiB in total.
- More than one top-level directory, or a directory name that does not match `irrigator-plugin-` plus the manifest version.
- Members outside the allowlist, secret filenames, content the heuristic scan flags, native executables, invalid manifests or skills.
- A `SHA256SUMS` or `release-manifest.json` next to the archive that disagrees with it, is a symlink or a non-regular file, is malformed or lists a path twice. Sidecar content is never copied into messages; only counts and line numbers are. If a sidecar is absent, `--verify` warns, and `--require-sidecars` turns that into an error.

`--verify` never extracts into the working tree. It reads members into memory and writes only to a temporary directory that is deleted afterwards.

## Package safety tests

`python3 -I verification/package-safety-test.py` builds every fixture in a temporary directory. It covers symlinked parents, planted `.part` links whose targets must stay unchanged, sidecar symlinks, archive path collisions, secret filenames, manifest path fields and a deterministic build followed by verify.

`python3 -I tools/test_parity_package.py` covers the nine-skill set, paths with spaces, `--binary` identity, source-tree digest cross-checks, deterministic output, tampered sidecars, hostile archives and that the consumer archive carries `docs/plugin-packaging.md` and the parity test itself. This page records how to run it, not that it passed.

## Reproducibility

Members are sorted, owned by no user, and given mode `0644`. Timestamps come from `SOURCE_DATE_EPOCH`, clamped to the zip range, otherwise 1980-01-01. To check that two builds match, build twice into different directories with the same `SOURCE_DATE_EPOCH` and compare both `SHA256SUMS` files.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success. |
| 1 | Validation or audit failed. |
| 2 | Usage or I/O error, or a refusal to overwrite. |

## Using the archive

The archive is an inspection and transfer artifact. See [host integration](host-integration.md) for isolated native host discovery and session-scoped loading, or use the [manual Codex workflow](codex-plugin.md#manual-workflow) without installing anything.

## What this does not establish

Discovery is not model invocation. A host can list a skill without executing it.

- Digests establish integrity, not publisher authenticity; this packager does not sign artifacts.
- Secret scans are heuristics, not proof that every sensitive value was found.
- An archive audit does not establish runtime execution, host installation, model invocation, or any viewer compatibility.
- Run the documented checks against the artifact you intend to use; results from another build do not transfer automatically.

The source-only plugin archive is narrower than a source checkout: Go files and
runtime assets are not bundled. Use the
[repository](https://github.com/grainulation/irrigator) for source-linked runtime
documentation.
