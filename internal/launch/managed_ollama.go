package launch

import (
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const OllamaMaxOutputTokens int64 = 512

// The endpoint is a pinned address, not an authenticated server identity.
func ValidateOllamaEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery || strings.ContainsAny(endpoint, "?#") {
		return errors.New("Ollama requires a bare HTTP loopback endpoint")
	}
	host, port, err := net.SplitHostPort(u.Host)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || (host != "127.0.0.1" && host != "::1") {
		return errors.New("Ollama requires numeric loopback and explicit port")
	}
	return nil
}
func validOllamaDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && strings.ToLower(s) == s
}
