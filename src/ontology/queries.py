from pathlib import Path
from typing import Any

from ontology.db import neo4j_driver

QUERY_DIR = Path("queries")


def list_queries() -> list[str]:
    return sorted(p.stem for p in QUERY_DIR.glob("*.cypher"))


def load_query(name: str) -> str:
    path = QUERY_DIR / f"{name}.cypher"
    if not path.exists():
        raise FileNotFoundError(f"No query {name!r} in {QUERY_DIR}/")
    return path.read_text()


def run_query(name: str, **params: Any) -> list[dict[str, Any]]:
    cypher = load_query(name)
    with neo4j_driver().session() as session:
        result = session.run(cypher, **params)
        return [dict(record) for record in result]
