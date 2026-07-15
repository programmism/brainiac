package core

import (
	"context"
	"testing"
	"time"
)

func hasEdgeType(det *NodeDetail, typ string) bool {
	if det == nil {
		return false
	}
	for _, e := range det.Edges {
		if e.Edge.Type == typ {
			return true
		}
	}
	return false
}

func TestGetNodeAsOf(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	t0 := time.Now() // before the entity exists
	time.Sleep(15 * time.Millisecond)

	edge, err := c.Link(ctx, LinkInput{From: "App", Type: "writes_to", To: "Kafka", Why: "event bus"})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	asOfWhileLive := time.Now() // edge created, not yet retired
	time.Sleep(15 * time.Millisecond)

	if err := c.RetireEdge(ctx, edge.ID); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// Before the entity existed → not found.
	if det, err := c.GetNodeAsOf(ctx, "", "App", "", t0); err != nil || det != nil {
		t.Fatalf("as-of before creation must be nil: det=%v err=%v", det, err)
	}
	// While the edge was live → present.
	detLive, err := c.GetNodeAsOf(ctx, "", "App", "", asOfWhileLive)
	if err != nil {
		t.Fatalf("as-of live: %v", err)
	}
	if !hasEdgeType(detLive, "writes_to") {
		t.Fatalf("as-of-while-live should show the edge: %+v", detLive)
	}
	// As of now, after retirement → the edge is gone from the as-of view.
	detNow, err := c.GetNodeAsOf(ctx, "", "App", "", time.Now())
	if err != nil {
		t.Fatalf("as-of now: %v", err)
	}
	if hasEdgeType(detNow, "writes_to") {
		t.Fatalf("as-of-after-retire should hide the retired edge: %+v", detNow)
	}
	// The live GetNode still shows it (with history), so nothing was lost.
	if det, _ := c.GetNode(ctx, "", "App", ""); !hasEdgeType(det, "writes_to") {
		t.Fatalf("live get_node should still include the retired edge in history")
	}
}
