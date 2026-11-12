package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"regexp"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

type CampaignGrant struct {
	ID             string `json:"id"`
	RunDirectory   string `json:"run_directory"`
	PlanHash       string `json:"plan_hash"`
	ReservedMicros int64  `json:"reserved_micros"`
	ReportedMicros int64  `json:"reported_micros"`
	Status         string `json:"status"`
}
type CampaignLedger struct {
	Schema        string                   `json:"schema"`
	ID            string                   `json:"id"`
	CeilingMicros int64                    `json:"ceiling_micros"`
	Grants        map[string]CampaignGrant `json:"grants"`
}

var campaignIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ValidateCampaignPlan(p model.Plan) error {
	if p.Campaign == nil {
		return nil
	}
	if PlanTracksCost(p) {
		return errors.New("campaigns enforce shared budgets and require cost_policy retain_unknown")
	}
	c := p.Campaign
	if p.Execution == nil || !launch.SupportsGovernedPlan(p.APIVersion) || !filepath.IsAbs(c.Directory) || filepath.Clean(c.Directory) != c.Directory || !campaignIDPattern.MatchString(c.CampaignID) || !campaignIDPattern.MatchString(c.GrantID) {
		return errors.New("campaign binding requires swarm/v5 and canonical directory/identities")
	}
	return nil
}
func CampaignDecode(raw []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	value, err := jsonio.ParseStrictJSON(d, 0)
	if err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing campaign JSON")
	}
	if err = model.ExactTurnJSONFields(value, reflect.TypeOf(into).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(into)
}
func ValidateCampaignLedger(c CampaignLedger) error {
	if c.Schema != "irrigator.campaign/v1" || !campaignIDPattern.MatchString(c.ID) || c.CeilingMicros < 0 || c.CeilingMicros > 1000000000000 || c.Grants == nil || len(c.Grants) > 1024 {
		return errors.New("invalid campaign ledger")
	}
	var exposure int64
	for id, g := range c.Grants {
		if id != g.ID || !campaignIDPattern.MatchString(id) || !filepath.IsAbs(g.RunDirectory) || filepath.Clean(g.RunDirectory) != g.RunDirectory || len(g.PlanHash) != 64 || g.ReservedMicros < 0 || g.ReportedMicros < 0 || g.ReportedMicros > g.ReservedMicros {
			return errors.New("invalid campaign grant")
		}
		switch g.Status {
		case "reserved":
			if g.ReportedMicros != 0 {
				return errors.New("unsettled campaign grant has reported amount")
			}
			exposure += g.ReservedMicros
		case "closed":
			exposure += g.ReportedMicros
		default:
			return errors.New("invalid campaign grant status")
		}
		if exposure < 0 || exposure > c.CeilingMicros {
			return errors.New("campaign exposure exceeds ceiling")
		}
	}
	return nil
}
