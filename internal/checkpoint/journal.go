package checkpoint

// The journal is a transaction log, not an independently written event sidecar.
// state.json is the commit marker. Only its fsynced prefix is acknowledged;
// an uncommitted tail is ignored by readers and truncated by the next writer.
// Snapshot/log generations are immutable to readers under the shared state lock.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/phasetrace"
	"github.com/grainulation/irrigator/internal/store"
)

// stateReadLimit bounds allocation independently from graph and worker limits.
const stateReadLimit int64 = 128 << 20

const journalDirectory = "checkpoint-data"
const journalByteLimit int64 = 16 << 20
const journalTransactionLimit = 128

type journalHead struct {
	SchemaVersion int              `json:"schema_version"`
	Version       int              `json:"version"`
	RunID         string           `json:"run_id"`
	PlanHash      string           `json:"plan_hash"`
	Storage       *journalPosition `json:"checkpoint_storage"`
}

type journalPosition struct {
	Format           string `json:"format"`
	Snapshot         string `json:"snapshot"`
	SnapshotSHA256   string `json:"snapshot_sha256"`
	SnapshotSequence uint64 `json:"snapshot_sequence"`
	Journal          string `json:"journal"`
	Bytes            int64  `json:"bytes"`
	Sequence         uint64 `json:"sequence"`
	SHA256           string `json:"sha256"`
	LogicalBound     int64  `json:"logical_bound"`
}

type statePatch struct {
	Path   []string        `json:"path"`
	Value  json.RawMessage `json:"value,omitempty"`
	Remove bool            `json:"remove,omitempty"`
	Append bool            `json:"append,omitempty"`
}

type journalTransaction struct {
	Sequence uint64       `json:"sequence"`
	Previous string       `json:"previous"`
	Patches  []statePatch `json:"patches"`
}

type journalRecord struct {
	Transaction journalTransaction `json:"transaction"`
	SHA256      string             `json:"sha256"`
}

var storageName = regexp.MustCompile(`^checkpoint-data/(snapshot|journal)-[a-f0-9]{32}\.(json|jsonl)$`)

func digestCheckpoint(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func isJournalCheckpoint(data []byte) bool {
	var h struct {
		Storage json.RawMessage `json:"checkpoint_storage"`
	}
	return json.Unmarshal(data, &h) == nil && len(h.Storage) > 0 && string(h.Storage) != "null"
}

func parseJournalHead(data []byte) (journalHead, error) {
	var h journalHead
	if err := validateJournalJSON(data); err != nil {
		return h, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&h); err != nil {
		return h, err
	}
	p := h.Storage
	if h.SchemaVersion != 3 || h.Version < 1 || h.Version > 5 || h.RunID == "" || len(h.PlanHash) != 64 || p == nil || p.Format != "irrigator.journal/v1" ||
		!storageName.MatchString(p.Snapshot) || !strings.HasSuffix(p.Snapshot, ".json") || !strings.Contains(p.Snapshot, "/snapshot-") ||
		!storageName.MatchString(p.Journal) || !strings.HasSuffix(p.Journal, ".jsonl") || !strings.Contains(p.Journal, "/journal-") ||
		p.Bytes < 0 || p.Bytes > journalByteLimit || p.Sequence < p.SnapshotSequence || p.Sequence-p.SnapshotSequence > journalTransactionLimit ||
		len(p.SHA256) != 64 || len(p.SnapshotSHA256) != 64 || p.LogicalBound < 1 || p.LogicalBound > stateReadLimit {
		return h, errors.New("invalid journal commit marker")
	}
	return h, nil
}

func readBoundedCheckpoint(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("checkpoint must be a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("checkpoint exceeds byte limit")
	}
	return b, err
}

func boundedCheckpointJSON(s *model.State, limit int64) ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("logical checkpoint exceeds byte limit")
	}
	return b, nil
}

