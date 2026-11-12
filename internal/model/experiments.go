package model

type PlanExperiments struct {
	Messaging *MessagingExperiment `json:"messaging,omitempty"`
}

type MessagingExperiment struct {
	Enabled        bool             `json:"enabled"`
	Edges          []MessageEdge    `json:"edges,omitempty"`
	Publish        []ChannelPublish `json:"publish,omitempty"`
	Subscribe      []ChannelSub     `json:"subscribe,omitempty"`
	MaxBytes       int              `json:"max_bytes,omitempty"`
	MaxPerTask     int              `json:"max_per_task,omitempty"`
	MaxPerRun      int              `json:"max_per_run,omitempty"`
	MaxHops        int              `json:"max_hops,omitempty"`
	CrossSwarmGate string           `json:"cross_swarm_gate,omitempty"`
}

// MessageEdge sends the verified output Field of task From to task To as a note.
type MessageEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Field string `json:"field"`
}

// ChannelPublish posts the verified output Field of Task to a campaign channel.
type ChannelPublish struct {
	Channel string `json:"channel"`
	Task    string `json:"task"`
	Field   string `json:"field"`
}

// ChannelSub delivers open items of a campaign channel to Task as notes.
type ChannelSub struct {
	Channel string `json:"channel"`
	Task    string `json:"task"`
}

func (m *MessagingExperiment) Limits() (bytes, perTask, perRun, hops int) {
	bytes, perTask, perRun, hops = 4096, 8, 256, 2
	if m.MaxBytes > 0 {
		bytes = m.MaxBytes
	}
	if m.MaxPerTask > 0 {
		perTask = m.MaxPerTask
	}
	if m.MaxPerRun > 0 {
		perRun = m.MaxPerRun
	}
	if m.MaxHops > 0 {
		hops = m.MaxHops
	}
	return
}
