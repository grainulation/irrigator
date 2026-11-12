package model

import (
	"fmt"
	"io"
	"os"
)

type ProcessMarker struct {
	Status         string `json:"status"`
	SupervisorPID  int    `json:"supervisor_pid"`
	ConfirmedBy    string `json:"confirmed_by,omitempty"`
	PreviousStatus string `json:"previous_status,omitempty"`
	CleanupError   string `json:"cleanup_error,omitempty"`
}

func ReadProcessEvidence(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("process evidence exceeds %d byte limit: %s", limit, path)
	}
	return raw, nil
}
