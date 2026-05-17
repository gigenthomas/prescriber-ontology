"""Pipeline run tracking. Wraps ETL operations so every change_event written
during the run is attributable to a specific pipeline_run row.

Usage:

    from ontology.lineage import pipeline_run

    with pipeline_run("prescriber.load", inputs={"year": 2023, "state": "CA"}) as run:
        ...  # ETL work
        run.outputs["Prescriber"] = 110430

The context manager:
- INSERTs a `pipeline_run` row at start (status='running')
- Sets `ontology.pipeline_run_id` as a Postgres SESSION variable, so triggers
  on entity / relation / entity_state stamp the run on every change_event row
- UPDATEs to status='succeeded' or 'failed' on exit
- Captures the latest git commit hash if available
"""

from __future__ import annotations

import json
import os
import subprocess
from contextlib import contextmanager
from typing import Any, Iterator

from ontology.db import pg_conn


class _PipelineRun:
    def __init__(self, id_: str):
        self.id = id_
        self.outputs: dict[str, Any] = {}


def _current_commit_sha() -> str | None:
    try:
        out = subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"],
            stderr=subprocess.DEVNULL,
            cwd=os.getcwd(),
            timeout=2,
        )
        return out.decode().strip()
    except Exception:
        return None


@contextmanager
def pipeline_run(
    name: str,
    source_id: str | None = None,
    inputs: dict[str, Any] | None = None,
    actor: str = "system",
) -> Iterator[_PipelineRun]:
    """Wrap an ETL operation so its changes are attributable to a pipeline_run."""
    commit = _current_commit_sha()
    with pg_conn() as conn, conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO pipeline_run (name, source_id, inputs, commit_sha, actor)
            VALUES (%s, %s, %s::jsonb, %s, %s)
            RETURNING id
            """,
            (name, source_id, json.dumps(inputs or {}), commit, actor),
        )
        run_id = cur.fetchone()[0]
        # Set session variable so triggers on entity/relation/entity_state
        # stamp this pipeline_run_id on every change_event row produced inside
        # this transaction. SET (no LOCAL) persists for the connection's life.
        cur.execute("SET ontology.pipeline_run_id = %s", (str(run_id),))
        conn.commit()

    run = _PipelineRun(str(run_id))
    try:
        yield run
    except Exception as exc:
        with pg_conn() as conn, conn.cursor() as cur:
            cur.execute(
                """
                UPDATE pipeline_run
                SET finished_at = now(),
                    status      = 'failed',
                    error_msg   = %s,
                    outputs     = %s::jsonb
                WHERE id = %s
                """,
                (str(exc), json.dumps(run.outputs), run_id),
            )
            cur.execute("RESET ontology.pipeline_run_id")
            conn.commit()
        raise
    else:
        with pg_conn() as conn, conn.cursor() as cur:
            cur.execute(
                """
                UPDATE pipeline_run
                SET finished_at = now(),
                    status      = 'succeeded',
                    outputs     = %s::jsonb
                WHERE id = %s
                """,
                (json.dumps(run.outputs), run_id),
            )
            cur.execute("RESET ontology.pipeline_run_id")
            conn.commit()
