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

	"github.com/programmism/brainiac/internal/config"
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

// runMigrate applies pending schema migrations, taking the DSN from the loaded
// config (which layers DATABASE_URL over config.yaml).
func runMigrate() error {
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := store.Connect(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	return store.Migrate(ctx, pool)
}

// configPath resolves the config file location: BRAINIAC_CONFIG or ./config.yaml.
func configPath() string {
	if p := os.Getenv("BRAINIAC_CONFIG"); p != "" {
		return p
	}
	return "config.yaml"
}
