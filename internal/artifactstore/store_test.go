package artifactstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	artifacts "github.com/grainulation/irrigator/internal/artifactstore"
)

// This byte-map adapter is intentionally independent of the file implementation.
// Shared tests specify the storage contract, not filesystem mechanics.
type memoryStore struct {
	mu      sync.Mutex
	scope   string
	limit   int64
	objects map[string][]byte
}

func (m *memoryStore) valid(r artifacts.Reference) bool {
	digest, e := hex.DecodeString(r.SHA256)
	return r.RunID == m.scope && r.Key != "" && r.Key != "." && !strings.HasPrefix(r.Key, "/") && !strings.Contains(r.Key, "\\") && !strings.Contains(r.Key, "\x00") && path.Clean(r.Key) == r.Key && r.Key != ".." && !strings.HasPrefix(r.Key, "../") && e == nil && len(digest) == sha256.Size && strings.ToLower(r.SHA256) == r.SHA256 && r.Size >= 0 && r.Size <= m.limit
}
func (m *memoryStore) Put(ctx context.Context, r artifacts.Reference, reader io.Reader) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if !m.valid(r) {
		return errors.New("invalid reference")
	}
	data, e := io.ReadAll(io.LimitReader(reader, m.limit+1))
	if e != nil {
		return e
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != r.Size || hex.EncodeToString(digest[:]) != r.SHA256 {
		return errors.New("invalid content")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objects[r.Key]; exists {
		return fs.ErrExist
	}
	m.objects[r.Key] = append([]byte(nil), data...)
	return nil
}
func (m *memoryStore) Get(ctx context.Context, r artifacts.Reference) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if !m.valid(r) {
		return nil, errors.New("invalid reference")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[r.Key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != r.Size || hex.EncodeToString(digest[:]) != r.SHA256 {
		return nil, errors.New("invalid content")
	}
	return append([]byte(nil), data...), nil
}
func reference(t *testing.T, key string, data []byte) artifacts.Reference {
	t.Helper()
	r, e := artifacts.Describe("run-a", key, data)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func adapters(t *testing.T, fn func(*testing.T, artifacts.Store)) {
	t.Helper()
	for _, name := range []string{"local", "memory"} {
		t.Run(name, func(t *testing.T) {
			var store artifacts.Store
			if name == "local" {
				local, e := artifacts.NewLocal(t.TempDir(), "run-a", 1024)
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					if e := local.Close(); e != nil {
						t.Error(e)
					}
				})
				store = local
			} else {
				store = &memoryStore{scope: "run-a", limit: 1024, objects: map[string][]byte{}}
			}
			fn(t, store)
		})
	}
}
func TestStoreRoundtripImmutable(t *testing.T) {
	adapters(t, func(t *testing.T, s artifacts.Store) {
		data := []byte("accepted answer")
		r := reference(t, "answer.json", data)
		ctx := context.Background()
		if e := s.Put(ctx, r, bytes.NewReader(data)); e != nil {
			t.Fatal(e)
		}
		got, e := s.Get(ctx, r)
		if e != nil || !bytes.Equal(got, data) {
			t.Fatalf("roundtrip %q %v", got, e)
		}
		got[0] = 'X'
		again, e := s.Get(ctx, r)
		if e != nil || !bytes.Equal(again, data) {
			t.Fatal("read aliases stored bytes")
		}
		if e := s.Put(ctx, r, bytes.NewReader(data)); !errors.Is(e, fs.ErrExist) {
			t.Fatalf("overwrite: %v", e)
		}
		changed := reference(t, r.Key, []byte("replacement"))
		if e := s.Put(ctx, changed, strings.NewReader("replacement")); !errors.Is(e, fs.ErrExist) {
			t.Fatalf("replacement: %v", e)
		}
	})
}
func TestStoreRejectsInvalidReferences(t *testing.T) {
	adapters(t, func(t *testing.T, s artifacts.Store) {
		good := reference(t, "answer.json", []byte("answer"))
		ctx := context.Background()
		cases := map[string]artifacts.Reference{}
		for _, key := range []string{"", ".", "..", "../escape", "/absolute", "a/../escape", "a//b", "a/./b", "a\\b", "a\x00b"} {
			r := good
			r.Key = key
			cases["key:"+key] = r
		}
		r := good
		r.RunID = "run-b"
		cases["scope"] = r
		r = good
		r.SHA256 = "not-a-digest"
		cases["digest"] = r
		r = good
		r.Size = -1
		cases["negative size"] = r
		r = good
		r.Size = 1025
		cases["oversize"] = r
		for name, r := range cases {
			t.Run(name, func(t *testing.T) {
				if e := s.Put(ctx, r, strings.NewReader("answer")); e == nil {
					t.Fatal("invalid put accepted")
				}
				if _, e := s.Get(ctx, r); e == nil {
					t.Fatal("invalid get accepted")
				}
			})
		}
		if e := s.Put(ctx, good, strings.NewReader("answer")); e != nil {
			t.Fatalf("invalid attempts poisoned key: %v", e)
		}
	})
}
func TestStoreRejectsPartialOrIncorrectContent(t *testing.T) {
	adapters(t, func(t *testing.T, s artifacts.Store) {
		ctx := context.Background()
		r := reference(t, "answer.json", []byte("answer"))
		for _, data := range []string{"ans", "answer extra", "wrong!", strings.Repeat("x", 2048)} {
			if e := s.Put(ctx, r, strings.NewReader(data)); e == nil {
				t.Fatal("bad content accepted")
			}
			if _, e := s.Get(ctx, r); e == nil {
				t.Fatal("partial object published")
			}
		}
		if e := s.Put(ctx, r, errorReader{}); e == nil {
			t.Fatal("reader failure ignored")
		}
		if e := s.Put(ctx, r, strings.NewReader("answer")); e != nil {
			t.Fatalf("failed put retained key: %v", e)
		}
		wrong := r
		wrong.SHA256 = strings.Repeat("0", 64)
		if _, e := s.Get(ctx, wrong); e == nil {
			t.Fatal("wrong digest accepted")
		}
		wrong = r
		wrong.Size++
		if _, e := s.Get(ctx, wrong); e == nil {
			t.Fatal("wrong length accepted")
		}
	})
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("injected read failure") }
func TestStoreCancelledBeforeCall(t *testing.T) {
	adapters(t, func(t *testing.T, s artifacts.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := reference(t, "answer.json", []byte("answer"))
		if e := s.Put(ctx, r, strings.NewReader("answer")); !errors.Is(e, context.Canceled) {
			t.Fatalf("cancelled put %v", e)
		}
		if _, e := s.Get(ctx, r); !errors.Is(e, context.Canceled) {
			t.Fatalf("cancelled get %v", e)
		}
		if e := s.Put(context.Background(), r, strings.NewReader("answer")); e != nil {
			t.Fatalf("cancelled put published object: %v", e)
		}
	})
}
func TestStoreConcurrentImmutablePublish(t *testing.T) {
	adapters(t, func(t *testing.T, s artifacts.Store) {
		r := reference(t, "answer.json", []byte("answer"))
		results := make(chan error, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); results <- s.Put(context.Background(), r, strings.NewReader("answer")) }()
		}
		wg.Wait()
		close(results)
		wins := 0
		for e := range results {
			if e == nil {
				wins++
			} else if !errors.Is(e, fs.ErrExist) {
				t.Error(e)
			}
		}
		if wins != 1 {
			t.Fatalf("successful publishers=%d", wins)
		}
	})
}

