from collections.abc import Iterable
from pathlib import Path

from psycopg.types.json import Jsonb
from tqdm import tqdm

from ontology.db import pg_conn
from ontology.ingest.mesh.parse import MeshDescriptor, iter_descriptors

BATCH = 1000

SCHEMA_TERMS = [
    ("entity_type", "Descriptor", "MeSH descriptor record"),
    ("predicate", "broader_descriptor", "Tree-parent relationship between MeSH descriptors"),
    ("attribute", "tree_numbers", "List of MeSH tree numbers locating the descriptor"),
    ("attribute", "scope_note", "Free-text scope note"),
]


def _ensure_source(conn, year: int) -> str:
    with conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO source (name, uri, version)
            VALUES (%s, %s, %s)
            ON CONFLICT (name) DO UPDATE SET version = EXCLUDED.version
            RETURNING id
            """,
            (f"MeSH-{year}", "https://www.nlm.nih.gov/mesh/", str(year)),
        )
        return cur.fetchone()[0]


def _ensure_schema_terms(conn) -> None:
    with conn.cursor() as cur:
        cur.executemany(
            """
            INSERT INTO schema_term (kind, name, description)
            VALUES (%s, %s, %s)
            ON CONFLICT (kind, name) DO NOTHING
            """,
            SCHEMA_TERMS,
        )


def _upsert_entities(conn, source_id: str, batch: list[MeshDescriptor]) -> None:
    rows = [
        (
            source_id,
            d.ui,
            "Descriptor",
            d.name,
            Jsonb(
                {
                    "tree_numbers": d.tree_numbers,
                    "scope_note": d.scope_note,
                }
            ),
        )
        for d in batch
    ]
    with conn.cursor() as cur:
        cur.executemany(
            """
            INSERT INTO entity (source_id, external_id, type, canonical_label, attrs)
            VALUES (%s, %s, %s, %s, %s)
            ON CONFLICT (source_id, external_id)
            DO UPDATE SET
                canonical_label = EXCLUDED.canonical_label,
                attrs           = EXCLUDED.attrs,
                updated_at      = now(),
                version         = entity.version + 1
            """,
            rows,
        )


def _upsert_synonyms(conn, source_id: str, batch: list[MeshDescriptor]) -> None:
    rows = []
    for d in batch:
        for syn in d.synonyms:
            rows.append((source_id, d.ui, syn))
    if not rows:
        return
    with conn.cursor() as cur:
        cur.executemany(
            """
            INSERT INTO entity_label (entity_id, label, label_type)
            SELECT e.id, %s, 'synonym'
            FROM entity e
            WHERE e.source_id = %s AND e.external_id = %s
            ON CONFLICT DO NOTHING
            """,
            [(syn, sid, ui) for sid, ui, syn in rows],
        )


def _batched(it: Iterable[MeshDescriptor], n: int) -> Iterable[list[MeshDescriptor]]:
    buf: list[MeshDescriptor] = []
    for x in it:
        buf.append(x)
        if len(buf) >= n:
            yield buf
            buf = []
    if buf:
        yield buf


def load_descriptors(xml_path: Path, year: int) -> int:
    total = 0
    with pg_conn() as conn:
        _ensure_schema_terms(conn)
        source_id = _ensure_source(conn, year)
        conn.commit()

        bar = tqdm(desc="descriptors", unit="rec")
        for batch in _batched(iter_descriptors(xml_path), BATCH):
            _upsert_entities(conn, source_id, batch)
            _upsert_synonyms(conn, source_id, batch)
            conn.commit()
            total += len(batch)
            bar.update(len(batch))
        bar.close()
    return total