func publishPlainCheckpoint(root string, s *model.State) error {
	b, err := boundedCheckpointJSON(s, stateReadLimit)
	if err != nil {
		return err
	}
	if err = jsonio.AtomicBytes(filepath.Join(root, "state.json"), b); err != nil {
		return err
	}
	checkpointPublications.Add(1)
	checkpointLogicalBytes.Add(uint64(len(b)))
	return nil
}

func journalReadFile(root, name string) ([]byte, error) {
	if !storageName.MatchString(name) {
		return nil, errors.New("unsafe journal path")
	}
	i, err := os.Lstat(filepath.Join(root, journalDirectory))
	if err != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe journal directory")
	}
	return readBoundedCheckpoint(filepath.Join(root, filepath.FromSlash(name)), stateReadLimit)
}

type journalCacheEntry struct {
	head         journalHead
	state        *model.State
	prefixSHA256 string
}

var committedCache = struct {
	sync.Mutex
	entries map[string]journalCacheEntry
}{entries: map[string]journalCacheEntry{}}

type plainCacheEntry struct {
	digest string
	state  *model.State
}

var plainCache = struct {
	sync.Mutex
	entries map[string]plainCacheEntry
}{entries: map[string]plainCacheEntry{}}

func readPlainCheckpoint(root string, raw []byte) (*model.State, error) {
	key, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	digest := digestCheckpoint(raw)
	plainCache.Lock()
	defer plainCache.Unlock()
	if prior, ok := plainCache.entries[key]; ok && prior.digest == digest {
		s := cloneCheckpoint(prior.state)
		if err := validateRecoveryCheckpoint(root, s); err != nil {
			return nil, err
		}
		return s, nil
	}
	s, err := DecodeState(root, raw)
	if err != nil {
		return nil, err
	}
	if len(raw) <= 32<<20 {
		if len(plainCache.entries) >= 2 {
			for old := range plainCache.entries {
				delete(plainCache.entries, old)
				break
			}
		}
		plainCache.entries[key] = plainCacheEntry{digest, cloneCheckpoint(s)}
	}
	return s, nil
}

// readJournalModelWithBase is called only while holding the shared/exclusive
// state lock. Cache identities include actual snapshot and committed log bytes,
// not just mtimes; external corruption is never hidden by a warm cache. It also
// returns base, a read-only state equal to the returned one before the caller
// changes it, or nil when none is retained. base is the cache's own immutable
// entry: the caller may compare against it but must never modify it, which
// saves a whole-state clone per commit.
//
// sharePlan lets the returned state share its plan and event details with the
// cache entry (see cloneSharingPlan). Only Update and the prewarm, which discards
// the state, pass true; anything that hands a state to a reader passes false.
func readJournalModelWithBase(root string, raw []byte, sharePlan bool) (*model.State, *model.State, error) {
	in, err := loadJournalInputs(root, raw)
	if err != nil {
		return nil, nil, err
	}
	return decodeJournalInputs(root, in, sharePlan)
}

// journalInputs are the committed bytes of one journal generation, read while
// the state lock was held. Everything after that point works from these owned
// bytes, so a shared reader need not keep the lock through the decode.
type journalInputs struct {
	raw      []byte
	head     journalHead
	snapshot []byte
	log      []byte
	logErr   error // reported where the unsplit read reported it, after the snapshot checks
}

// loadJournalInputs parses the commit marker and reads the snapshot and journal
// files. It is the only part of a journal read that needs the state lock.
func loadJournalInputs(root string, raw []byte) (journalInputs, error) {
	h, err := parseJournalHead(raw)
	if err != nil {
		return journalInputs{}, err
	}
	snapshot, err := journalReadFile(root, h.Storage.Snapshot)
	if err != nil {
		return journalInputs{}, err
	}
	log, logErr := journalReadFile(root, h.Storage.Journal)
	return journalInputs{raw: raw, head: h, snapshot: snapshot, log: log, logErr: logErr}, nil
}