func TestLocalTamperedContentAndUnsafeObjects(t *testing.T) {
	for _, kind := range []string{"digest", "length", "symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			local, e := artifacts.NewLocal(dir, "run-a", 1024)
			if e != nil {
				t.Fatal(e)
			}
			defer local.Close()
			r := reference(t, "answer.json", []byte("answer"))
			name := filepath.Join(dir, r.Key)
			switch kind {
			case "digest":
				e = os.WriteFile(name, []byte("wrong!"), 0600)
			case "length":
				e = os.WriteFile(name, []byte("answer extra"), 0600)
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("answer"), 0600); err != nil {
					t.Fatal(err)
				}
				e = os.Symlink(outside, name)
			case "directory":
				e = os.Mkdir(name, 0700)
			case "fifo":
				e = syscall.Mkfifo(name, 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { _, err := local.Get(context.Background(), r); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("tampered/unsafe object accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("unsafe object blocked read")
			}
		})
	}
}
func TestLocalNestedKeysAndParentEscape(t *testing.T) {
	dir := t.TempDir()
	local, e := artifacts.NewLocal(dir, "run-a", 1024)
	if e != nil {
		t.Fatal(e)
	}
	defer local.Close()
	r := reference(t, "results/answer.json", []byte("answer"))
	if e := local.Put(context.Background(), r, strings.NewReader("answer")); e != nil {
		t.Fatal(e)
	}
	if _, e := local.Get(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(dir, "escape")); e != nil {
		t.Fatal(e)
	}
	r = reference(t, "escape/answer.json", []byte("answer"))
	if e := local.Put(context.Background(), r, strings.NewReader("answer")); e == nil {
		t.Fatal("escaped parent accepted")
	}
	if _, e := os.Stat(filepath.Join(outside, "answer.json")); !errors.Is(e, fs.ErrNotExist) {
		t.Fatalf("outside directory changed: %v", e)
	}
}

