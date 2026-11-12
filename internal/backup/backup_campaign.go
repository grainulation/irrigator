package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"syscall"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func backupCaptureCampaign(root string, s *model.State) (*backupCampaignEvidence, func(), error) {
	noop := func() {}
	if s.Plan.Campaign == nil {
		return nil, noop, nil
	}
	c, err := checkpoint.OpenCampaign(s.Plan.Campaign.Directory)
	if err != nil {
		return nil, noop, err
	}
	fail := func(e error) (*backupCampaignEvidence, func(), error) { c.Close(); return nil, noop, e }
	// Do not let a hardlinked external ledger or lock bypass evidence boundaries.
	for _, p := range []string{"campaign.lock", "campaign.json"} {
		st, e := c.Root.Lstat(p)
		if e != nil {
			return fail(e)
		}
		ss, ok := st.Sys().(*syscall.Stat_t)
		if !st.Mode().IsRegular() || !ok || ss.Nlink != 1 {
			return fail(errors.New("campaign evidence requires single-link regular files"))
		}
	}
	l, err := c.Read()
	if err != nil {
		return fail(err)
	}
	data, err := json.Marshal(l)
	if err != nil {
		return fail(err)
	}
	h := sha256.Sum256(data)
	e := &backupCampaignEvidence{Schema: "irrigator.backup-campaign/v1", SourceRun: root, Directory: s.Plan.Campaign.Directory, Ledger: backupEntry{Path: "campaign.json", Size: int64(len(data)), SHA256: hex.EncodeToString(h[:]), Data: data}}
	if err = validateBackupCampaign(s, e); err != nil {
		return fail(err)
	}
	return e, c.Close, nil
}
