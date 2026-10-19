// Package wire holds the on-the-wire and on-disk envelope types shared by the
// host, the broker and the in-container agent, so a JSON tag cannot drift
// between producer and consumer. It is a leaf: it must import nothing
// internal, so any package can depend on it without an import cycle.
package wire

// Bootstrap is the enrolment envelope the host (for the manager) or the broker
// (for a worker, and on auto-re-enrol) deposits as bootstrap.json for
// lever-agent boot to consume.
type Bootstrap struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
	// Claude is the agent's Claude Code configuration from the host config,
	// applied by lever-agent boot at every start. nil when none is
	// configured.
	Claude *Claude `json:"claude,omitempty"`
}