// A blocked input makes the publication boundary observable without sleeps.
type pausedReader struct {
	started chan struct{}
	release chan struct{}
	phase   int
}

func (p *pausedReader) Read(b []byte) (int, error) {
	if p.phase == 0 {
		p.phase++
		close(p.started)
		return copy(b, []byte("ans")), nil
	}
	if p.phase == 1 {
		p.phase++
		<-p.release
		return copy(b, []byte("wer")), nil
	}
	return 0, io.EOF
}
func TestStorePublishesOnlyCompleteObject(t *testing.T) {
	adapters(t, func(t *testing.T, s artifacts.Store) {
		input := &pausedReader{started: make(chan struct{}), release: make(chan struct{})}
		var once sync.Once
		release := func() { once.Do(func() { close(input.release) }) }
		defer release()
		r := reference(t, "answer.json", []byte("answer"))
		done := make(chan error, 1)
		go func() { done <- s.Put(context.Background(), r, input) }()
		select {
		case <-input.started:
		case <-time.After(time.Second):
			t.Fatal("put did not read input")
		}
		_, e := s.Get(context.Background(), r)
		release()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("put did not complete")
		}
		if !errors.Is(e, fs.ErrNotExist) {
			t.Fatalf("uncommitted object visible: %v", e)
		}
		if _, e := s.Get(context.Background(), r); e != nil {
			t.Fatal(e)
		}
	})
}

func TestStoreCancellationDuringReadLeavesNoObject(t *testing.T) {
	adapters(t, func(t *testing.T, s artifacts.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		input := &pausedReader{started: make(chan struct{}), release: make(chan struct{})}
		var once sync.Once
		release := func() { once.Do(func() { close(input.release) }) }
		defer release()
		r := reference(t, "answer.json", []byte("answer"))
		done := make(chan error, 1)
		go func() { done <- s.Put(ctx, r, input) }()
		select {
		case <-input.started:
		case <-time.After(time.Second):
			t.Fatal("put did not read input")
		}
		cancel()
		release()
		select {
		case e := <-done:
			if !errors.Is(e, context.Canceled) {
				t.Fatalf("cancelled put: %v", e)
			}
		case <-time.After(time.Second):
			t.Fatal("put did not stop")
		}
		if _, e := s.Get(context.Background(), r); !errors.Is(e, fs.ErrNotExist) {
			t.Fatalf("cancelled object exists: %v", e)
		}
		if e := s.Put(context.Background(), r, strings.NewReader("answer")); e != nil {
			t.Fatal(e)
		}
	})
}
