package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// TestDeriveNodeTrust is a pure-logic check of the node-trust rule (#375): a node
// is untrusted only when it has ≥1 current edge and every current edge is
// untrusted; historical/proposed edges and the no-edge case don't make it
// untrusted.
func TestDeriveNodeTrust(t *testing.T) {
	ev := func(status model.Status, trust string) EdgeView {
		return EdgeView{Edge: model.Edge{Status: status, Trust: trust}}
	}
	cases := []struct {
		name  string
		edges []EdgeView
		want  string
	}{
		{"no edges", nil, model.TrustTrusted},
		{"all current untrusted", []EdgeView{ev(model.StatusCurrent, model.TrustUntrusted), ev(model.StatusCurrent, model.TrustUntrusted)}, model.TrustUntrusted},
		{"one current trusted", []EdgeView{ev(model.StatusCurrent, model.TrustUntrusted), ev(model.StatusCurrent, model.TrustTrusted)}, model.TrustTrusted},
		{"untrusted but historical", []EdgeView{ev(model.StatusHistorical, model.TrustUntrusted)}, model.TrustTrusted},
		{"current untrusted + historical trusted", []EdgeView{ev(model.StatusCurrent, model.TrustUntrusted), ev(model.StatusHistorical, model.TrustTrusted)}, model.TrustUntrusted},
	}
	for _, tc := range cases {
		if got := deriveNodeTrust(tc.edges); got != tc.want {
			t.Errorf("%s: deriveNodeTrust = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestGetNodeDerivedTrust checks the derived trust surfaces on GetNode (#375): an
// entity known only through an untrusted edge reads untrusted; adding a trusted
// current edge flips it to trusted.
func TestGetNodeDerivedTrust(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	var aID string
	err := store.WithTx(ctx, pool, func(db store.DBTX) error {
		a := &model.Node{CanonicalName: "OrderService", Type: "service"}
		b := &model.Node{CanonicalName: "Kafka", Type: "system"}
		if err := store.InsertNode(ctx, db, a); err != nil {
			return err
		}
		if err := store.InsertNode(ctx, db, b); err != nil {
			return err
		}
		aID = a.ID
		return store.InsertEdge(ctx, db, &model.Edge{
			FromID: a.ID, ToID: b.ID, Type: "writes_to", Why: "durability",
			SourceURI: "doc://u", Author: "x", Trust: model.TrustUntrusted,
		})
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	det, err := c.GetNode(ctx, aID, "", "")
	if err != nil || det == nil {
		t.Fatalf("get node: %v (nil=%v)", err, det == nil)
	}
	if det.Trust != model.TrustUntrusted {
		t.Fatalf("node trust = %q, want untrusted (only edge is untrusted)", det.Trust)
	}

	// Add a trusted current edge → the entity is now vouched for by trusted content.
	err = store.WithTx(ctx, pool, func(db store.DBTX) error {
		d := &model.Node{CanonicalName: "OrdersDB", Type: "datastore"}
		if err := store.InsertNode(ctx, db, d); err != nil {
			return err
		}
		return store.InsertEdge(ctx, db, &model.Edge{
			FromID: aID, ToID: d.ID, Type: "persists_to", Why: "state",
			SourceURI: "doc://t", Author: "x", Trust: model.TrustTrusted,
		})
	})
	if err != nil {
		t.Fatalf("add trusted edge: %v", err)
	}
	det, err = c.GetNode(ctx, aID, "", "")
	if err != nil {
		t.Fatalf("get node 2: %v", err)
	}
	if det.Trust != model.TrustTrusted {
		t.Fatalf("node trust = %q, want trusted (has a trusted current edge)", det.Trust)
	}
}

// TestPerCallTrust checks the opt-in per-call trust on IngestTextWithTrust (#375):
// a client can mark a specific document trusted; an invalid value is rejected; and
// the plain IngestText stays untrusted (fail-closed).
func TestPerCallTrust(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	trustOf := func(uri string) string {
		t.Helper()
		var tr string
		if err := pool.QueryRow(ctx, `SELECT trust FROM chunks WHERE source_uri = $1 LIMIT 1`, uri).Scan(&tr); err != nil {
			t.Fatalf("read trust %s: %v", uri, err)
		}
		return tr
	}

	if _, err := c.IngestTextWithTrust(ctx, "doc://vouched", "OrderService streams events to Kafka for durability and audit.", "", model.TrustTrusted); err != nil {
		t.Fatalf("ingest trusted: %v", err)
	}
	if got := trustOf("doc://vouched"); got != model.TrustTrusted {
		t.Fatalf("per-call trusted chunk trust = %q, want trusted", got)
	}

	if _, err := c.IngestText(ctx, "doc://plain", "BillingService reconciles invoices against the ledger nightly.", ""); err != nil {
		t.Fatalf("ingest plain: %v", err)
	}
	if got := trustOf("doc://plain"); got != model.TrustUntrusted {
		t.Fatalf("plain IngestText chunk trust = %q, want untrusted (fail-closed)", got)
	}

	if _, err := c.IngestTextWithTrust(ctx, "doc://bad", "text", "", "maybe"); err == nil {
		t.Fatal("invalid trust value should be rejected")
	}
}