// decodeJournalInputs verifies, replays and validates inputs read by
// loadJournalInputs. It needs no state lock.
func decodeJournalInputs(root string, in journalInputs, sharePlan bool) (*model.State, *model.State, error) {
	clone := cloneCheckpoint
	if sharePlan {
		clone = cloneSharingPlan
	}
	raw, h, snapshot, log := in.raw, in.head, in.snapshot, in.log
	if digestCheckpoint(snapshot) != h.Storage.SnapshotSHA256 {
		return nil, nil, errors.New("journal snapshot digest mismatch")
	}
	if h.Storage.LogicalBound != int64(len(snapshot))+h.Storage.Bytes {
		return nil, nil, errors.New("journal logical size bound mismatch")
	}
	if in.logErr != nil {
		return nil, nil, in.logErr
	}
	if int64(len(log)) < h.Storage.Bytes {
		return nil, nil, errors.New("journal committed prefix is truncated")
	}
	prefix := digestCheckpoint(log[:h.Storage.Bytes])
	key, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	committedCache.Lock()
	defer committedCache.Unlock()
	prior, ok := committedCache.entries[key]
	// Replay and validation work on a copy that shares the plan and event details
	// with the cache entry; in the dual verify mode, prove they leave them alone.
	sharedIntact := func() error { return nil }
	if sharePlan && ok && journalVerifyMode == journalVerifyDual {
		same := snapshotShared(prior.state)
		sharedIntact = func() error {
			if same() {
				return nil
			}
			delete(committedCache.entries, key)
			return errors.New("journal replay or validation wrote through state shared with the commit cache")
		}
	}
	if ok && reflect.DeepEqual(prior.head, h) && prior.prefixSHA256 == prefix {
		s := clone(prior.state)
		if err := validateRecoveryCheckpoint(root, s); err != nil {
			return nil, nil, err
		}
		if err := sharedIntact(); err != nil {
			return nil, nil, err
		}
		return s, prior.state, nil
	}
	var cached *journalCacheEntry
	if ok && prior.head.Storage.Snapshot == h.Storage.Snapshot && prior.head.Storage.Journal == h.Storage.Journal && prior.head.Storage.Bytes <= h.Storage.Bytes && prior.head.Storage.SnapshotSequence == h.Storage.SnapshotSequence &&
		prior.head.Storage.SnapshotSHA256 == h.Storage.SnapshotSHA256 && digestCheckpoint(log[:prior.head.Storage.Bytes]) == prior.prefixSHA256 {
		cached = &prior
	}
	var tail replayTail
	s, err := expandJournalInto(raw, func(name string) ([]byte, error) {
		if name == h.Storage.Snapshot {
			return snapshot, nil
		}
		if name == h.Storage.Journal {
			return log, nil
		}
		return nil, errors.New("unknown journal file")
	}, cached, &tail, sharePlan)
	if err != nil {
		return nil, nil, err
	}
	// Every replayed transaction is hash-chained onto a base that was fully
	// validated, so the whole-state marshal and re-parse is an identity round
	// trip. Apply the same typed validation without it when the replay is of a
	// supported shape; anything else, and every cold load, takes the full path.
	if cached != nil && journalVerifyMode != journalVerifyFull {
		var sFull *model.State
		if journalVerifyMode == journalVerifyDual {
			sFull = cloneCheckpoint(s)
		}
		if validated, handled, err := validateIncrementalReplay(root, s, &tail); handled {
			if journalVerifyMode == journalVerifyDual {
				full, _, fullErr := validateFullReplay(root, sFull)
				if (err != nil) != (fullErr != nil) || (err == nil && !reflect.DeepEqual(validated, full)) {
					return nil, nil, fmt.Errorf("incremental journal validation diverged from full validation (incremental error %v, full error %v)", err, fullErr)
				}
			}
			if err != nil {
				return nil, nil, err
			}
			var base *model.State
			if h.Storage.LogicalBound <= 32<<20 {
				if len(committedCache.entries) >= 2 && !ok {
					for old := range committedCache.entries {
						delete(committedCache.entries, old)
						break
					}
				}
				base = clone(validated)
				committedCache.entries[key] = journalCacheEntry{h, base, prefix}
			}
			if err := sharedIntact(); err != nil {
				return nil, nil, err
			}
			return validated, base, nil
		}
	}
	validated, logicalSize, err := validateFullReplay(root, s)
	if err != nil {
		return nil, nil, err
	}
	var base *model.State
	if logicalSize <= 32<<20 {
		if len(committedCache.entries) >= 2 && !ok {
			for old := range committedCache.entries {
				delete(committedCache.entries, old)
				break
			}
		}
		base = clone(validated)
		committedCache.entries[key] = journalCacheEntry{h, base, prefix}
	}
	if err := sharedIntact(); err != nil {
		return nil, nil, err
	}
	return validated, base, nil
}

