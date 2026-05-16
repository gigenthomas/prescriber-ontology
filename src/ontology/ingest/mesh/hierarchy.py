from tqdm import tqdm

from ontology.db import pg_conn

# MeSH tree numbers encode hierarchy: "C01.234.567" is a child of "C01.234".
# After all descriptors are loaded, we materialise broader_descriptor edges.

BUILD_SQL = """
WITH expanded AS (
    SELECT
        e.id            AS entity_id,
        e.source_id     AS source_id,
        tn.tree_number  AS tn,
        regexp_replace(tn.tree_number, '\\.[^.]*$', '') AS parent_tn
    FROM entity e
    CROSS JOIN LATERAL jsonb_array_elements_text(e.attrs->'tree_numbers') AS tn(tree_number)
    WHERE e.type = 'Descriptor'
      AND e.source_id = %(source_id)s
),
parent_match AS (
    SELECT
        c.entity_id AS child_id,
        p.entity_id AS parent_id,
        c.source_id AS source_id
    FROM expanded c
    JOIN expanded p
      ON c.parent_tn = p.tn
     AND c.source_id = p.source_id
    WHERE c.tn <> c.parent_tn
)
INSERT INTO relation (src_entity_id, dst_entity_id, predicate, source_id)
SELECT DISTINCT child_id, parent_id, 'broader_descriptor', source_id
FROM parent_match
ON CONFLICT DO NOTHING;
"""


def build_hierarchy(year: int) -> int:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute("SELECT id FROM source WHERE name = %s", (f"MeSH-{year}",))
        row = cur.fetchone()
        if row is None:
            raise RuntimeError(f"No source MeSH-{year}; run loader first")
        source_id = row[0]

        with tqdm(desc="hierarchy", total=1) as bar:
            cur.execute(BUILD_SQL, {"source_id": source_id})
            inserted = cur.rowcount
            bar.update(1)
        conn.commit()
    return inserted
