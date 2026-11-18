package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
)

// captureStream is supplementary evidence about the local pipe capture. It
// says nothing about provider completeness, successful execution or safe replay.
type captureStream struct {
	SHA256        string `json:"sha256"`
	CapturedBytes int64  `json:"captured_bytes"`
	LimitBytes    int64  `json:"limit_bytes"`
	Truncated     bool   `json:"truncated"`
	Complete      bool   `json:"complete"`
}

func writeProcessCapture(prefix string, stdout, stderr *cappedWriter, complete bool) error {
	streams := make(map[string]captureStream, 2)
	for name, item := range map[string]struct {
		suffix string
		writer *cappedWriter
		limit  int64
	}{
		"stdout": {".stdout.json", stdout, model.MaxOutputBytes},
		"stderr": {".stderr.log", stderr, 1024 * 1024},
	} {
		f, err := os.Open(prefix + item.suffix)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(f, item.limit+1))
		_ = f.Close()
		if err != nil {
			return err
		}
		streams[name] = captureStream{hex.EncodeToString(h.Sum(nil)), n, item.limit, item.writer.overflow, complete && !item.writer.failed}
	}
	raw, err := json.Marshal(struct {
		Schema  string                   `json:"schema"`
		Streams map[string]captureStream `json:"streams"`
	}{"irrigator.process-capture/v1", streams})
	if err != nil {
		return err
	}
	// Never overwrite evidence from an earlier invocation of this attempt. A
	// missing, partial or mismatched sidecar is reported as unknown by readers.
	return localfs.WriteNew(prefix+".capture.json", append(raw, '\n'))
}