// expandJournal reconstructs only committed transactions and checks the entire
// hash chain. The caller holds state.lock, or supplies immutable backup bytes.
func expandJournal(data []byte, read func(string) ([]byte, error), cached *journalCacheEntry) (*model.State, error) {
	return expandJournalInto(data, read, cached, nil, false)
}

// expandJournalInto is expandJournal that also records, when tail is non-nil,
// each patch it applies on top of the cached state.
func expandJournalInto(data []byte, read func(string) ([]byte, error), cached *journalCacheEntry, tail *replayTail, sharePlan bool) (*model.State, error) {
	h, err := parseJournalHead(data)
	if err != nil {
		return nil, err
	}
	p := h.Storage
	var s *model.State
	offset, sequence, chain := int64(0), p.SnapshotSequence, p.SnapshotSHA256
	if cached != nil && cached.head.Storage.Snapshot == p.Snapshot && cached.head.Storage.Journal == p.Journal &&
		cached.head.Storage.Sequence <= p.Sequence && cached.head.Storage.Bytes <= p.Bytes && cached.head.Storage.SnapshotSHA256 == p.SnapshotSHA256 {
		if sharePlan {
			s = cloneSharingPlan(cached.state)
		} else {
			s = cloneCheckpoint(cached.state)
		}
		offset, sequence, chain = cached.head.Storage.Bytes, cached.head.Storage.Sequence, cached.head.Storage.SHA256
	} else {
		b, e := read(p.Snapshot)
		if e != nil {
			return nil, e
		}
		if digestCheckpoint(b) != p.SnapshotSHA256 {
			return nil, errors.New("journal snapshot digest mismatch")
		}
		if p.LogicalBound != int64(len(b))+p.Bytes {
			return nil, errors.New("journal logical size bound mismatch")
		}
		s = &model.State{}
		if err := json.Unmarshal(b, s); err != nil {
			return nil, err
		}
	}
	log, err := read(p.Journal)
	if err != nil {
		return nil, err
	}
	if int64(len(log)) < p.Bytes {
		return nil, errors.New("journal committed prefix is truncated")
	}
	committed := log[offset:p.Bytes]
	if len(committed) > 0 && committed[len(committed)-1] != '\n' {
		return nil, errors.New("incomplete committed journal transaction")
	}
	for _, line := range bytes.Split(committed, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if err := validateJournalJSON(line); err != nil {
			return nil, err
		}
		var rec journalRecord
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return nil, fmt.Errorf("corrupt committed journal: %w", err)
		}
		body, err := json.Marshal(rec.Transaction)
		if err != nil {
			return nil, err
		}
		if rec.SHA256 != digestCheckpoint(body) || rec.Transaction.Previous != chain || rec.Transaction.Sequence != sequence+1 || len(rec.Transaction.Patches) == 0 {
			return nil, errors.New("journal transaction integrity or sequence mismatch")
		}
		for _, patch := range rec.Transaction.Patches {
			if len(patch.Path) == 0 || len(patch.Path) > 32 {
				return nil, errors.New("invalid journal patch path")
			}
			eventsBefore := len(s.Events)
			if err := applyStatePatch(reflect.ValueOf(s).Elem(), patch.Path, patch); err != nil {
				return nil, err
			}
			if tail != nil {
				tail.record(patch, eventsBefore, len(s.Events))
			}
		}
		sequence, chain = rec.Transaction.Sequence, rec.SHA256
	}
	if sequence != p.Sequence || chain != p.SHA256 || s.RunID != h.RunID || s.PlanHash != h.PlanHash || s.Version != h.Version {
		return nil, errors.New("journal checkpoint identity mismatch")
	}
	return s, nil
}

func validateJournalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if _, err := jsonio.ParseStrictJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("journal JSON has trailing values")
	}
	return nil
}

func expandFileJournal(root string, data []byte, locked bool) ([]byte, error) {
	if !locked {
		f, err := os.OpenFile(filepath.Join(root, "state.lock"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
			return nil, err
		}
		defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	}
	// Cache only between reads of the same committed files. All bytes are still
	// checked on periodic full reads, including edits preserving marker metadata.
	s, err := expandJournal(data, func(name string) ([]byte, error) { return journalReadFile(root, name) }, nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// ExpandCheckpointFiles reconstructs an archived journal from its verified file
// inventory without opening source paths, creating files or executing work.
func ExpandCheckpointFiles(data []byte, files map[string][]byte) ([]byte, error) {
	if !isJournalCheckpoint(data) {
		return data, nil
	}
	s, err := expandJournal(data, func(name string) ([]byte, error) {
		b, ok := files[name]
		if !ok {
			return nil, errors.New("backup missing journal file")
		}
		return b, nil
	}, nil)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err == nil && int64(len(b)) > stateReadLimit {
		return nil, errors.New("logical checkpoint exceeds byte limit")
	}
	return b, err
}

// CheckpointStorageFiles returns the complete current recovery set. Old derived
// generations are excluded from backups; their history is in the current state.
func CheckpointStorageFiles(root string) (map[string]bool, error) {
	b, err := readBoundedCheckpoint(filepath.Join(root, "state.json"), stateReadLimit)
	if err != nil {
		return nil, err
	}
	if !isJournalCheckpoint(b) {
		return nil, nil
	}
	h, err := parseJournalHead(b)
	if err != nil {
		return nil, err
	}
	return map[string]bool{h.Storage.Snapshot: true, h.Storage.Journal: true}, nil
}

// readCheckpointForUpdate returns the validated state for the caller to change,
// the raw commit marker and, for a journal checkpoint, a read-only base equal to
// the state before the change (nil when none is retained).
func readCheckpointForUpdate(root string) (*model.State, []byte, *model.State, error) {
	b, err := readBoundedCheckpoint(filepath.Join(root, "state.json"), stateReadLimit)
	if err != nil {
		return nil, nil, nil, err
	}
	if isJournalCheckpoint(b) {
		s, base, readErr := readJournalModelWithBase(root, b, true)
		return s, b, base, readErr
	}
	s, err := readPlainCheckpoint(root, b)
	return s, b, nil, err
}

func readCurrentJournal(root string) (*model.State, error) {
	in, plain, err := loadCurrentCheckpoint(root)
	if err != nil {
		return nil, err
	}
	if plain != nil {
		return plain, nil
	}
	if journalReadHook != nil {
		journalReadHook()
	}
	s, _, err := decodeJournalInputs(root, in, false)
	return s, err
}

// journalReadHook, when set by a test, runs after a journal read has released
// the shared state lock and before it decodes.
var journalReadHook func()

// loadCurrentCheckpoint holds the shared state lock only long enough to read the
// commit marker and the committed journal files; a plain checkpoint is decoded
// under the lock as before. A journal checkpoint comes back as inputs for
// decodeJournalInputs, which runs after the lock is released so that a long
// decode never makes a committing writer wait.
func loadCurrentCheckpoint(root string) (journalInputs, *model.State, error) {
	phasetrace.Emit("ckpt", "rd_enter", "")
	f, err := os.OpenFile(filepath.Join(root, "state.lock"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return journalInputs{}, nil, err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return journalInputs{}, nil, err
	}
	phasetrace.Emit("ckpt", "rd_locked", "")
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		phasetrace.Emit("ckpt", "rd_unlocked", "")
	}()
	raw, err := readBoundedCheckpoint(filepath.Join(root, "state.json"), stateReadLimit)
	if err != nil {
		return journalInputs{}, nil, err
	}
	if !isJournalCheckpoint(raw) {
		s, err := DecodeState(root, raw)
		return journalInputs{}, s, err
	}
	in, err := loadJournalInputs(root, raw)
	return in, nil, err
}

func publishJournalSnapshot(root string, s *model.State, sequence uint64) error {
	if err := os.MkdirAll(filepath.Join(root, journalDirectory), 0700); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(root, journalDirectory))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe journal directory")
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return err
	}
	name := hex.EncodeToString(id)
	snapshot := journalDirectory + "/snapshot-" + name + ".json"
	log := journalDirectory + "/journal-" + name + ".jsonl"
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if int64(len(b)) > stateReadLimit {
		return errors.New("logical checkpoint exceeds byte limit")
	}
	if err = jsonio.AtomicBytes(filepath.Join(root, filepath.FromSlash(snapshot)), b); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(log)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Join(root, journalDirectory))
	if err != nil {
		return err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, "state.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	lock.Close()
	hash := digestCheckpoint(b)
	h := journalHead{3, s.Version, s.RunID, s.PlanHash, &journalPosition{"irrigator.journal/v1", snapshot, hash, sequence, log, 0, sequence, hash, int64(len(b))}}
	if err = jsonio.AtomicJSON(filepath.Join(root, "state.json"), h); err != nil {
		return err
	}
	checkpointPublications.Add(1)
	checkpointSnapshots.Add(1)
	checkpointLogicalBytes.Add(uint64(len(b)))
	if marker, err := os.Stat(filepath.Join(root, "state.json")); err == nil {
		checkpointLogicalBytes.Add(uint64(marker.Size()))
	}
	// Exclusive writer ownership plus shared reader locks make old generations
	// disposable only after the new commit marker has been durably published.
	entries, err := os.ReadDir(filepath.Join(root, journalDirectory))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := journalDirectory + "/" + entry.Name()
		if storageName.MatchString(path) && path != snapshot && path != log {
			if err = os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil {
				return err
			}
		}
	}
	return nil
}

