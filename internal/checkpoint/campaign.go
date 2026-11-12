package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func CampaignNonce() (string, error) {
	b := make([]byte, 16)
	_, e := rand.Read(b)
	return hex.EncodeToString(b), e
}
func CampaignCanonical(path string) (string, error) {
	a, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	p, e := filepath.EvalSymlinks(filepath.Dir(a))
	if e != nil {
		return "", e
	}
	return filepath.Join(p, filepath.Base(a)), nil
}

type CampaignStore struct {
	Root *os.Root
	lock *os.File
}

func OpenCampaign(dir string, nonblocking ...bool) (*CampaignStore, error) {
	dir, e := CampaignCanonical(dir)
	if e != nil {
		return nil, e
	}
	r, e := BackupOpenRoot(dir)
	if e != nil {
		return nil, e
	}
	f, e := r.OpenFile("campaign.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		r.Close()
		return nil, e
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		r.Close()
		return nil, errors.New("campaign lock must be regular")
	}
	mode := syscall.LOCK_EX
	if len(nonblocking) > 0 && nonblocking[0] {
		mode |= syscall.LOCK_NB
	}
	if e = syscall.Flock(int(f.Fd()), mode); e != nil {
		f.Close()
		r.Close()
		return nil, e
	}
	return &CampaignStore{r, f}, nil
}
func (c *CampaignStore) Close() { Unlock(c.lock); c.Root.Close() }
func (c *CampaignStore) Read() (CampaignLedger, error) {
	var l CampaignLedger
	f, e := c.Root.OpenFile("campaign.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return l, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return l, errors.New("invalid campaign file")
	}
	data, e := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if e != nil {
		return l, e
	}
	if len(data) > 4<<20 {
		return l, errors.New("campaign file too large")
	}
	if e = CampaignDecode(data, &l); e != nil {
		return l, e
	}
	return l, ValidateCampaignLedger(l)
}
func (c *CampaignStore) Write(l CampaignLedger) error {
	if e := ValidateCampaignLedger(l); e != nil {
		return e
	}
	data, e := json.MarshalIndent(l, "", "  ")
	if e != nil {
		return e
	}
	id, e := CampaignNonce()
	if e != nil {
		return e
	}
	name := ".campaign-" + id
	f, e := c.Root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer c.Root.Remove(name)
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = c.Root.Rename(name, "campaign.json"); e != nil {
		return e
	}
	d, e := c.Root.Open(".")
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

func ValidateCampaignAuthority(root string, p model.Plan, nonblocking ...bool) error {
	if e := ValidateCampaignPlan(p); e != nil {
		return e
	}
	if p.Campaign == nil {
		return nil
	}
	b := p.Campaign
	c, e := OpenCampaign(b.Directory, nonblocking...)
	if e != nil {
		return e
	}
	defer c.Close()
	l, e := c.Read()
	if e != nil {
		return e
	}
	path, e := CampaignCanonical(root)
	if e != nil {
		return e
	}
	g, ok := l.Grants[b.GrantID]
	if !ok || l.ID != b.CampaignID || g.Status != "reserved" || g.RunDirectory != path || g.PlanHash != launch.ExecutionDigest(p) {
		return errors.New("campaign authority closed, missing or belongs to another run")
	}
	amount, e := launch.ExecutionBudgetMicros(p.Execution.RunBudgetUSD)
	if e != nil || g.ReservedMicros != amount {
		return errors.New("campaign run ceiling changed")
	}
	return nil
}
