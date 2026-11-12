// Package localfs is confined local-filesystem helpers.
//
// # Responsibility
//
// Root pins every directory component and refuses ancestor symlinks before a write;
// OpenContextFile, PrivateDir, Abs and RelativePath validate and resolve paths; WriteNew
// creates a file exclusively (O_EXCL, mode 0600) and syncs it. Higher packages use these
// instead of calling os.OpenFile on caller-supplied paths.
//
// # Allowed dependencies
//
// Layer 4. Imports no other internal package. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Paths are validated before use: RelativePath accepts only clean, relative, valid-UTF-8
//     paths without control characters or "..".
//   - WriteNew never overwrites an existing file.
//   - Directory resolution never follows a symlink in an ancestor of the target.
//   - Leaf package: it imports only the standard library (including syscall).
package localfs