func publishJournalUpdate(root string, raw []byte, before, after *model.State) error {
	h, err := parseJournalHead(raw)
	if err != nil {
		return err
	}
	var patches []statePatch
	if err = diffState(reflect.ValueOf(before).Elem(), reflect.ValueOf(after).Elem(), nil, &patches); err != nil {
		return err
	}
	if len(patches) == 0 {
		checkpointNoops.Add(1)
		return nil
	}
	tx := journalTransaction{h.Storage.Sequence + 1, h.Storage.SHA256, patches}
	body, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	rec := journalRecord{tx, digestCheckpoint(body)}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if h.Storage.Sequence-h.Storage.SnapshotSequence >= journalTransactionLimit || h.Storage.Bytes+int64(len(line)) > journalByteLimit || h.Storage.LogicalBound+int64(len(line)) > stateReadLimit {
		return publishJournalSnapshot(root, after, tx.Sequence)
	}
	path := filepath.Join(root, filepath.FromSlash(h.Storage.Journal))
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Truncate(h.Storage.Bytes); err != nil {
		return err
	}
	if _, err = f.Seek(h.Storage.Bytes, io.SeekStart); err != nil {
		return err
	}
	if _, err = f.Write(line); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	h.Storage.Bytes += int64(len(line))
	h.Storage.LogicalBound += int64(len(line))
	h.Storage.Sequence = tx.Sequence
	h.Storage.SHA256 = rec.SHA256
	h.Version = after.Version
	h.RunID = after.RunID
	h.PlanHash = after.PlanHash
	if err = jsonio.AtomicJSON(filepath.Join(root, "state.json"), h); err != nil {
		return err
	}
	checkpointPublications.Add(1)
	checkpointTransactions.Add(1)
	checkpointLogicalBytes.Add(uint64(len(line)))
	if marker, err := os.Stat(filepath.Join(root, "state.json")); err == nil {
		checkpointLogicalBytes.Add(uint64(marker.Size()))
	}
	return nil
}

