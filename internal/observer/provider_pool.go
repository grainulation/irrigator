package observer

import (
	"github.com/grainulation/irrigator/internal/model"
)

type ProviderLedger struct {
	Receipts         map[string]model.ProviderReceipt `json:"receipts"`
	AdmissionBlocked bool                             `json:"admission_blocked,omitempty"`
}
