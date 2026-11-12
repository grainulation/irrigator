// Package campaign admits and closes campaigns.
//
// # Responsibility
//
// Campaign admission and close (`irrigator campaign`). A campaign reserves an entire run
// allowance before creating a run. The grant and ledger validators live in checkpoint, which
// must see them on every write.
//
// # Allowed dependencies
//
// Layer 14. May import only lower layers; today it imports checkpoint, commandflags, jsonio,
// launch, model. Third-party: none. The compiler rejects import cycles; the layer order is
// recorded in docs/architecture.md.
//
// # Invariants
//
//   - Campaign state is read and written through internal/checkpoint (OpenCampaign,
//     CampaignCanonical, grants and ledger); this package adds the init, admit and close
//     operations and the `irrigator campaign` CLI.
//   - The grant and ledger validators and the guards that gate other writes stay in
//     internal/checkpoint (a lower layer), not here.
package campaign
