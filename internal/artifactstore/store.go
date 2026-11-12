package artifactstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Reference is portable content identity. Key is relative to an authorized run.
// It contains neither a filesystem path outside that run nor access credentials.
type Reference struct {
	RunID  string `json:"run_id"`
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Store publishes new immutable objects and returns only verified bytes. A Put
// error does not establish that a remote write had no effect; reconcile the same
// reference before retry. Implementations must never overwrite different bytes.
type Store interface {
	Put(context.Context, Reference, io.Reader) error
	Get(context.Context, Reference) ([]byte, error)
}

func validIdentity(ref Reference) error {
	if ref.RunID == "" || len(ref.RunID) > 256 || !utf8.ValidString(ref.RunID) || strings.ContainsAny(ref.RunID, "\x00\r\n") {
		return errors.New("invalid artifact run scope")
	}
	if !fs.ValidPath(ref.Key) || ref.Key == "." || !utf8.ValidString(ref.Key) || len(ref.Key) > 4096 || strings.ContainsAny(ref.Key, "\\\x00") {
		return errors.New("artifact key must be a canonical relative path")
	}
	if ref.Size < 0 || len(ref.SHA256) != 64 || strings.ToLower(ref.SHA256) != ref.SHA256 {
		return errors.New("invalid artifact size or digest")
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return errors.New("invalid artifact digest")
	}
	return nil
}

func Describe(runID, key string, data []byte) (Reference, error) {
	sum := sha256.Sum256(data)
	ref := Reference{RunID: runID, Key: key, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	return ref, validIdentity(ref)
}

// local binds one existing private directory to one trusted run scope. Its root
// handle confines resolution, including parent symlinks. A future hosted adapter
// must bind the scope from authenticated authority rather than request data.
type local struct {
	root     *os.Root
	runID    string
	maxBytes int64
}

func NewLocal(directory, runID string, maxBytes int64) (*local, error) {
	if _, err := Describe(runID, "probe", nil); err != nil {
		return nil, err
	}
	if maxBytes < 1 || maxBytes > 1<<30 {
		return nil, errors.New("artifact byte limit must be 1..1 GiB")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &local{root: root, runID: runID, maxBytes: maxBytes}, nil
}
func (s *local) Close() error { return s.root.Close() }
func (s *local) check(ctx context.Context, ref Reference) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validIdentity(ref); err != nil {
		return err
	}
	if ref.RunID != s.runID {
		return errors.New("artifact run scope mismatch")
	}
	if ref.Size > s.maxBytes {
		return errors.New("artifact exceeds byte limit")
	}
	return nil
}

func verifiedBytes(ctx context.Context, ref Reference, input io.Reader) ([]byte, error) {
	if input == nil {
		return nil, errors.New("artifact reader is nil")
	}
	// Blocking readers must implement their own context cancellation. No worker
	// receives these bytes until the complete bounded payload has been verified.
	data, err := io.ReadAll(io.LimitReader(input, ref.Size+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Size {
		return nil, errors.New("artifact length mismatch")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, errors.New("artifact digest mismatch")
	}
	return data, nil
}

func (s *local) Put(ctx context.Context, ref Reference, input io.Reader) error {
	if err := s.check(ctx, ref); err != nil {
		return err
	}
	data, err := verifiedBytes(ctx, ref, input)
	if err != nil {
		return err
	}
	parent := path.Dir(ref.Key)
	if err = s.root.MkdirAll(parent, 0700); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	staged := path.Join(parent, ".irrigator-artifact-"+hex.EncodeToString(nonce[:]))
	f, err := s.root.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = s.root.Remove(staged)
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Link publishes the already complete file atomically and refuses replacement.
	// Unlike Rename, it cannot overwrite a concurrent writer's object.
	if err = s.root.Link(staged, ref.Key); err != nil {
		return err
	}
	if err = s.root.Remove(staged); err != nil {
		return err
	}
	// Once bytes are published, a directory-sync error is an ambiguous durability
	// acknowledgement. Keep the verified object available for reconciliation.
	directory, err := s.root.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func (s *local) Get(ctx context.Context, ref Reference) ([]byte, error) {
	if err := s.check(ctx, ref); err != nil {
		return nil, err
	}
	f, err := s.root.OpenFile(ref.Key, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size {
		return nil, errors.New("artifact must be a regular file of the expected size")
	}
	return verifiedBytes(ctx, ref, f)
}
