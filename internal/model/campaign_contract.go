package model

// A campaign reserves an entire run allowance before creating that run. This
// deliberately favors stranded reservations over granting a second spending
// authority after a crash between two durable stores.
type CampaignBinding struct {
	Directory  string `json:"directory"`
	CampaignID string `json:"campaign_id"`
	GrantID    string `json:"grant_id"`
}
