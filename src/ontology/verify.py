from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass

from ontology.db import neo4j_driver, pg_conn


@dataclass
class CheckResult:
    name: str
    passed: bool
    detail: str
    fatal: bool = False

    def line(self) -> str:
        tag = "PASS" if self.passed else "FAIL"
        return f"[{tag}] {self.name}: {self.detail}"


# ── Postgres checks ───────────────────────────────────────────────────────────

def pg_has_data() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute("SELECT count(*) FROM source")
        sources = cur.fetchone()[0]
        cur.execute("SELECT count(*) FROM entity")
        entities = cur.fetchone()[0]
        cur.execute("SELECT count(*) FROM relation")
        relations = cur.fetchone()[0]
    ok = sources > 0 and entities > 0 and relations > 0
    return CheckResult(
        "pg_has_data",
        ok,
        f"{sources} source(s), {entities:,} entities, {relations:,} relations",
        fatal=not ok,
    )


def pg_entity_counts() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute("SELECT type, count(*) FROM entity GROUP BY type ORDER BY 2 DESC")
        rows = cur.fetchall()
    detail = ", ".join(f"{t}={n:,}" for t, n in rows) or "(none)"
    return CheckResult("pg_entity_counts_by_type", bool(rows), detail)


def pg_relation_counts() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute("SELECT predicate, count(*) FROM relation GROUP BY predicate ORDER BY 2 DESC")
        rows = cur.fetchall()
    detail = ", ".join(f"{p}={n:,}" for p, n in rows) or "(none)"
    return CheckResult("pg_relation_counts_by_predicate", bool(rows), detail)


def pg_no_null_labels() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT count(*) FROM entity WHERE canonical_label IS NULL OR canonical_label = ''"
        )
        n = cur.fetchone()[0]
    return CheckResult("pg_no_null_labels", n == 0, f"{n} entities with null/empty label")


def pg_no_orphan_relations() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute(
            """
            SELECT count(*) FROM relation r
            LEFT JOIN entity s ON s.id = r.src_entity_id
            LEFT JOIN entity d ON d.id = r.dst_entity_id
            WHERE s.id IS NULL OR d.id IS NULL
            """
        )
        n = cur.fetchone()[0]
    return CheckResult("pg_no_orphan_relations", n == 0, f"{n} dangling relations")


def pg_no_duplicate_external_ids() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute(
            """
            SELECT count(*) FROM (
                SELECT source_id, external_id, count(*)
                FROM entity GROUP BY source_id, external_id HAVING count(*) > 1
            ) d
            """
        )
        n = cur.fetchone()[0]
    return CheckResult("pg_no_duplicate_external_ids", n == 0, f"{n} duplicate keys")


# ── Neo4j checks ──────────────────────────────────────────────────────────────

def neo_has_data() -> CheckResult:
    with neo4j_driver().session() as s:
        n = s.run("MATCH (n) RETURN count(n) AS c").single()["c"]
        r = s.run("MATCH ()-[r]->() RETURN count(r) AS c").single()["c"]
    ok = n > 0 and r > 0
    return CheckResult(
        "neo_has_data", ok, f"{n:,} nodes, {r:,} relationships", fatal=not ok
    )


def neo_every_node_has_entity_label() -> CheckResult:
    with neo4j_driver().session() as s:
        n = s.run("MATCH (n) WHERE NOT n:Entity RETURN count(n) AS c").single()["c"]
    return CheckResult("neo_every_node_has_entity_label", n == 0, f"{n} nodes missing :Entity")


def neo_every_node_has_canonical_label() -> CheckResult:
    with neo4j_driver().session() as s:
        n = s.run(
            "MATCH (n:Entity) WHERE n.canonical_label IS NULL RETURN count(n) AS c"
        ).single()["c"]
    return CheckResult(
        "neo_every_node_has_canonical_label", n == 0, f"{n} :Entity nodes missing canonical_label"
    )


