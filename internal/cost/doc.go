// Package cost is integer micro-dollar accounting and budget arithmetic.
//
// # Responsibility
//
// All monetary quantities are int64 micro-dollars (1 USD = 1,000,000 micros). Floats exist only
// at external JSON boundaries. Conversions reject NaN, Inf, negative amounts and amounts above
// $1,000,000,000 (1e15 micros). Money arithmetic that other packages need (parse, format, add,
// compare against a budget, resolve cost_micros against cost_usd) lives here so no package
// re-implements it.
//
// # Allowed dependencies
//
// Layer 1. Imports no other internal package. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Amounts are never negative and never exceed MaxUSD; every conversion that could violate
//     that returns an error or a false ok.
//   - Add does not wrap: overflow is reported, not silently truncated.
//   - No I/O and no dependency on any other internal package.
package cost