// EnableJournal migrates an inactive or active run under the normal transaction
// authority/lock gates, preserving a complete original JSON checkpoint backup.
func EnableJournal(root string) error {
	if err := RejectQuarantinedRun(root); err != nil {
		return err
	}
	lock, err := FileLock(filepath.Join(root, "state.lock"), false)
	if err != nil {
		return err
	}
	defer Unlock(lock)
	s, raw, _, err := readCheckpointForUpdate(root)
	if err != nil {
		return err
	}
	if isJournalCheckpoint(raw) {
		return nil
	}
	if err = ValidateCampaignAuthority(root, s.Plan, false); err != nil {
		return err
	}
	if err = ValidateBillingAuthority(s); err != nil {
		return err
	}
	if err = store.EnsureBackup(filepath.Join(root, "state.json")); err != nil {
		return err
	}
	return publishJournalSnapshot(root, s, 0)
}

func cloneCheckpoint(s *model.State) *model.State {
	return cloneValue(reflect.ValueOf(s)).Interface().(*model.State)
}

// cloneSharingPlan is cloneCheckpoint except that the copy shares two kinds of
// immutable data with the original instead of duplicating them:
//
//   - the Plan, which is immutable provenance (see model.State) and never changes
//     between graph revisions, which are recorded in ActiveTasks and Revisions;
//   - the Detail value of every event. Events are append-only and a detail is
//     built once, when the event is appended. The Event structs themselves are
//     copied into a private array, so appending to or replacing an element of the
//     copy's slice never reaches the original; only the maps and slices that a
//     detail points to are shared.
//
// Together they are about half of the work of a whole-state clone and equality
// walk on a large run, and identical shared pointers let the diff skip them.
//
// Sharing is only sound while nothing writes through State.Plan or into an
// existing event's Detail after a read. Replacing a whole field is fine. Update
// guards against such writes in the dual verify mode used by the tests
// (guardSharedState).
func cloneSharingPlan(s *model.State) *model.State {
	without := *s
	without.Plan = model.Plan{}
	without.Events = nil
	c := cloneCheckpoint(&without)
	c.Plan = s.Plan
	if s.Events != nil {
		c.Events = make([]model.Event, len(s.Events))
		copy(c.Events, s.Events)
	}
	return c
}

func cloneValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		o := reflect.New(v.Type().Elem())
		o.Elem().Set(cloneValue(v.Elem()))
		return o
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		o := reflect.New(v.Type()).Elem()
		o.Set(cloneValue(v.Elem()))
		return o
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		o := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			o.SetMapIndex(it.Key(), cloneValue(it.Value()))
		}
		return o
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		o := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			o.Index(i).Set(cloneValue(v.Index(i)))
		}
		return o
	case reflect.Struct:
		o := reflect.New(v.Type()).Elem()
		o.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if o.Field(i).CanSet() && v.Type().Field(i).IsExported() {
				o.Field(i).Set(cloneValue(v.Field(i)))
			}
		}
		return o
	default:
		return v
	}
}

// diffDescendsFirst reports whether diffState descends into a without comparing
// it whole first: the root struct, and a non-nil string-keyed map on both sides.
// Both cases recurse over every member, so an equal pair emits nothing.
func diffDescendsFirst(a, b reflect.Value, path []string) bool {
	switch a.Kind() {
	case reflect.Struct:
		return len(path) == 0 && a.Type().PkgPath() != "time"
	case reflect.Map:
		return !a.IsNil() && !b.IsNil() && a.Type().Key().Kind() == reflect.String
	}
	return false
}

