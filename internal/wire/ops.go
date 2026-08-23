package wire

// Reserved operation and tool names shared by config (which validates and
// injects them), the broker registry (which enforces them) and the agent side
// (which mints and presents them). They live here, in the contract leaf, so
// config need not import the broker's registry to spell them.

// Reserved names of the broker's built-in LLM pseudo-tool: the /llm proxy,
// which has no backend subprocess and no /mcp/llm/ route. config injects the
// implicit {llm, generate} grant; registry and broker alias these.
const (
	ReservedLLMTool = "llm"
	ReservedLLMOp   = "generate"
)

// WildcardOp is the operation name of a coarse tool's single capability. It is
// a literal op value — minted, granted, and verified by exact match — so the
// token layer needs no wildcard logic; the gateway simply REQUIRES this op for
// a coarse tool (and never for a fine one, so "*" cannot widen a fine tool).
const WildcardOp = "*"
