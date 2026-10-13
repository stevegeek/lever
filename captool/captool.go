// Package captool is the host-side SDK a first-party (capability-aware) lever
// tool uses to serve MCP, independently verify the presented capability token,
// enforce a hard backstop, and register with the broker at boot.
package captool

import (
	"crypto/ed25519"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/logging"
)

const defaultEpochTTL = 5 * time.Second

// The broker-to-tool shared secret: minted per broker boot, passed to each
// supervised tool in ToolSecretEnv (environment, never argv), and presented
// by the gateway in ToolSecretHeader. A request without it is refused before
// any MCP dispatch, so a jail agent that reaches the tool's loopback port
// directly cannot self-assert X-Lever-Caller. The broker spells the header
// separately (toolSecretHeader); a broker test pins the two together.
const (
	ToolSecretHeader = "X-Lever-Tool-Secret"
	ToolSecretEnv    = "LEVER_TOOL_SECRET"
)

// NewLogger returns a logger in lever's log line shape, with credential
// masking, for Config.Log. The broker's supervisor writes a tool's stderr to
// tool-logs/<tool>.log.
func NewLogger(w io.Writer, component string) *slog.Logger {
	return logging.New(w, component)
}

// ParamSpec is one declared argument of an Operation.
type ParamSpec struct{ Name, Type, Description string }

// ValidatedContext is the verified call context handed to Backstop and
// Handler. Constraints are the constraint-keyed parameters every token caveat
// was checked against.
type ValidatedContext struct {
	Caller      string
	Tool        string
	Operation   string
	Constraints map[string]string
}

// Operation is one MCP tool the host-side tool exposes.
type Operation struct {
	Name        string
	Description string
	Params      []ParamSpec
	// CaveatParam maps a token constraint key to a differently-named request
	// argument (scalar args only).
	CaveatParam map[string]string
	// Backstop enforces the tool's hard invariants after token verification,
	// independent of the token; a non-nil error denies the call. It receives
	// the raw args the Handler will run on, not the constraint-keyed params.
	Backstop func(ValidatedContext, map[string]string) error
	// Handler performs the action with clean args (_capability removed).
	Handler func(ValidatedContext, map[string]string) (any, error)
}

// Config assembles a Server.
type Config struct {
	Name       string // the capability tool this verifies (e.g. "db")
	Version    string // default "dev"
	Backend    string // listen address (host loopback)
	AdminURL   string // broker admin base URL (loopback)
	Operations []Operation
	EpochTTL   time.Duration // epoch cache TTL (default 5s)
	Log        *slog.Logger
	// Secret is the broker-to-tool shared secret; empty means ToolSecretEnv.
	// With neither set every request is refused: the server cannot tell a
	// brokered request from a direct dial.
	Secret string
}

// Server is a running captool MCP server.
type Server struct {
	name     string
	version  string
	backend  string
	adminURL string
	ops      map[string]Operation
	log      *slog.Logger
	epochTTL time.Duration
	secret   []byte // empty refuses every request

	mu      sync.Mutex
	pubKey  ed25519.PublicKey
	epoch   int
	epochAt time.Time
}

// New validates the config and builds a Server. Call Register before serving.
func New(c Config) (*Server, error) {
	if c.Name == "" || c.Backend == "" {
		return nil, fmt.Errorf("captool: Name and Backend are required")
	}
	if len(c.Operations) == 0 {
		return nil, fmt.Errorf("captool: at least one operation is required")
	}
	if c.EpochTTL <= 0 {
		c.EpochTTL = defaultEpochTTL
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	ops := make(map[string]Operation, len(c.Operations))
	for _, o := range c.Operations {
		if o.Name == "" || o.Handler == nil {
			return nil, fmt.Errorf("captool: operation needs a Name and Handler")
		}
		ops[o.Name] = o
	}
	secret := c.Secret
	if secret == "" {
		secret = os.Getenv(ToolSecretEnv)
	}
	if secret == "" {
		c.Log.Warn("captool.no_tool_secret", "tool", c.Name,
			"detail", "no broker tool secret (Config.Secret / "+ToolSecretEnv+"): every request will be refused; run this tool under the lever broker supervisor")
	}
	return &Server{
		name: c.Name, version: c.Version, backend: c.Backend, adminURL: c.AdminURL,
		ops: ops, log: c.Log, epochTTL: c.EpochTTL, secret: []byte(secret),
	}, nil
}

// secretOK compares in constant time; an empty secret matches nothing.
func (s *Server) secretOK(presented string) bool {
	return len(s.secret) > 0 && subtle.ConstantTimeCompare([]byte(presented), s.secret) == 1
}

// Handler returns the MCP-over-HTTP handler to serve on Config.Backend.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// audit's message and fields are a stable record operators grep. The detail
// is masked here because Config.Log may be any handler.
func (s *Server) audit(op, caller, decision, detail string) {
	s.log.Info("captool.decision", "tool", s.name, "op", op, "caller", caller, "decision", decision, "detail", logging.Redact(detail))
}
