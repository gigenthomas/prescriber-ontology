-- pgvector: semantic embeddings for entity resolution.
--
-- An additional column on `entity` holds a 1536-dimensional embedding
-- (OpenAI text-embedding-3-small dimension). HNSW index for fast
-- approximate nearest-neighbor search via cosine distance.

CREATE EXTENSION IF NOT EXISTS vector;

ALTER TABLE entity
    ADD COLUMN embedding        vector(1536),
    ADD COLUMN embedding_text   TEXT,             -- the exact string we embedded (for audit/regen)
    ADD COLUMN embedding_model  TEXT;             -- e.g. 'openai:text-embedding-3-small'

-- HNSW index for cosine distance ( <=> operator ).
-- Settings: m=16, ef_construction=64 — solid defaults; tune later if recall is off.
CREATE INDEX entity_embedding_hnsw ON entity
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- Track which entities still need embedding (NULL = pending).
CREATE INDEX entity_embedding_missing
    ON entity (id)
    WHERE embedding IS NULL;
