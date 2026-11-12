// Package textsafe sanitizes untrusted text for terminal presentation.
//
// Terminal output is presentation only: callers must keep the raw bytes for
// durable storage, hashing and copy operations. The output is always valid
// UTF-8 and contains no terminal control, Unicode format (Cf), line/paragraph
// separator, or invalid byte. Everything neutralized is rendered visibly with
// one unambiguous backslash convention instead of being stripped:
//
//	\\            a literal backslash (escaped so no input can forge an escape)
//	\0 \a \b \e \r   NUL, BEL, BS, ESC, CR
//	\xHH          any other C0 control or DEL (HH < 0x80), or an invalid UTF-8
//	              byte (HH >= 0x80); the two ranges never overlap
//	\uHHHH        a C1 control, Unicode Cf format character, U+2028 or U+2029
//	\UHHHHHHHH    the same for code points above U+FFFF
//
// Tab and newline pass through. Every other character, including ordinary
// text containing ^[, <ZWSP> or <0xFF>, is unchanged, so the mapping is
// injective: two different inputs never display the same.
//
// # Allowed dependencies
//
// Unnumbered: it sits outside the numbered layer table and was not changed by the package
// split. Imports no other internal package. Third-party: none.
//
// # Invariants
//
//   - The output is always valid UTF-8, contains no terminal control, Unicode format (Cf), line
//     or paragraph separator or invalid byte, and the mapping is injective.
//   - Callers keep the raw bytes for storage, hashing and copying; sanitized text is for
//     presentation only.
//   - Leaf package: standard library only.
package textsafe
