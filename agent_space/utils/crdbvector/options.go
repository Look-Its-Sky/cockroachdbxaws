package crdbvector

import (
	"errors"
	"fmt"

	"github.com/tmc/langchaingo/embeddings"
)

const (
	DefaultCollectionName           = "langchain"
	DefaultPreDeleteCollection      = false
	DefaultEmbeddingStoreTableName  = "langchain_pg_embedding"
	DefaultCollectionStoreTableName = "langchain_pg_collection"
)

// ErrInvalidOptions is returned when the options given are invalid.
var ErrInvalidOptions = errors.New("invalid options")

// enables modification of client
type Option func(p *Store)

// specify the embedder to use for generating embeddings
func WithEmbedder(e embeddings.Embedder) Option {
	return func(p *Store) {
		p.embedder = e
	}
}

// specify whether to pre-delete the collection before creating it
func WithPreDeleteCollection(preDelete bool) Option {
	return func(p *Store) {
		p.preDeleteCollection = preDelete
	}
}

// specify collection name
func WithCollectionName(name string) Option {
	return func(p *Store) {
		p.collectionName = name
	}
}

// specify embedding table name
func WithEmbeddingTableName(name string) Option {
	return func(p *Store) {
		p.embeddingTableName = name
	}
}

// specify collection table name
func WithCollectionTableName(name string) Option {
	return func(p *Store) {
		p.collectionTableName = name
	}
}

// specify connection url
func WithConnectionURL(connectionURL string) Option {
	return func(p *Store) {
		p.connURL = connectionURL
	}
}

// specify active pgx connection
func WithConn(conn PGXConn) Option {
	return func(p *Store) {
		p.conn = conn
	}
}

// specify collection metadata
func WithCollectionMetadata(metadata map[string]any) Option {
	return func(p *Store) {
		p.collectionMetadata = metadata
	}
}

// specify vector dims
func WithVectorDimensions(size int) Option {
	return func(p *Store) {
		p.vectorDimensions = size
	}
}

// hnsw pgvector: m connections per layer (16), efConstruction candidate list size (64), distanceFunction (l2)
func WithHNSWIndex(m int, efConstruction int, distanceFunction string) Option {
	return func(p *Store) {
		p.hnswIndex = &HNSWIndex{
			m:                m,
			efConstruction:   efConstruction,
			distanceFunction: distanceFunction,
		}
	}
}

func applyClientOptions(opts ...Option) (Store, error) {
	o := &Store{
		collectionName:      DefaultCollectionName,
		preDeleteCollection: DefaultPreDeleteCollection,
		embeddingTableName:  DefaultEmbeddingStoreTableName,
		collectionTableName: DefaultCollectionStoreTableName,
	}

	for _, opt := range opts {
		opt(o)
	}

	if o.conn == nil && o.connURL == "" {
		return Store{}, fmt.Errorf("%w: missing postgres connection", ErrInvalidOptions)
	}

	if o.embedder == nil {
		return Store{}, fmt.Errorf("%w: missing embedder", ErrInvalidOptions)
	}

	return *o, nil
}
