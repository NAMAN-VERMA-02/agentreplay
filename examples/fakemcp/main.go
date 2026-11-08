// fakemcp is the file-search MCP server the demo and benchmark record against.
package main

import (
	"fmt"
	"os"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/fake/mcpserver"
)

func main() {
	if err := mcpserver.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fakemcp:", err)
		os.Exit(1)
	}
}
