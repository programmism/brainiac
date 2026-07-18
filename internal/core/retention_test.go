package core

import (
	"context"
	"testing"
	"time"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// TestSweepRetention: the retention pass purges aged historical rows but never
// current rows, and keeps a historical node still referenced by an edge (#363).
func TestSweepRetention(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	mk := func(name string) *model.Node {
		t.Helper()
		n := &model.Node{CanonicalName: name, Type: "thing"}
		if err := store.InsertNode(ctx, pool, n); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return n
	}
	histNode := func(id string) {
		t.Helper()
		if err := store.UpdateNodeStatus(ctx, pool, id, model.StatusHistorical); err != nil {
			t.Fatalf("retire node: %v", err)
		}
	}
	backdate := func(tbl, id string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE `+tbl+` SET superseded_at = now() - interval '400 days' WHERE id = $1`, id); err != nil {
			t.Fatalf("backdate %s: %v", tbl, err)
		}
	}
	exists := func(tbl, id string) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+tbl+` WHERE id = $1)`, id).Scan(&ok); err != nil {
			t.Fatalf("exists %s: %v", tbl, err)
		}
		return ok
	}

	// A: aged historical node with no edges → purged.
	a := mk("Aged")
	histNode(a.ID)
	backdate("nodes", a.ID)
	// Cur: current node → survives.
	cur := mk("Current")
	// D: aged historical node still referenced by a CURRENT edge → kept.
	d := mk("Referenced")
	src := mk("Source")
	refEdge := &model.Edge{FromID: src.ID, ToID: d.ID, Type: "mentions", Why: "keeps D pinned"}
	if err := store.InsertEdge(ctx, pool, refEdge); err != nil {
		t.Fatalf("insert ref edge: %v", err)
	}
	histNode(d.ID)
	backdate("nodes", d.ID)
	// E: aged historical edge between current nodes → purged.
	x, y := mk("X"), mk("Y")
	e := &model.Edge{FromID: x.ID, ToID: y.ID, Type: "old_rel", Why: "stale"}
	if err := store.InsertEdge(ctx, pool, e); err != nil {
		t.Fatalf("insert edge E: %v", err)
	}
	if _, err := store.UpdateEdgeStatus(ctx, pool, e.ID, model.StatusHistorical); err != nil {
		t.Fatalf("retire edge E: %v", err)
	}
	backdate("edges", e.ID)

	counts, err := c.SweepRetention(ctx, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if exists("nodes", a.ID) {
		t.Error("aged historical node A should be purged")
	}
	if !exists("nodes", cur.ID) {
		t.Error("current node wrongly purged")
	}
	if !exists("nodes", d.ID) {
		t.Error("historical node D still referenced by a current edge should be kept")
	}
	if exists("edges", e.ID) {
		t.Error("aged historical edge E should be purged")
	}
	if !exists("edges", refEdge.ID) {
		t.Error("current edge wrongly purged")
	}
	if counts.Nodes != 1 || counts.Edges != 1 {
		t.Fatalf("counts = %+v, want 1 node (A) + 1 edge (E)", counts)
	}

	// Disabled window is rejected.
	if _, err := c.SweepRetention(ctx, 0); err == nil {
		t.Fatal("SweepRetention(0) should error")
	}
}
