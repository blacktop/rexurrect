package state

import "github.com/blacktop/rexurrect/internal/snapshot"

type AgentNotResumed struct {
	BlockID snapshot.SourceID `json:"block_source_id"`
	Harness string            `json:"harness"`
	Reason  string            `json:"reason"`
}

// The envelope has already validated each receipt. Keep the skip decision in
// the create intent so lost acknowledgements cannot erase this result.
func agentNotResumed(session snapshot.Session) []AgentNotResumed {
	var out []AgentNotResumed
	for _, b := range session.Blocks() {
		if r := b.AgentResume; r != nil && r.InitialInput() == "" {
			out = append(out, AgentNotResumed{BlockID: b.ID, Harness: r.Name(), Reason: "automatic resume disabled"})
		}
	}
	return out
}
