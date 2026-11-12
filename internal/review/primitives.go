package review

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/textsafe"
)

func TerminalText(text string) string { return textsafe.Terminal(text) }
func Digest(data []byte) string       { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func GitBlobMatches(data []byte, objectID string) bool {
	payload := append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...)
	if len(objectID) == 40 {
		h := sha1.Sum(payload)
		return hex.EncodeToString(h[:]) == objectID
	}
	if len(objectID) == 64 {
		return Digest(payload) == objectID
	}
	return false
}
func Path(p string) bool { return localfs.RelativePath(p) }
