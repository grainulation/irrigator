package messaging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func approvalFixture(t *testing.T) (string, string) {
	t.Helper()
	campaign, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(campaign, "channels", "updates")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	it := ChannelItem{ID: "run-task", Channel: "updates", Text: "verified", SHA256: NoteDigest("verified"), Status: "pending"}
	data, _ := json.Marshal(it)
	if err := os.WriteFile(filepath.Join(dir, it.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return campaign, dir
}

func TestChannelApprovalRejectsSymlinks(t *testing.T) {
	for _, target := range []string{"item", "lock", "ancestor"} {
		t.Run(target, func(t *testing.T) {
			campaign, dir := approvalFixture(t)
			outside := filepath.Join(campaign, "outside")
			if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch target {
			case "item":
				os.Remove(filepath.Join(dir, "run-task.json"))
				if err := os.Symlink(outside, filepath.Join(dir, "run-task.json")); err != nil {
					t.Fatal(err)
				}
			case "lock":
				if err := os.Symlink(outside, filepath.Join(dir, "run-task.lock")); err != nil {
					t.Fatal(err)
				}
			case "ancestor":
				real := dir + "-real"
				if err := os.Rename(dir, real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := ApproveChannelItem(campaign, "updates", "run-task"); err == nil {
				t.Fatal("accepted symlink")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("outside changed: %q %v", data, err)
			}
		})
	}
}

func TestChannelApprovalIgnoresLegacyTemporarySymlink(t *testing.T) {
	campaign, dir := approvalFixture(t)
	outside := filepath.Join(campaign, "outside")
	os.WriteFile(outside, []byte("untouched"), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "run-task.json.approve")); err != nil {
		t.Fatal(err)
	}
	if err := ApproveChannelItem(campaign, "updates", "run-task"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "untouched" {
		t.Fatal("temporary symlink target overwritten")
	}
}

func TestChannelApprovalSerializesAndPreservesFirstApproval(t *testing.T) {
	campaign, dir := approvalFixture(t)
	lock, err := os.OpenFile(filepath.Join(dir, "run-task.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ApproveChannelItem(campaign, "updates", "run-task") }()
	select {
	case err := <-done:
		t.Fatalf("approval bypassed lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "run-task.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ApproveChannelItem(campaign, "updates", "run-task"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, _ := os.ReadFile(filepath.Join(dir, "run-task.json"))
	if string(before) != string(after) {
		t.Fatal("reapproval changed original approval")
	}
}

func TestChannelApprovalRejectsInvalidItems(t *testing.T) {
	for _, change := range []string{"digest", "channel", "status", "id"} {
		t.Run(change, func(t *testing.T) {
			campaign, dir := approvalFixture(t)
			path := filepath.Join(dir, "run-task.json")
			data, _ := os.ReadFile(path)
			var it ChannelItem
			json.Unmarshal(data, &it)
			switch change {
			case "digest":
				it.Text = "tampered"
			case "channel":
				it.Channel = "elsewhere"
			case "status":
				it.Status = "rejected"
			case "id":
				it.ID = "../run-task"
			}
			data, _ = json.Marshal(it)
			os.WriteFile(path, data, 0600)
			if err := ApproveChannelItem(campaign, "updates", "run-task"); err == nil {
				t.Fatal("approved invalid item")
			}
		})
	}
}
