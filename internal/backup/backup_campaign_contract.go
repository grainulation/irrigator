package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

const backupCompatibilityV1 = "checkpoint versions 1..3; offline evidence only; no migration or execution authority"
const backupCompatibilityV2 = "checkpoint versions 1..4; offline evidence only; no migration or execution authority"

type backupCampaignEvidence struct {
	Schema    string      `json:"schema"`
	SourceRun string      `json:"source_run"`
	Directory string      `json:"original_directory"`
	Ledger    backupEntry `json:"ledger"`
}

func validateBackupCampaign(s *model.State, e *backupCampaignEvidence) error {
	if s.Plan.Campaign == nil {
		if e != nil {
			return errors.New("unexpected campaign evidence")
		}
		return nil
	}
	if e == nil {
		return errors.New("campaign evidence missing")
	}
	if err := checkpoint.ValidateCampaignPlan(s.Plan); err != nil {
		return err
	}
	b := s.Plan.Campaign
	if e.Schema != "irrigator.backup-campaign/v1" || e.Directory != b.Directory || !filepath.IsAbs(e.SourceRun) || filepath.Clean(e.SourceRun) != e.SourceRun || e.Ledger.Path != "campaign.json" || e.Ledger.Size < 0 || e.Ledger.Size > 4<<20 || e.Ledger.Size != int64(len(e.Ledger.Data)) {
		return errors.New("invalid campaign evidence metadata")
	}
	h := sha256.Sum256(e.Ledger.Data)
	if hex.EncodeToString(h[:]) != e.Ledger.SHA256 {
		return errors.New("campaign evidence digest mismatch")
	}
	var l checkpoint.CampaignLedger
	if err := checkpoint.CampaignDecode(e.Ledger.Data, &l); err != nil {
		return err
	}
	if err := checkpoint.ValidateCampaignLedger(l); err != nil {
		return err
	}
	g, ok := l.Grants[b.GrantID]
	if !ok || l.ID != b.CampaignID || g.RunDirectory != e.SourceRun || g.PlanHash != s.PlanHash || s.PlanHash != launch.ExecutionDigest(s.Plan) || s.Plan.Execution == nil {
		return errors.New("campaign evidence binding mismatch")
	}
	amount, err := launch.ExecutionBudgetMicros(s.Plan.Execution.RunBudgetUSD)
	if err != nil || g.ReservedMicros != amount {
		return errors.New("campaign evidence allowance mismatch")
	}
	if g.Status == "closed" {
		if s.Execution == nil {
			return errors.New("closed campaign missing execution ledger")
		}
		var reported int64
		for _, r := range s.Execution.Reservations {
			if r.Status != "settled" {
				return errors.New("closed campaign conflicts with retained uncertainty")
			}
			reported += r.ReportedMicros
		}
		if reported != g.ReportedMicros {
			return errors.New("closed campaign reported spend mismatch")
		}
	}
	return nil
}
