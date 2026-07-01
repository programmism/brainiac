// Command kb is the Brainiac operator CLI — a thin adapter over internal/core
// and internal/store.
//
// The full command set is restructured onto cobra in issue #16. For now it
// wires the essentials (version, migrate) to real code so the binary is useful
// from day one.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/programmism/brainiac/internal/core"
	"github.com/programmism/brainiac/internal/store"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && (args[0] == "version" || args[0] == "--version"):
		fmt.Printf("kb %s\n", core.Version)
	case len(args) > 0 && args[0] == "migrate":
		if err := runMigrate(); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
		fmt.Println("migrations applied")
	default:
		fmt.Printf("kb %s — Brainiac CLI (full command set lands in #16)\n", core.Version)
	}
}

// runMigrate applies pending schema migrations. DSN comes from DATABASE_URL for
// now; the typed config loader (#5) becomes the source of truth later.
func runMigrate() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return store.Migrate(ctx, pool)
}
