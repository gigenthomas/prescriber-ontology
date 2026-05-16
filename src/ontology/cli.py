import json
from pathlib import Path

import typer

from ontology.config import settings
from ontology.db import close_all, neo4j_driver, pg_conn

app = typer.Typer(help="Hybrid Postgres + Neo4j ontology pipeline.")


@app.command()
def init() -> None:
    """Apply Neo4j constraints."""
    cypher_file = Path("db/neo4j/init/constraints.cypher")
    statements = [s.strip() for s in cypher_file.read_text().split(";") if s.strip()]
    with neo4j_driver().session() as session:
        for stmt in statements:
            session.run(stmt)
    typer.echo(f"applied {len(statements)} Neo4j constraints")


@app.command()
def reset(
    yes: bool = typer.Option(False, "--yes", "-y", help="Skip confirmation"),
) -> None:
    """Wipe all ontology data from both stores. Schema and constraints survive."""
    if not yes:
        typer.confirm("Wipe all data from Postgres and Neo4j?", abort=True)
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute(
            "TRUNCATE entity, entity_label, relation, source, schema_term, "
            "projection_log RESTART IDENTITY CASCADE"
        )
        cur.execute("DROP TABLE IF EXISTS mup_dpr_staging")
        conn.commit()
    with neo4j_driver().session() as session:
        session.run("MATCH (n) DETACH DELETE n").consume()
    typer.echo("wiped Postgres + Neo4j data")


@app.command()
def fetch() -> None:
    """Download the configured prescriber CSV."""
    from ontology.ingest.prescriber.fetch import download

    path = download()
    typer.echo(f"downloaded {path} ({path.stat().st_size:,} bytes)")


@app.command()
def load(
    year: int = typer.Option(settings.prescriber_year),
    state: str = typer.Option(settings.prescriber_state, help="Two-letter state code"),
) -> None:
    """Ingest the CMS Part D Prescriber dataset, filtered to one state."""
    from ontology.ingest.prescriber.load import run

    counts = run(year=year, state=state.upper())
    typer.echo(f"staged {counts['_staged_rows']:,} CSV rows")
    for k, v in counts.items():
        if not k.startswith("_"):
            typer.echo(f"  {k:<15} {v:,}")


@app.command()
def project() -> None:
    """Project Postgres entities + relations into Neo4j."""
    from ontology.project.to_neo4j import project as run

    e, r = run()
    typer.echo(f"projected {e:,} entities and {r:,} relations into Neo4j")


@app.command()
def verify(
    scope: str = typer.Option(
        "all", help="Which checks to run: postgres | neo4j | all"
    ),
) -> None:
    """Run sanity + cross-store consistency checks. Exits non-zero on any failure."""
    from ontology.verify import run_all

    results = run_all(scope=scope)
    failed = 0
    for r in results:
        typer.echo(r.line())
        if not r.passed:
            failed += 1
    typer.echo("-" * 60)
    total = len(results)
    typer.echo(f"{total - failed}/{total} checks passed")
    if failed:
        raise typer.Exit(code=1)


@app.command()
def stats() -> None:
    """Print quick counts from both stores."""
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute("SELECT type, count(*) FROM entity GROUP BY type ORDER BY 2 DESC")
        typer.echo("postgres entities:")
        for t, c in cur.fetchall():
            typer.echo(f"  {t:<20} {c:>10,}")
        cur.execute("SELECT predicate, count(*) FROM relation GROUP BY predicate ORDER BY 2 DESC")
        typer.echo("postgres relations:")
        for p, c in cur.fetchall():
            typer.echo(f"  {p:<20} {c:>10,}")

    with neo4j_driver().session() as session:
        n = session.run("MATCH (n:Entity) RETURN count(n) AS c").single()["c"]
        r = session.run("MATCH ()-[r]->() RETURN count(r) AS c").single()["c"]
        typer.echo(f"neo4j: {n:,} nodes, {r:,} relationships")

    close_all()


def _parse_param(raw: str) -> tuple[str, object]:
    if "=" not in raw:
        raise typer.BadParameter(f"param must be key=value, got {raw!r}")
    k, v = raw.split("=", 1)
    try:
        return k, json.loads(v)
    except json.JSONDecodeError:
        return k, v


@app.command(name="list-queries")
def list_queries_cmd() -> None:
    """List available .cypher queries in queries/."""
    from ontology.queries import list_queries

    for name in list_queries():
        typer.echo(name)


@app.command()
def query(
    name: str = typer.Argument(..., help="Query name (filename without .cypher)"),
    param: list[str] = typer.Option(
        [], "--param", "-p", help="key=value pair; value is JSON-parsed if possible"
    ),
    limit: int = typer.Option(50, help="Truncate result rows when printing"),
) -> None:
    """Run a query from queries/<name>.cypher with optional --param key=value."""
    from ontology.queries import run_query

    params = dict(_parse_param(p) for p in param)
    rows = run_query(name, **params)
    typer.echo(f"{len(rows)} rows")
    for row in rows[:limit]:
        typer.echo(json.dumps(row, default=str))
    if len(rows) > limit:
        typer.echo(f"... ({len(rows) - limit} more)")


if __name__ == "__main__":
    app()
