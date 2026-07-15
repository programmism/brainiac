package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/programmism/brainiac/internal/store"
)

// Principal is a caller's hard-isolation identity (Layer 2, #120): the set of
// project namespaces it may READ and the single namespace all its WRITES are
// pinned to. It is derived from a bearer token (HTTP, per request) or from config
// (MCP, per process) and rides on the request context. A nil principal means
// Layer 1 — no wall, the pre-principal behavior.
//
// The global (shared) namespace is the empty string; a principal sees global only
// if "" (or the config alias "global") is in its Read set — global is not leaked
// by default.
type Principal struct {
	Name  string
	Read  []string // project namespaces this token may read ("" = global)
	Write string   // single namespace all writes are pinned to
	// MaxNodes / MaxChunks cap how many rows this principal's namespace may hold
	// (#186). 0 = unlimited. Enforced at write time against the live row count.
	MaxNodes  int
	MaxChunks int
}

// ErrForbiddenNamespace is returned when a caller tries to write into a namespace
// other than its principal's single write target.
var ErrForbiddenNamespace = errors.New("write outside principal's namespace")

// ErrQuotaExceeded is returned when a write would push a principal's namespace
// past its configured row quota (#186).
var ErrQuotaExceeded = errors.New("namespace quota exceeded")

// checkNodeQuota rejects a new-node write that would exceed the principal's node
// quota. No principal or a zero cap means unlimited. Counting uses the given db so
// it sees uncommitted rows inside a link transaction.
func checkNodeQuota(ctx context.Context, db store.DBTX) error {
	p := PrincipalFrom(ctx)
	if p == nil || p.MaxNodes == 0 {
		return nil
	}
	n, err := store.CountNodes(ctx, db, store.Namespaces([]string{p.Write}))
	if err != nil {
		return fmt.Errorf("node quota check: %w", err)
	}
	if n >= p.MaxNodes {
		return fmt.Errorf("%w: %s at %d/%d nodes", ErrQuotaExceeded, p.Write, n, p.MaxNodes)
	}
	return nil
}

// checkChunkQuota rejects an ingest that would push the principal's namespace past
// its chunk quota. adding is how many new chunks the caller is about to insert.
func checkChunkQuota(ctx context.Context, db store.DBTX, adding int) error {
	p := PrincipalFrom(ctx)
	if p == nil || p.MaxChunks == 0 {
		return nil
	}
	n, err := store.CountChunks(ctx, db, store.Namespaces([]string{p.Write}))
	if err != nil {
		return fmt.Errorf("chunk quota check: %w", err)
	}
	if n+adding > p.MaxChunks {
		return fmt.Errorf("%w: %s would reach %d/%d chunks", ErrQuotaExceeded, p.Write, n+adding, p.MaxChunks)
	}
	return nil
}

type principalKey struct{}

// WithPrincipal binds a principal to the context for downstream core enforcement.
// Passing nil is a no-op (Layer 1).
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the bound principal, or nil for a Layer 1 (unwalled) call.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// readScope resolves the effective read constraints for a query: the soft #119
// lens (a caller-chosen ?project= narrowing) plus the hard #120 wall (the
// principal's read ceiling, which a caller cannot widen past). With no principal
// the wall is off and behavior is exactly Layer 1.
func (c *Core) readScope(ctx context.Context, project string) (store.ScopeFilter, store.Wall) {
	soft := store.LensFor(project)
	p := PrincipalFrom(ctx)
	if p == nil {
		return soft, store.NoWall()
	}
	if project != "" {
		// A caller may narrow to a project only WITHIN its read-set; a request for
		// any other namespace sees nothing rather than falling back to Layer 1.
		if !contains(p.Read, project) {
			return soft, store.Namespaces([]string{})
		}
		return soft, store.Namespaces([]string{project})
	}
	// No narrowing: the wall is the principal's whole read-set.
	return soft, store.Namespaces(p.Read)
}

// pinWrite resolves the identity set a write lands in, given the caller's
// already-merged discriminators (adapters fold `project` into the map). With no
// principal it is a no-op — Layer 1. Under a principal the `project` axis is
// pinned to the principal's single Write namespace; a caller that named a
// different project is rejected loudly (ErrForbiddenNamespace) rather than
// silently redirected. Other identity axes (env, client, …) pass through.
func (c *Core) pinWrite(ctx context.Context, disc map[string]string) (map[string]string, error) {
	p := PrincipalFrom(ctx)
	if p == nil {
		return disc, nil
	}
	if proj := disc["project"]; proj != "" && proj != p.Write {
		return nil, ErrForbiddenNamespace
	}
	out := map[string]string{"project": p.Write}
	for k, v := range disc {
		if k != "project" {
			out[k] = v
		}
	}
	return out, nil
}

// visibleToPrincipal reports whether a single fetched node is inside the caller's
// read wall — the post-filter for by-id/by-name lookups (the row is fetched but
// withheld, so an id guess across the wall yields "not found", not a leak).
func (c *Core) visibleToPrincipal(ctx context.Context, disc map[string]string) bool {
	p := PrincipalFrom(ctx)
	if p == nil {
		return true
	}
	return contains(p.Read, disc["project"])
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