def neo_constraint_present() -> CheckResult:
    with neo4j_driver().session() as s:
        rows = list(s.run("SHOW CONSTRAINTS YIELD name RETURN name"))
    names = {row["name"] for row in rows}
    ok = "entity_id_unique" in names
    return CheckResult(
        "neo_uniqueness_constraint_present",
        ok,
        f"constraints={sorted(names)}",
    )


# ── Cross-store checks ────────────────────────────────────────────────────────

def cross_entity_count_matches() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute("SELECT count(*) FROM entity")
        pg = cur.fetchone()[0]
    with neo4j_driver().session() as s:
        neo = s.run("MATCH (n:Entity) RETURN count(n) AS c").single()["c"]
    return CheckResult("cross_entity_count_matches", pg == neo, f"PG={pg:,}  Neo4j={neo:,}")


def cross_relation_count_matches() -> CheckResult:
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute("SELECT count(*) FROM relation")
        pg = cur.fetchone()[0]
    with neo4j_driver().session() as s:
        neo = s.run("MATCH ()-[r]->() RETURN count(r) AS c").single()["c"]
    return CheckResult("cross_relation_count_matches", pg == neo, f"PG={pg:,}  Neo4j={neo:,}")


def cross_sample_consistency(sample_size: int = 20) -> CheckResult:
    """Pick N random entities and verify label + out-degree match in both stores."""
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute(
            f"""
            WITH s AS (SELECT id, external_id, type, canonical_label FROM entity ORDER BY random() LIMIT {sample_size})
            SELECT s.external_id, s.type, s.canonical_label,
                   (SELECT count(*) FROM relation r WHERE r.src_entity_id = s.id) AS out_deg
            FROM s
            """
        )
        sample = cur.fetchall()
    if not sample:
        return CheckResult("cross_sample_consistency", False, "no entities to sample")

    mismatches: list[str] = []
    with neo4j_driver().session() as s:
        for ext_id, etype, label, out_deg in sample:
            row = s.run(
                """
                MATCH (n:Entity {external_id: $ext, type: $type})
                OPTIONAL MATCH (n)-[r]->()
                RETURN n.canonical_label AS label, count(r) AS out_deg
                """,
                ext=ext_id,
                type=etype,
            ).single()
            if row is None:
                mismatches.append(f"{etype}:{ext_id} missing in Neo4j")
                continue
            if row["label"] != label:
                mismatches.append(f"{etype}:{ext_id} label PG={label!r} Neo={row['label']!r}")
            if row["out_deg"] != out_deg:
                mismatches.append(
                    f"{etype}:{ext_id} out_degree PG={out_deg} Neo={row['out_deg']}"
                )
    ok = not mismatches
    return CheckResult(
        "cross_sample_consistency",
        ok,
        f"sampled {len(sample)} entities; "
        + ("all match" if ok else f"{len(mismatches)} mismatch(es): " + "; ".join(mismatches[:3])),
    )


# ── Runner ────────────────────────────────────────────────────────────────────

POSTGRES_CHECKS: list[Callable[[], CheckResult]] = [
    pg_has_data,
    pg_entity_counts,
    pg_relation_counts,
    pg_no_null_labels,
    pg_no_orphan_relations,
    pg_no_duplicate_external_ids,
]

NEO4J_CHECKS: list[Callable[[], CheckResult]] = [
    neo_has_data,
    neo_every_node_has_entity_label,
    neo_every_node_has_canonical_label,
    neo_constraint_present,
]

CROSS_CHECKS: list[Callable[[], CheckResult]] = [
    cross_entity_count_matches,
    cross_relation_count_matches,
    cross_sample_consistency,
]


def run_all(scope: str = "all") -> list[CheckResult]:
    funcs: list[Callable[[], CheckResult]] = []
    if scope in ("all", "postgres"):
        funcs += POSTGRES_CHECKS
    if scope in ("all", "neo4j"):
        funcs += NEO4J_CHECKS
    if scope == "all":
        funcs += CROSS_CHECKS

    results: list[CheckResult] = []
    for fn in funcs:
        try:
            results.append(fn())
        except Exception as exc:
            results.append(CheckResult(fn.__name__, False, f"error: {exc}", fatal=True))
    return results
