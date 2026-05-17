"""OpenAI embeddings for entity resolution.

Reads every entity from Postgres, builds a short embedding-friendly text
representation, sends it to the OpenAI embeddings API in batches, and
writes the resulting vectors back to `entity.embedding`.

Designed to be re-run safely: only rows where `embedding IS NULL` are
processed unless `force=True`. The exact text we embedded is stored in
`entity.embedding_text` so we can audit or regenerate selectively.

Provider is OpenAI by default (text-embedding-3-small, 1536-dim). To
swap providers, replace `_embed_batch` — the schema doesn't change.
"""

from __future__ import annotations

import os
from typing import Iterator

from openai import OpenAI
from tqdm import tqdm

from ontology.db import pg_conn

DEFAULT_MODEL = "text-embedding-3-small"
DEFAULT_DIMENSIONS = 1536
DEFAULT_BATCH_SIZE = 1000   # OpenAI accepts up to 2048 per request


def _client() -> OpenAI:
    if not os.getenv("OPENAI_API_KEY"):
        raise RuntimeError(
            "OPENAI_API_KEY not set. Add it to .env or set in shell before running 'ontology embed'."
        )
    return OpenAI()


# ── Text builder ───────────────────────────────────────────────────────────

def entity_text(
    entity_type: str,
    canonical_label: str,
    attrs: dict | None,
    generic_label: str | None = None,
) -> str:
    """Build a short, embedding-friendly description of an entity.

    The text is what determines retrieval quality. Include the type prefix
    (so "Cardiology" the Specialty doesn't collide with "Cardiology" appearing
    in a Prescriber name) and the most distinguishing attrs.
    """
    a = attrs or {}
    if entity_type == "Prescriber":
        bits = [f"Prescriber: {canonical_label}"]
        spec = a.get("specialty")
        city = a.get("city")
        state = a.get("state")
        if spec:
            bits.append(f"specialty: {spec}")
        if city and state:
            bits.append(f"practices in {city}, {state}")
        elif city:
            bits.append(f"practices in {city}")
        return "; ".join(bits)

    if entity_type == "Drug":
        if generic_label:
            return f"Brand drug: {canonical_label} (generic substance: {generic_label})"
        return f"Brand drug: {canonical_label}"

    if entity_type == "GenericDrug":
        return f"Generic drug substance: {canonical_label}"

    if entity_type == "Specialty":
        return f"Medical prescriber specialty: {canonical_label}"

    if entity_type == "Location":
        # canonical_label is already "City, ST" for Location entities.
        return f"City of practice: {canonical_label}"

    return f"{entity_type}: {canonical_label}"


# ── Fetcher ────────────────────────────────────────────────────────────────

def _fetch_pending(force: bool, limit: int | None) -> Iterator[tuple]:
    """Yield (id, type, canonical_label, attrs, generic_label) for entities
    that still need embedding (or all of them, if force=True)."""
    where = "" if force else "WHERE e.embedding IS NULL"
    lim = f"LIMIT {int(limit)}" if limit else ""

    sql = f"""
        SELECT
            e.id::text,
            e.type,
            e.canonical_label,
            e.attrs,
            (
                SELECT g.canonical_label
                FROM relation r
                JOIN entity g ON g.id = r.dst_entity_id
                WHERE r.src_entity_id = e.id
                  AND r.predicate     = 'generic_of'
                LIMIT 1
            ) AS generic_label
        FROM entity e
        {where}
        ORDER BY e.id
        {lim}
    """
    with pg_conn() as conn, conn.cursor(name="embed_cursor") as cur:
        cur.itersize = 1000
        cur.execute(sql)
        for row in cur:
            yield row


# ── OpenAI call ────────────────────────────────────────────────────────────

def _embed_batch(client: OpenAI, texts: list[str], model: str) -> list[list[float]]:
    resp = client.embeddings.create(model=model, input=texts)
    return [d.embedding for d in resp.data]


# ── Driver ─────────────────────────────────────────────────────────────────

def backfill(
    *,
    force: bool = False,
    limit: int | None = None,
    batch_size: int = DEFAULT_BATCH_SIZE,
    model: str = DEFAULT_MODEL,
) -> int:
    """Embed pending entities. Returns count embedded."""
    client = _client()
    model_label = f"openai:{model}"

    buf_ids: list[str] = []
    buf_texts: list[str] = []
    total = 0

    bar = tqdm(desc=f"embedding ({model})", unit="entity")
    for row in _fetch_pending(force=force, limit=limit):
        entity_id, etype, label, attrs, generic_label = row
        text = entity_text(etype, label, attrs, generic_label)
        buf_ids.append(entity_id)
        buf_texts.append(text)
        if len(buf_ids) >= batch_size:
            total += _flush(client, model, model_label, buf_ids, buf_texts)
            bar.update(len(buf_ids))
            buf_ids.clear()
            buf_texts.clear()
    if buf_ids:
        total += _flush(client, model, model_label, buf_ids, buf_texts)
        bar.update(len(buf_ids))
    bar.close()
    return total


def _flush(
    client: OpenAI,
    model: str,
    model_label: str,
    ids: list[str],
    texts: list[str],
) -> int:
    vectors = _embed_batch(client, texts, model)

    # Build the per-row update payload. psycopg's executemany handles vector
    # type via the string form '[1.0, 2.0, ...]'.
    rows = [
        (_to_vector_literal(v), text, model_label, entity_id)
        for entity_id, text, v in zip(ids, texts, vectors, strict=True)
    ]
    with pg_conn() as conn, conn.cursor() as cur:
        cur.executemany(
            """
            UPDATE entity
            SET embedding       = %s::vector,
                embedding_text  = %s,
                embedding_model = %s
            WHERE id = %s::uuid
            """,
            rows,
        )
        conn.commit()
    return len(rows)


def _to_vector_literal(values: list[float]) -> str:
    # pgvector accepts '[v1, v2, ...]' as text input. Format with limited
    # precision to keep the literal short.
    return "[" + ",".join(f"{v:.6f}" for v in values) + "]"


def stats() -> dict[str, int]:
    """Quick check: how many entities have/lack embeddings."""
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute(
            """
            SELECT
                type,
                count(*)                              AS total,
                count(*) FILTER (WHERE embedding IS NOT NULL) AS embedded,
                count(*) FILTER (WHERE embedding IS NULL)     AS pending
            FROM entity
            GROUP BY type
            ORDER BY total DESC
            """
        )
        return [
            {"type": t, "total": total, "embedded": embedded, "pending": pending}
            for t, total, embedded, pending in cur.fetchall()
        ]
