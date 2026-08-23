package agent

// Version is the lever-agent build version advertised in the capability MCP
// server's serverInfo. It is "dev" for a plain `go build`; the Makefile's
// lever-agent-linux target stamps the release version with
// -ldflags "-X github.com/stevegeek/lever/internal/agent.Version=…". It lives
// here rather than on cli.Version because the jail binary must not link
// internal/cli.
var Version = "dev"
