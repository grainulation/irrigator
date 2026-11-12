package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/localfs"
)

const snapshotLimit = 64 << 20

func contextText(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}
func text(data []byte) bool { return utf8.Valid(data) && !bytes.ContainsRune(data, 0) }

// Blob is one workspace path. Contents are retained only when they are
// needed later and not recoverable from Git; SHA256 always identifies them.
type Blob struct {
	Path   string `json:"path"`
	Data   []byte `json:"data"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256,omitempty"`
	Object string `json:"object,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Large  bool   `json:"large,omitempty"`
	Link   bool   `json:"link,omitempty"`

	GitSHA1, GitSHA256 string `json:"-"`
}
type File struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	Binary       bool   `json:"binary"`
	Large        bool   `json:"large,omitempty"`
	Link         bool   `json:"link,omitempty"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
	Before       string `json:"before"`
	After        string `json:"after"`
	Diff         string `json:"diff"`
	BeforeMode   uint32 `json:"before_mode"`
	AfterMode    uint32 `json:"after_mode"`
}
type Comment struct {
	ID         string `json:"id"`
	SnapshotID string `json:"snapshot_id"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
}
type Snapshot struct {
	Kind       string    `json:"kind,omitempty"`
	Title      string    `json:"title,omitempty"`
	Body       string    `json:"body,omitempty"`
	Version    int       `json:"version"`
	ID         string    `json:"id"`
	Workspace  string    `json:"workspace"`
	BaseCommit string    `json:"base_commit"`
	BaselineID string    `json:"baseline_id,omitempty"`
	CapturedAt string    `json:"captured_at"`
	Files      []File    `json:"files"`
	Inventory  []Blob    `json:"inventory,omitempty"`
	Comments   []Comment `json:"comments"`
}

func Identity(s Snapshot) string {
	s.ID = ""
	s.Comments = nil
	data, _ := json.Marshal(s)
	return Digest(data)
}
func ReadFile(path string, limit int64) ([]byte, error) {
	path, err := localfs.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := localfs.OpenContextFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("review evidence must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("review evidence exceeds limit")
	}
	return data, nil
}
func ReadSnapshot(dir string) (*Snapshot, error) {
	data, err := ReadFile(filepath.Join(dir, "snapshot.json"), snapshotLimit)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&s); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("review snapshot must contain one JSON value")
	}
	if s.Version != 1 || s.ID == "" || s.ID != Identity(s) {
		return nil, errors.New("review snapshot content hash mismatch")
	}
	switch s.Kind {
	case "", "code":
	case "plan", "direction", "decision":
		if !contextText(s.Title, 200) || !utf8.ValidString(s.Title) || strings.TrimSpace(s.Title) == "" || len(s.Body) > 1<<20 || !text([]byte(s.Body)) || strings.TrimSpace(s.Body) == "" || len(s.Files) != 1 || len(s.Inventory) != 0 || s.BaseCommit != "" || s.BaselineID != "" {
			return nil, errors.New("invalid document review snapshot")
		}
		f := s.Files[0]
		if f.Path != "review.md" || f.Status != "added" || f.Binary || f.Before != "" || f.After != s.Body || f.Diff != s.Body || f.AfterSHA256 != Digest([]byte(s.Body)) {
			return nil, errors.New("document review artifact does not match its body")
		}
	default:
		return nil, errors.New("unsupported review snapshot kind")
	}
	s.Comments = []Comment{}
	return &s, nil
}
func WriteSnapshot(dir string, s *Snapshot) error {
	dir, err := localfs.Abs(dir)
	if err != nil {
		return err
	}
	parent, err := localfs.Root(filepath.Dir(dir))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("review output parent directory does not exist: %s", filepath.Dir(dir))
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	if err = parent.Mkdir(filepath.Base(dir), 0700); err != nil {
		return err
	}
	lock, err := localfs.OpenContextFile(filepath.Join(dir, "comments.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = lock.Close(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > snapshotLimit {
		return errors.New("review snapshot exceeds limit")
	}
	f, err := localfs.OpenContextFile(filepath.Join(dir, "snapshot.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
