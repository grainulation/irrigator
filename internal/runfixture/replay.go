package runfixture

import (
	"bytes"
	"os"
	"path/filepath"
)

func ReplayDir() string {
	if d := os.Getenv("IRRIGATOR_REPLAY_DIR"); d != "" {
		return d
	}
	return filepath.Join("..", "..", "internal", "debug", "testdata", "replay")
}

const (
	SentinelPrompt = "SENTINEL_PROMPT_7f3a"
	SentinelEquals = "SENTINEL_EQUALS_91bc"
	SentinelOutput = "SENTINEL_OUTPUT_c0de"
	SentinelEnv    = "SENTINEL_ENV_TOKEN_55aa"
	SentinelToken  = "sk-ant-SENTINELtoken0123456789abcdef"
)

func SentinelHomePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/Users/sentinel-user"
	}
	return filepath.Join(home, ".config", "replay-sentinel", "credentials.json")
}

func ReplayContainsSentinel(b []byte) string {
	for _, s := range []string{"SENTINEL_", "SENTINELtoken", "replay-sentinel", SentinelEnv, SentinelPrompt, SentinelEquals, SentinelOutput, SentinelToken, SentinelHomePath()} {
		if bytes.Contains(b, []byte(s)) {
			return s
		}
	}
	return ""
}
