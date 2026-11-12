// Package examples embeds the credential-free demo plan.
package examples

import _ "embed"

// TerminalSwarmJSON is the mock plan used by irrigator demo.
//
//go:embed terminal-swarm.json
var TerminalSwarmJSON []byte
