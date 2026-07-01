// Command brainiac-mcp is the MCP server exposing core operations as tools to
// Claude — a thin adapter over internal/core.
//
// The tool definitions (official Go MCP SDK) are built in issue #15; this
// scaffold exists so the binary and its wiring are in place.
package main

import (
	"fmt"

	"github.com/programmism/brainiac/internal/core"
)

func main() {
	fmt.Printf("brainiac-mcp %s — MCP server (tools land in #15)\n", core.Version)
}
