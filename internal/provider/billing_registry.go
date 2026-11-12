package provider

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func billingRegistryDirectory() (string, error) {
	dir := os.Getenv("IRRIGATOR_BILLING_REGISTRY")
	if dir == "" {
		base, e := os.UserConfigDir()
		if e != nil {
			return "", e
		}
		dir = filepath.Join(base, "Irrigator", "billing")
	}
	var err error
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	return filepath.EvalSymlinks(dir)
}

func ReserveBillingAllocation(s *model.State, r model.BillingReceipt) error {
	dir := s.Execution.BillingRegistry
	if dir == "" {
		var e error
		dir, e = billingRegistryDirectory()
		if e != nil {
			return e
		}
	}
	c, e := checkpoint.OpenCampaign(dir)
	if e != nil {
		return e
	}
	defer c.Close()
	name := checkpoint.BillingAllocationName(r)
	want := []byte(launch.ExecutionDigest(r))
	f, e := c.Root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e == nil {
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("invalid billing allocation")
		}
		got, err := io.ReadAll(io.LimitReader(f, 65))
		if err != nil || !bytes.Equal(got, want) {
			return errors.New("external receipt already allocated to a different operation")
		}
		s.Execution.BillingRegistry = dir
		return nil
	}
	if !os.IsNotExist(e) {
		return e
	}
	id, e := checkpoint.CampaignNonce()
	if e != nil {
		return e
	}
	temp := ".billing-" + id
	f, e = c.Root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer c.Root.Remove(temp)
	if _, e = f.Write(want); e != nil {
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
	if e = c.Root.Rename(temp, name); e != nil {
		return e
	}
	d, e := c.Root.Open(".")
	if e != nil {
		return e
	}
	defer d.Close()
	if e = d.Sync(); e != nil {
		return e
	}
	s.Execution.BillingRegistry = dir
	return nil
}
