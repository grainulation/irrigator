package model

import (
	"encoding/json"

	"github.com/grainulation/irrigator/internal/cost"
)

type ProviderReceipt struct {
	Conversation *ProviderConversation `json:"conversation,omitempty"`
	Hash         string                `json:"hash"`
	Status       string                `json:"status"`
	Reserved     float64               `json:"reserved_usd"`
	// ReservedMicros is the exact reservation; ledgers written before it existed
	// derive it from Reserved (see reservedAmount).
	ReservedMicros int64        `json:"reserved_micros,omitempty"`
	Response       WorkResponse `json:"response"`
	// CostUnknown durably records that the response's billing is unknown (an invalid
	// cost was reported). The held reservation stays in Reserved/ReservedMicros as
	// accounting only; it is never a reported charge. WorkResponse.CostUnknown is not
	// a wire field, so this flag is what survives a ledger reload. Absent in ledgers
	// written before it existed, which keep their meaning unchanged.
	CostUnknown bool                 `json:"cost_unknown,omitempty"`
	Attribution *ProviderAttribution `json:"attribution,omitempty"`
}

// UnmarshalJSON restores the in-memory unknown-cost marker from the durable flag so
// a reloaded receipt replays as unknown, never as a known zero invoice.
func (r *ProviderReceipt) UnmarshalJSON(data []byte) error {
	type plain ProviderReceipt // no methods: avoids recursion
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = ProviderReceipt(decoded)
	if r.CostUnknown {
		r.Response.CostUnknown = true
	}
	return nil
}

// reservedAmount is the receipt's reserved or settled amount in micros.
func (r ProviderReceipt) ReservedAmount() int64 {
	if r.ReservedMicros != 0 {
		return r.ReservedMicros
	}
	m, _ := cost.MicrosFromFloat(r.Reserved)
	return m
}

// setReserved keeps the float and exact fields together.
func (r *ProviderReceipt) SetReserved(m int64) {
	r.ReservedMicros, r.Reserved = m, cost.MicrosToFloat(m)
}
