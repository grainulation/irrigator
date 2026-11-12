package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/review"
)

const reviewSnapshotLimit = 64 << 20

func ReviewTerminalText(text string) string { return review.TerminalText(text) }

func ReviewEnv() []string {
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
}

type ReviewLimitedBuffer struct {
	bytes.Buffer
	Limit int
}

func (b *ReviewLimitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.Limit-b.Len() {
		return 0, errors.New("review Git output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func ReviewGit(workspace string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	argv := append([]string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "log.showSignature=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = workspace
	cmd.Env = ReviewEnv()
	out := &ReviewLimitedBuffer{Limit: reviewSnapshotLimit}
	stderr := &ReviewLimitedBuffer{Limit: 8192}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("review git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
