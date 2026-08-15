-- crdbvector deliberately skips CREATE EXTENSION, because CockroachDB ships the
-- vector type built in and errors if you try (see
-- utils/crdbvector/pgvector.go: createVectorExtensionIfNotExists).
--
-- Plain PostgreSQL does need the extension, so it is created here at initdb
-- time instead. Everything else the store emits — vector(N), the <=> cosine
-- operator, vector_dims(), USING hnsw — is stock pgvector and needs no changes.
CREATE EXTENSION IF NOT EXISTS vector;
