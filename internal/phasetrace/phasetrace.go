// Package phasetrace records timestamped phase markers from the controller, the
// workers and the checkpoint store into one append-only file, to attribute where
// the time of a task goes. It does nothing unless IRRIGATOR_PHASE_TRACE names a
// file, so it is free in normal runs.
package phasetrace

import (
	"os"
	"strconv"
	"sync"
	"time"
)

var path = os.Getenv("IRRIGATOR_PHASE_TRACE")

var (
	once sync.Once
	file *os.File
)

// Emit appends "<unix nanoseconds> <pid> <role> <phase> <task>". Each line is a
// single O_APPEND write, so concurrent processes interleave whole lines.
func Emit(role, phase, task string) {
	if path == "" {
		return
	}
	once.Do(func() { file, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) })
	if file == nil {
		return
	}
	if task == "" {
		task = "-"
	}
	_, _ = file.WriteString(strconv.FormatInt(time.Now().UnixNano(), 10) + " " + strconv.Itoa(os.Getpid()) + " " + role + " " + phase + " " + task + "\n")
}
