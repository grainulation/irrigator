// Package selfupdate verifies signed update manifests.
//
// # Responsibility
//
// Update-manifest signature verification and `irrigator update check`. Signatures cover exactly
// json.Marshal(UpdateManifest) without a trailing newline.
//
// # Allowed dependencies
//
// Layer 27. May import only lower layers; today it imports commandflags, jsonio, launch.
// Third-party: none. The compiler rejects import cycles; the layer order is recorded in
// docs/architecture.md.
//
// # Invariants
//
//   - Offline validation only; no install occurs.
//   - The trusted key and the installed sequence come from trusted release configuration, never
//     from the downloaded manifest.
//   - tools/release-sign mirrors this manifest; change both together (go test
//     ./tools/release-sign).
package selfupdate
