package remediation

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmc/langchaingo/schema"
	"github.com/tmc/langchaingo/vectorstores"
)

// DecisionCollection is the collection decisions are embedded into.
//
// Separate from the incidents rather than tagged alongside them, because the
// store's filters are equality-only: there is no way to say "not a decision",
// so decisions sharing the incident collection would silently take slots away
// from the incident history the agent recalls.
const DecisionCollection = "sre_decisions"

// the metadata key a document is deleted by. A decision that is replaced has to
// take its document with it, or the index keeps recalling a judgement that has
// since been reversed.
const decisionIDKey = "decision_id"

// MaxPrecedents is how many past decisions are put in front of a model at once.
//
// Small on purpose. These are opinions, and the more of them are quoted the
// more the current evidence has to argue against; four incidents and two
// precedents leaves the live fault as the loudest thing in the prompt.
const MaxPrecedents = 2

// Precedents is the decision index: the collection documents go into, and the
// pool needed to take one back out.
//
// Nil is a working value. Everything here degrades to doing nothing, the same
// way a missing container runtime or queue does — a decision that cannot be
// indexed is still recorded, and the pipeline is unchanged.
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

// Forget removes superseded decisions from the index.
//
// Done with SQL rather than through the store, which has no per-document
// delete. Parameterised on the ids: the store's own filter path interpolates
// values straight into the statement, which is safe for its constants and would
// not be for these.
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

// Recall returns the documents of the most similar past decisions.
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
