// Command kb is the Brainiac operator CLI — a thin cobra wrapper over
// internal/core for operators and automation/cron (SYSTEM.md §6.3).
package main

import "os"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// cobra already printed the error.
		os.Exit(1)
	}
}
