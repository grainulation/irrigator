# Offline update verification

`irrigator update-check` is a fail-closed verification gate, **not an updater**.
It downloads nothing, installs nothing, changes no configuration and records no
new installed sequence. Signing, an authenticated release channel, atomic
installation and platform signature checks remain separate. This command does not
install CLI archives, provision keys or maintain a signed release channel/sequence policy.

```sh
irrigator update-check \
  --manifest release.json --signature release.sig \
  --artifact irrigator-darwin-arm64.tar.gz --public-key trusted-release.pub \
  --installed-sequence 4
```

The public key is an independently trusted raw 32-byte Ed25519 key. The detached
signature is raw 64-byte Ed25519. Do not accept a public key or installed sequence
supplied by the release manifest, download server or an untrusted caller. The
`--installed-sequence` argument is mandatory, including an explicit `0` for a
trusted first-install policy. This command cannot establish where that trusted
sequence or key came from.

A manifest has exactly these fields, in this order. Sign the UTF-8 bytes produced
by Go `json.Marshal(UpdateManifest)`, without whitespace or a final newline:

```json
{"schema":"irrigator.update/v1","sequence":5,"version":"1.0.1","os":"darwin","arch":"arm64","expires_at":"2026-12-01T00:00:00Z","artifact_sha256":"<64 lowercase hexadecimal characters>","artifact_bytes":123456}
```

The example digest is a placeholder, not an accepted manifest. Unknown, missing,
duplicate and case-aliased fields, nulls, trailing values and noncanonical
encodings are rejected. Verification requires a valid signature, supported
schema, a sequence strictly greater than the trusted installed sequence, exact
current OS/architecture, unexpired UTC RFC3339 timestamp, and a matching artifact
size and SHA-256. Manifests are bounded to 4 KiB and artifacts to 512 MiB.

Inputs must be regular files. The shared pinned-directory reader rejects symlinks
in any path component and nonregular inputs such as FIFOs. The reader is part of
the complete runtime (Go 1.26+, the module minimum).
No filesystem path is an authorization grant. A successful check covers the
bytes read during that check, not a path that could later be replaced: an eventual
installer must consume the same verified bytes or reverify its staged artifact.

The JSON result deliberately includes `installed:false`. A valid cryptographic
signature establishes control of the pinned signing key; it does not establish
Apple signing/notarization, safety of the program, publisher identity beyond that
key, or successful installation. The local prototype package currently carries no
release signing key or authenticated update channel, so automatic updates remain
disabled.

Tests use ephemeral keys generated in memory and free local file fixtures; no
private signing keys or network services are used. Run:

```sh
go test -race -run '^TestUpdate' ./...
```
