package graph

import (
	"time"
)

// Delivery is separately journaled: acceptance never changes graph verification,
// and resuming a workflow never implicitly retries failed paid work.
type DeliveryRecord struct {
	RunID         string    `json:"run_id"`
	GraphRevision int       `json:"graph_revision,omitempty"`
	Directory     string    `json:"directory"`
	Nodes         []string  `json:"nodes"`
	Field         string    `json:"field"`
	Check         []string  `json:"check"`
	Status        string    `json:"status"`
	Started       time.Time `json:"started"`
	Updated       time.Time `json:"updated"`
	GraphMS       int64     `json:"graph_ms"`
	ExportMS      int64     `json:"export_ms"`
	CheckMS       int64     `json:"check_ms"`
	WallMS        int64     `json:"wall_ms"`
	ReportedCost  float64   `json:"reported_cost_usd"`
	Error         string    `json:"error,omitempty"`
}
