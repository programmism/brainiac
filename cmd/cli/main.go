// Command kb is the Brainiac operator CLI — a thin adapter over internal/core.
//
// The full command set (migrate, health, import, refresh, consolidate,
// reembed) is built on cobra in issue #16. This scaffold prints the version so
// the module builds and ships a real binary from day one.
package main

import (
	"fmt"
	"os"

	"github.com/programmism/brainiac/internal/core"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("kb %s\n", core.Version)
		return
	}
	fmt.Printf("kb %s — Brainiac CLI (commands land in #16)\n", core.Version)
}
