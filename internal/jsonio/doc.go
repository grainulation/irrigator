// Package jsonio is strict JSON decoding and atomic JSON file output.
//
// # Responsibility
//
// ParseStrictJSON reads one value while rejecting duplicate object keys and nesting beyond a
// fixed depth (numbers stay json.Number). AtomicJSON writes a document by temporary file,
// fsync, rename and directory fsync. PrintJSON writes indented JSON to standard output. The
// package owns no schema and no file layout.
//
// # Allowed dependencies
//
// Layer 3. Imports no other internal package. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - AtomicJSON publishes a complete document or leaves the previous file untouched: a reader
//     sees the old or the new content, never a partial write.
//   - ParseStrictJSON never accepts a duplicate key at any depth and never loses number
//     precision.
//   - Leaf package: it imports only the standard library.
package jsonio
