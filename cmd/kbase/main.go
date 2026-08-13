// Command kbase is the agent-facing knowledge-base CLI. Phase 4 stub:
// it exists so stack images carry a real binary at /usr/local/bin/kbase;
// Phase 9 replaces it with the real client.
package main

import "fmt"

// version is stamped via -ldflags at build time.
var version = "dev"

func main() {
	fmt.Printf("kbase %s (stub)\n", version)
}
