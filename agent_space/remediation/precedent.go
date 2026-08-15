package remediation

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmc/langchaingo/schema"
	"github.com/tmc/langchaingo/vectorstores"
)

// separate from the incidents rather than tagged alongside them: the store's
// filters are equality-only, so decisions sharing a collection would silently
// take recall slots from the incident history
const DecisionCollection = "sre_decisions"

// the metadata key a document is deleted by, so a replaced decision takes its
// document with it rather than being recalled after it was reversed
const decisionIDKey = "decision_id"

// how many past decisions go in front of a model at once, small on purpose:
// these are opinions, and the live fault has to stay the loudest thing there
const MaxPrecedents = 2

// the decision index: the collection documents go into and the pool to take one
// back out. Nil is a working value; an unindexed decision is still recorded.
type Precedents struct {
	store vectorstores.VectorStore
	pool  *pgxpool.Pool
	table string
}

func NewPrecedents(store vectorstores.VectorStore, pool *pgxpool.Pool, embeddingTable string) *Precedents {
	if store == nil || pool == nil {
		return nil
	}
	return &Precedents{store: store, pool: pool, table: embeddingTable}
}

// Add embeds a decision so future incidents can recall it.
func (p *Precedents) Add(ctx context.Context, d Decision) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(d.Document) == "" {
		return fmt.Errorf("remediation: decision %s has no document to index", d.ID)
	}

	_, err := p.store.AddDocuments(ctx, []schema.Document{{
		PageContent: d.Document,
		Metadata: map[string]any{
			"source":           "decision",
			decisionIDKey:      d.ID,
			"investigation_id": d.InvestigationID,
			"service_id":       d.ServiceID,
		},
	}})
	if err != nil {
		return fmt.Errorf("remediation: index decision %s: %w", d.ID, err)
	}
	return nil
}

// removes superseded decisions from the index, in SQL because the store has no
// per-document delete; parameterised, unlike the store's interpolating filter
func (p *Precedents) Forget(ctx context.Context, decisionIDs []string) error {
	if p == nil || len(decisionIDs) == 0 {
		return nil
	}

	q := `DELETE FROM ` + p.table + ` WHERE cmetadata->>'` + decisionIDKey + `' = ANY($1)`
	if _, err := p.pool.Exec(ctx, q, decisionIDs); err != nil {
		return fmt.Errorf("remediation: forget decisions: %w", err)
	}
	return nil
}

// the documents of the most similar past decisions
func (p *Precedents) Recall(ctx context.Context, query string, limit int) ([]string, error) {
	if p == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = MaxPrecedents
	}

	docs, err := p.store.SimilaritySearch(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("remediation: recall decisions: %w", err)
	}

	out := make([]string, 0, len(docs))
	for _, d := range docs {
		if s := strings.TrimSpace(d.PageContent); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}
