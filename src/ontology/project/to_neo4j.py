from tqdm import tqdm

from ontology.db import neo4j_driver, pg_conn

BATCH = 5000

ENTITY_QUERY = """
SELECT e.id::text, e.external_id, e.type, e.canonical_label, e.attrs,
       s.name AS source_name
FROM entity e
LEFT JOIN source s ON s.id = e.source_id
"""

RELATION_QUERY = """
SELECT r.id::text, r.src_entity_id::text, r.dst_entity_id::text, r.predicate, r.attrs
FROM relation r
"""

# Top-level keys in attrs become native node properties so they're queryable.
# Cypher list/scalar types only — nested objects are skipped.
UPSERT_ENTITIES = """
UNWIND $batch AS row
MERGE (e:Entity {id: row.id})
SET e.external_id     = row.external_id,
    e.type            = row.type,
    e.canonical_label = row.canonical_label,
    e.source          = row.source
SET e += row.props
WITH e, row
CALL apoc.create.addLabels(e, [row.type]) YIELD node
RETURN count(node)
"""

UPSERT_RELATIONS = """
UNWIND $batch AS row
MATCH (a:Entity {id: row.src})
MATCH (b:Entity {id: row.dst})
CALL apoc.merge.relationship(a, row.predicate, {id: row.id}, row.props, b, row.props)
YIELD rel
RETURN count(rel)
"""


def _flatten_props(attrs: dict | None) -> dict:
    """Keep only top-level scalar / list-of-scalar entries — what Neo4j accepts as properties."""
    if not attrs:
        return {}
    out: dict = {}
    for k, v in attrs.items():
        if v is None:
            continue
        if isinstance(v, (str, int, float, bool)):
            out[k] = v
        elif isinstance(v, list) and all(isinstance(x, (str, int, float, bool)) for x in v):
            out[k] = v
    return out


def _chunks(cur, n: int):
    while True:
        rows = cur.fetchmany(n)
        if not rows:
            break
        yield rows


def project() -> tuple[int, int]:
    """Read entities + relations from Postgres and project them into Neo4j."""
    from ontology.lineage import pipeline_run

    with pipeline_run("prescriber.project", actor="user:cli") as plr:
        e, r = _project_inner()
        plr.outputs.update({"entities": e, "relations": r})
        return e, r


def _project_inner() -> tuple[int, int]:
    driver = neo4j_driver()
    entities = 0
    relations = 0

    with pg_conn() as conn, driver.session() as session:
        with conn.cursor(name="entity_stream") as cur:
            cur.itersize = BATCH
            cur.execute(ENTITY_QUERY)
            bar = tqdm(desc="entities -> neo4j", unit="ent")
            for rows in _chunks(cur, BATCH):
                batch = [
                    {
                        "id": r[0],
                        "external_id": r[1],
                        "type": r[2],
                        "canonical_label": r[3],
                        "props": _flatten_props(r[4]),
                        "source": r[5],
                    }
                    for r in rows
                ]
                session.execute_write(lambda tx, b=batch: tx.run(UPSERT_ENTITIES, batch=b).consume())
                entities += len(batch)
                bar.update(len(batch))
            bar.close()

        with conn.cursor(name="rel_stream") as cur:
            cur.itersize = BATCH
            cur.execute(RELATION_QUERY)
            bar = tqdm(desc="relations -> neo4j", unit="rel")
            for rows in _chunks(cur, BATCH):
                batch = [
                    {
                        "id": r[0],
                        "src": r[1],
                        "dst": r[2],
                        "predicate": r[3],
                        "props": _flatten_props(r[4]),
                    }
                    for r in rows
                ]
                session.execute_write(lambda tx, b=batch: tx.run(UPSERT_RELATIONS, batch=b).consume())
                relations += len(batch)
                bar.update(len(batch))
            bar.close()

    return entities, relations
