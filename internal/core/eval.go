package core

import "context"

// DefaultEvalK is the default cutoff for recall@k.
const DefaultEvalK = 8

// GoldenQuery is one evaluation case: a query and the source URIs a good answer
// must surface.
type GoldenQuery struct {
	Query           string   `json:"query"`
	ExpectedSources []string `json:"expected_sources"`
}

// QueryResult is the per-query outcome.
type QueryResult struct {
	Query    string  `json:"query"`
	Expected int     `json:"expected"`
	Found    int     `json:"found"`
	Hit      bool    `json:"hit"`    // ≥1 expected source in the top-k
	Recall   float64 `json:"recall"` // found / expected
}

// EvalResult aggregates a run (SYSTEM.md §9, PRD §18).
type EvalResult struct {
	K                int           `json:"k"`
	Queries          int           `json:"queries"`
	RecallAtK        float64       `json:"recall_at_k"`        // fraction of queries with ≥1 expected in top-k
	MeanSourceRecall float64       `json:"mean_source_recall"` // mean of per-query found/expected
	PerQuery         []QueryResult `json:"per_query"`
}

// Eval runs the golden query set through search and reports recall@k — the
// objective proof that retrieval quality holds across growth and model/threshold
// changes.
func (c *Core) Eval(ctx context.Context, golden []GoldenQuery, k int) (*EvalResult, error) {
	if k <= 0 {
		k = DefaultEvalK
	}
	res := &EvalResult{K: k, Queries: len(golden), PerQuery: make([]QueryResult, 0, len(golden))}
	if len(golden) == 0 {
		return res, nil
	}

	var hits int
	var sumRecall float64
	for _, g := range golden {
		found, err := c.foundSources(ctx, g, k)
		if err != nil {
			return nil, err
		}
		qr := QueryResult{Query: g.Query, Expected: len(g.ExpectedSources), Found: found, Hit: found > 0}
		if len(g.ExpectedSources) > 0 {
			qr.Recall = float64(found) / float64(len(g.ExpectedSources))
		}
		if qr.Hit {
			hits++
		}
		sumRecall += qr.Recall
		res.PerQuery = append(res.PerQuery, qr)
	}
	res.RecallAtK = float64(hits) / float64(len(golden))
	res.MeanSourceRecall = sumRecall / float64(len(golden))
	return res, nil
}

func (c *Core) foundSources(ctx context.Context, g GoldenQuery, k int) (int, error) {
	results, err := c.Search(ctx, g.Query, k, "", false) // eval spans all scopes, hot tier
	if err != nil {
		return 0, err
	}
	got := make(map[string]bool, len(results))
	for _, h := range results {
		got[h.SourceURI] = true
	}
	found := 0
	for _, want := range g.ExpectedSources {
		if got[want] {
			found++
		}
	}
	return found, nil
}
