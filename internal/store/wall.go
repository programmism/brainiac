package store

import "fmt"

// Wall is the hard per-principal visibility wall (Layer 2 isolation, #120). It is
// applied IN ADDITION to the soft ScopeFilter lens (#119): the lens is a
// caller-chosen narrowing, the wall is a deployment-enforced ceiling a caller
// cannot widen past.
//
// A namespace is a value of the `project` identity axis. NoWall (the zero value)
// means Layer 1 — no wall, every read behaves exactly as before principals
// existed. A wall that is "on" restricts reads to rows whose project is in the
// allowed set; an empty allowed set sees nothing, and "" in the set admits
// global (empty-discriminator) rows.
type Wall struct {
	on bool
	ns []string
}

// NoWall is the Layer 1 posture: no hard wall (reads span whatever the soft lens
// allows). It is the zero value so a caller that forgets a wall gets Layer 1, and
// only an explicit principal narrows visibility.
func NoWall() Wall { return Wall{} }

// Namespaces builds an active wall over the given project namespaces (an "" entry
// admits global). A nil/empty set is a live wall that admits nothing — used when
// a caller requests a project outside its read-set, which must return zero rows
// rather than fall back to Layer 1.
func Namespaces(ns []string) Wall {
	if ns == nil {
		ns = []string{}
	}
	return Wall{on: true, ns: ns}
}

// arg is the SQL bind value: Go nil (SQL NULL) when off so `$n IS NULL`
// short-circuits the clause; a non-nil (possibly empty) []string otherwise so
// `= ANY($n)` matches the allowed projects (or nothing for an empty set).
func (w Wall) arg() any {
	if !w.on {
		return nil
	}
	return w.ns
}

// projectClause is the wall predicate for a table whose discriminators jsonb is
// reachable via the given alias prefix (e.g. "" for the base table, "n." for a
// join). paramIdx is the $n position of the wall text[] argument. A NULL argument
// (Layer 1) disables the clause.
func projectClause(alias string, paramIdx int) string {
	return fmt.Sprintf(
		`($%d::text[] IS NULL OR COALESCE(NULLIF(%sdiscriminators->>'project',''),'') = ANY($%d::text[]))`,
		paramIdx, alias, paramIdx,
	)
}

// edgeEndpointsClause is the wall predicate for an edge row aliased `e`: both
// endpoint nodes must be inside the wall, so an edge that crosses the wall (e.g. a
// global node linked to a project node) is hidden from a principal that cannot see
// both ends. paramIdx is the $n position of the wall text[] argument.
func edgeEndpointsClause(paramIdx int) string {
	return fmt.Sprintf(`($%d::text[] IS NULL OR (
		EXISTS (SELECT 1 FROM nodes nf WHERE nf.id = e.from_id
		        AND COALESCE(NULLIF(nf.discriminators->>'project',''),'') = ANY($%d::text[]))
	AND EXISTS (SELECT 1 FROM nodes nt WHERE nt.id = e.to_id
		        AND COALESCE(NULLIF(nt.discriminators->>'project',''),'') = ANY($%d::text[]))))`,
		paramIdx, paramIdx, paramIdx)
}
