package wire

// Reserved names of the broker's built-in LLM pseudo-tool: the /llm proxy,
// which has no backend subprocess and no /mcp/llm/ route.
const (
	ReservedLLMTool = "llm"
	ReservedLLMOp   = "generate"
)

// WildcardOp is the operation name of a coarse tool's single capability. It is
// a literal op matched exactly, not a pattern; the gateway requires it for a
// coarse tool and never accepts it for a fine one, so "*" cannot widen a fine
// tool.
const WildcardOp = "*"
