// Package captool is the host-side SDK a first-party (capability-aware) lever
// tool uses to serve MCP, independently verify the presented capability token,
// enforce a hard backstop, and register with the broker at boot.
package captool

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const defaultEpochTTL = 5 * time.Second

// ParamSpec is one declared argument of an Operation (drives the MCP schema).
type ParamSpec struct{ Name, Type, Description string }

// ValidatedContext is the verified call context handed to Backstop/Handler.
// Constraints are the verified, constraint-keyed parameters (every token caveat
// was satisfied by the corresponding value).
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
	// CaveatParam maps a token constraint key to the request argument it binds
	// (scalar args only). Identity (key == arg) is the common case.
	CaveatParam map[string]string
	// Backstop enforces the tool's hard invariants AFTER token verification,
	// independent of the token; a non-nil error denies the call.
	Backstop func(ValidatedContext, map[string]string) error
	// Handler performs the action with clean args (_capability removed).
	Handler func(ValidatedContext, map[string]string) (any, error)
}

// Config assembles a Server.
type Config struct {
	Name       string // tool/registry name; the capability tool this verifies (e.g. "db")
	Backend    string // listen address (host loopback)
	AdminURL   string // broker admin base URL (loopback) for /register and /epoch
	Operations []Operation
	EpochTTL   time.Duration // freshness cache TTL (default 5s)
	Log        *slog.Logger
}

// Server is a running captool MCP server.
type Server struct {
	name     string
	backend  string
	adminURL string
	ops      map[string]Operation
	log      *slog.Logger
	epochTTL time.Duration

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
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ops := make(map[string]Operation, len(c.Operations))
	for _, o := range c.Operations {
		if o.Name == "" || o.Handler == nil {
			return nil, fmt.Errorf("captool: operation needs a Name and Handler")
		}
		ops[o.Name] = o
	}
	return &Server{
		name: c.Name, backend: c.Backend, adminURL: c.AdminURL,
		ops: ops, log: c.Log, epochTTL: c.EpochTTL,
	}, nil
}

// Handler returns the MCP-over-HTTP handler (mount on Config.Backend).
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) audit(op, caller, decision, detail string) {
	s.log.Info("captool.decision", "tool", s.name, "op", op, "caller", caller, "decision", decision, "detail", detail)
}
