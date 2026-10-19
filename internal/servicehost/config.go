package servicehost

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// AllOperations as a tool's whole allow list admits every operation.
const AllOperations = "*"

const defaultCallTimeout = 5 * time.Minute

// Config is the service host's own file. Nothing in it comes from the lever
// host: the ceiling is set where the credentials live.
type Config struct {
	Listen              string        `yaml:"listen"`
	AllowWildcardListen bool          `yaml:"allow_wildcard_listen"`
	ClientFingerprint   string        `yaml:"client_fingerprint"`
	Cert                string        `yaml:"cert"`
	Key                 string        `yaml:"key"`
	CallTimeout         time.Duration `yaml:"call_timeout"`
	Tools               []Tool        `yaml:"tools"`

	dir string
}

// Tool is one MCP server the service host serves at /mcp/<name>.
type Tool struct {
	Name        string            `yaml:"name"`
	Command     []string          `yaml:"command"`
	Dir         string            `yaml:"dir"`
	Env         map[string]string `yaml:"env"`
	EnvFiles    map[string]string `yaml:"env_files"`
	PassEnv     []string          `yaml:"pass_env"`
	Upstream    string            `yaml:"upstream"`
	Headers     map[string]string `yaml:"headers"`
	HeaderFiles map[string]string `yaml:"header_files"`
	// Allow is the ceiling: the operations this instance may call here,
	// whatever the broker allows.
	Allow []string `yaml:"allow"`
}

// allows reports whether op is inside the tool's ceiling.
func (t Tool) allows(op string) bool {
	return slices.Equal(t.Allow, []string{AllOperations}) || slices.Contains(t.Allow, op)
}

// Matches broker.tools names, which become the same route on the broker.
var toolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Header names the service host sets or that would change how the request
// is framed or routed; a configured header must not override them.
var reservedHeaders = []string{
	"Host", "Content-Length", "Content-Type", "Transfer-Encoding", "Connection", "Upgrade",
	"Te", "Trailer", "Keep-Alive", "Proxy-Authorization", "Proxy-Connection",
	"Accept", "Mcp-Session-Id", "Mcp-Protocol-Version",
}

// LoadConfig reads and checks the service host config at path. It refuses a
// file another user owns or others can write: whoever can edit it can widen
// the ceiling.
func LoadConfig(path string) (*Config, error) {
	b, err := readChecked(path, false)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("service host config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.dir = filepath.Dir(abs)
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("service host config %s: %w", path, err)
	}
	return &c, nil
}

// CertPath and KeyPath default to server.crt and server.key beside the
// config.
func (c *Config) CertPath() string { return c.resolve(c.Cert, "server.crt") }
func (c *Config) KeyPath() string  { return c.resolve(c.Key, "server.key") }

