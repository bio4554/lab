// Command lab-agent is the agent-facing control CLI (send turns, list
// and spawn agents, report status). Phase 4 stub: it exists so stack
// images carry a real binary at /usr/local/bin/lab-agent; Phase 11
// replaces it with the real client.
package main

import "fmt"

// version is stamped via -ldflags at build time.
var version = "dev"

func main() {
	fmt.Printf("lab-agent %s (stub)\n", version)
}