func jsonField(t reflect.StructField) string {
	name := strings.Split(t.Tag.Get("json"), ",")[0]
	if name == "" {
		name = t.Name
	}
	return name
}

func diffState(a, b reflect.Value, path []string, out *[]statePatch) error {
	// Comparing a container before descending into it walks the same data again
	// at every level, so the root struct and string-keyed maps go straight to
	// their members; each member still gets its own comparison. Identical
	// inputs then produce no patches exactly as before.
	if !diffDescendsFirst(a, b, path) && reflect.DeepEqual(a.Interface(), b.Interface()) {
		return nil
	}
	emit := func(value reflect.Value, appendOnly bool) error {
		raw, err := json.Marshal(value.Interface())
		if err == nil {
			*out = append(*out, statePatch{Path: append([]string{}, path...), Value: raw, Append: appendOnly})
		}
		return err
	}
	if a.Kind() == reflect.Pointer && !a.IsNil() && !b.IsNil() {
		return diffState(a.Elem(), b.Elem(), path, out)
	}
	switch a.Kind() {
	case reflect.Struct:
		if a.Type().PkgPath() == "time" {
			return emit(b, false)
		}
		for i := 0; i < a.NumField(); i++ {
			field := a.Type().Field(i)
			name := jsonField(field)
			if !field.IsExported() || name == "-" {
				continue
			}
			if err := diffState(a.Field(i), b.Field(i), append(append([]string{}, path...), name), out); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return emit(b, false)
		}
		if a.Type().Key().Kind() != reflect.String {
			return emit(b, false)
		}
		it := a.MapRange()
		for it.Next() {
			key := it.Key()
			next := b.MapIndex(key)
			p := append(append([]string{}, path...), key.String())
			if !next.IsValid() {
				*out = append(*out, statePatch{Path: p, Remove: true})
				continue
			}
			if err := diffState(it.Value(), next, p, out); err != nil {
				return err
			}
		}
		it = b.MapRange()
		for it.Next() {
			if !a.MapIndex(it.Key()).IsValid() {
				raw, err := json.Marshal(it.Value().Interface())
				if err != nil {
					return err
				}
				*out = append(*out, statePatch{Path: append(append([]string{}, path...), it.Key().String()), Value: raw})
			}
		}
		return nil
	case reflect.Slice:
		if b.Len() > a.Len() && reflect.DeepEqual(a.Interface(), b.Slice(0, a.Len()).Interface()) {
			return emit(b.Slice(a.Len(), b.Len()), true)
		}
	}
	return emit(b, false)
}

func applyStatePatch(v reflect.Value, path []string, p statePatch) error {
	if len(path) == 0 {
		if p.Remove {
			return errors.New("journal remove requires map key")
		}
		n := reflect.New(v.Type())
		if err := json.Unmarshal(p.Value, n.Interface()); err != nil {
			return err
		}
		if p.Append {
			if v.Kind() != reflect.Slice {
				return errors.New("journal append requires array")
			}
			v.Set(reflect.AppendSlice(v, n.Elem()))
		} else {
			v.Set(n.Elem())
		}
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return errors.New("journal path through nil pointer")
		}
		return applyStatePatch(v.Elem(), path, p)
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.IsExported() && jsonField(f) == path[0] {
				return applyStatePatch(v.Field(i), path[1:], p)
			}
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return errors.New("journal requires string map key")
		}
		key := reflect.ValueOf(path[0]).Convert(v.Type().Key())
		if len(path) == 1 && p.Remove {
			v.SetMapIndex(key, reflect.Value{})
			return nil
		}
		old := v.MapIndex(key)
		next := reflect.New(v.Type().Elem()).Elem()
		if old.IsValid() {
			next.Set(old)
		}
		if err := applyStatePatch(next, path[1:], p); err != nil {
			return err
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		v.SetMapIndex(key, next)
		return nil
	}
	return errors.New("invalid journal patch path")
}
