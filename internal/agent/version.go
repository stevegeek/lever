package agent

// Version is the lever-agent build version advertised in the capability MCP
// server's serverInfo, stamped by the Makefile with -ldflags -X. It is not
// cli.Version because the jail binary must not link internal/cli.
var Version = "dev"