func (c *Config) resolve(p, def string) string {
	if p == "" {
		p = def
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

func (c *Config) callTimeout() time.Duration {
	if c.CallTimeout > 0 {
		return c.CallTimeout
	}
	return defaultCallTimeout
}

func (c *Config) tool(name string) (Tool, bool) {
	for _, t := range c.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func (c *Config) validate() error {
	if err := checkListen(c.Listen, c.AllowWildcardListen); err != nil {
		return err
	}
	if c.ClientFingerprint == "" {
		return errors.New("client_fingerprint is required (lever service-host pair prints it on the lever host)")
	}
	if _, err := ParseFingerprint(c.ClientFingerprint); err != nil {
		return fmt.Errorf("client_fingerprint: %w", err)
	}
	if c.CallTimeout < 0 {
		return errors.New("call_timeout must not be negative")
	}
	if len(c.Tools) == 0 {
		return errors.New("tools: at least one tool is required")
	}
	seen := map[string]bool{}
	for _, t := range c.Tools {
		if err := t.validate(); err != nil {
			return err
		}
		if seen[t.Name] {
			return fmt.Errorf("tool %q is declared twice", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// checkListen refuses a wildcard address unless forced: it would also listen
// on every other network the machine is on.
func checkListen(listen string, allowWildcard bool) error {
	if listen == "" {
		return errors.New("listen is required (host:port, this machine's tailnet address)")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q: %v", listen, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
		return fmt.Errorf("listen %q: needs a fixed port, 1-65535", listen)
	}
	if host == "" {
		if allowWildcard {
			return nil
		}
		return fmt.Errorf("listen %q has no address, which listens on every network; give this machine's tailnet address, or set allow_wildcard_listen: true", listen)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("listen %q: the host must be a literal IP address", listen)
	}
	if ip.IsUnspecified() && !allowWildcard {
		return fmt.Errorf("listen %q is a wildcard address, which listens on every network; give this machine's tailnet address, or set allow_wildcard_listen: true", listen)
	}
	return nil
}

func (t Tool) validate() error {
	if !toolNameRE.MatchString(t.Name) {
		return fmt.Errorf("tool name %q must match %s", t.Name, toolNameRE)
	}
	hasCmd, hasUp := len(t.Command) > 0, t.Upstream != ""
	if hasCmd == hasUp {
		return fmt.Errorf("tool %q needs exactly one of command and upstream", t.Name)
	}
	if len(t.Allow) == 0 {
		return fmt.Errorf("tool %q needs allow: the operations this instance may call (or [\"*\"] for all)", t.Name)
	}
	for _, op := range t.Allow {
		if op == "" {
			return fmt.Errorf("tool %q allow has an empty operation", t.Name)
		}
		if op == AllOperations && len(t.Allow) > 1 {
			return fmt.Errorf("tool %q allow mixes \"*\" with operation names", t.Name)
		}
	}
	if hasCmd {
		if t.Command[0] == "" {
			return fmt.Errorf("tool %q command has an empty program", t.Name)
		}
		if len(t.Headers)+len(t.HeaderFiles) > 0 {
			return fmt.Errorf("tool %q: headers and header_files apply to an upstream tool only", t.Name)
		}
		for _, m := range []map[string]string{t.Env, t.EnvFiles} {
			for k := range m {
				if !envNameRE.MatchString(k) {
					return fmt.Errorf("tool %q: %q is not an environment variable name", t.Name, k)
				}
			}
		}
		for _, k := range t.PassEnv {
			if !envNameRE.MatchString(k) {
				return fmt.Errorf("tool %q pass_env: %q is not an environment variable name", t.Name, k)
			}
		}
		return nil
	}
	if t.Dir != "" || len(t.Env)+len(t.EnvFiles)+len(t.PassEnv) > 0 {
		return fmt.Errorf("tool %q: dir, env, env_files and pass_env apply to a command tool only", t.Name)
	}
	u, err := url.Parse(t.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("tool %q upstream %q: want an http:// or https:// URL without credentials", t.Name, t.Upstream)
	}
	for _, m := range []map[string]string{t.Headers, t.HeaderFiles} {
		for k := range m {
			ck := http.CanonicalHeaderKey(k)
			if !validHeaderName(k) || slices.Contains(reservedHeaders, ck) || strings.HasPrefix(ck, "X-Lever-") {
				return fmt.Errorf("tool %q: header %q cannot be set", t.Name, k)
			}
		}
	}
	return nil
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// readChecked reads a file that only its owner, who must be this user or
// root, can change; with private, that only the owner can read either. The
// checks are made on the opened file, so a swap after them changes nothing.
func readChecked(path string, private bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() && st.Uid != 0 {
		return nil, fmt.Errorf("%s is owned by uid %d, not by this user", path, st.Uid)
	}
	perm := fi.Mode().Perm()
	if private && perm&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable or writable by group or others (mode %04o); chmod 600 it", path, perm)
	}
	if perm&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by group or others (mode %04o); chmod go-w it", path, perm)
	}
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// readPrivate reads a key or secret file that only its owner may read.
func readPrivate(path string) ([]byte, error) { return readChecked(path, true) }

// readSecretValue is a secret file's content without its trailing newline.
// A header value must be one line; an environment value may hold several.
func readSecretValue(path string, multiline bool) (string, error) {
	b, err := readPrivate(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	if strings.ContainsRune(v, 0) || (!multiline && strings.ContainsAny(v, "\r\n")) {
		return "", fmt.Errorf("%s holds a NUL or, for a header, more than one line", path)
	}
	return v, nil
}
