package checkpoint

import "sync/atomic"

var checkpointDecodes atomic.Uint64
var checkpointPublications atomic.Uint64
var checkpointLogicalBytes atomic.Uint64
var checkpointNoops atomic.Uint64
var checkpointSnapshots atomic.Uint64
var checkpointTransactions atomic.Uint64

// StorageMetrics reports application counters in this process only. It does not
// measure physical disk traffic or counters in re-executed worker processes.
func StorageMetrics() map[string]uint64 {
	return map[string]uint64{
		"full_decodes":          checkpointDecodes.Load(),
		"publications":          checkpointPublications.Load(),
		"logical_bytes_written": checkpointLogicalBytes.Load(),
		"unchanged_updates":     checkpointNoops.Load(),
		"snapshots":             checkpointSnapshots.Load(),
		"journal_transactions":  checkpointTransactions.Load(),
	}
}
