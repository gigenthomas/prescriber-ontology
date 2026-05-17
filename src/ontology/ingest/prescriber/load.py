import csv
from pathlib import Path

from tqdm import tqdm

from ontology.config import settings
from ontology.db import pg_conn
from ontology.ingest.prescriber import sql

# Columns we COPY into staging, in CSV order. Numeric columns get '' -> NULL conversion.
COLUMNS = [
    "Prscrbr_NPI", "Prscrbr_Last_Org_Name", "Prscrbr_First_Name",
    "Prscrbr_City", "Prscrbr_State_Abrvtn", "Prscrbr_State_FIPS",
    "Prscrbr_Type", "Prscrbr_Type_Src",
    "Brnd_Name", "Gnrc_Name",
    "Tot_Clms", "Tot_30day_Fills", "Tot_Day_Suply", "Tot_Drug_Cst", "Tot_Benes",
    "GE65_Sprsn_Flag", "GE65_Tot_Clms", "GE65_Tot_30day_Fills",
    "GE65_Tot_Drug_Cst", "GE65_Tot_Day_Suply", "GE65_Bene_Sprsn_Flag", "GE65_Tot_Benes",
]
NUMERIC_INDEXES = {10, 11, 12, 13, 14, 16, 17, 18, 19, 21}  # 0-indexed into COLUMNS

STATE_COL = COLUMNS.index("Prscrbr_State_Abrvtn")


def _row_for_copy(row: list[str]) -> list[str | None]:
    out: list[str | None] = []
    for i, val in enumerate(row):
        if val == "":
            out.append(None)
        elif i in NUMERIC_INDEXES:
            out.append(val)
        else:
            out.append(val)
    return out


def _ensure_source(conn, year: int, state: str) -> str:
    name = f"CMS-PartD-Prescribers-{year}-{state}"
    with conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO source (name, uri, version)
            VALUES (%s, %s, %s)
            ON CONFLICT (name) DO UPDATE SET version = EXCLUDED.version
            RETURNING id
            """,
            (name, settings.prescriber_url, str(year)),
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
            sql.SCHEMA_TERMS,
        )


def stage_csv(csv_path: Path, state: str) -> int:
    """Stream CSV, filter to state, COPY filtered rows into mup_dpr_staging. Returns row count."""
    with pg_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql.STAGING_DDL)
            cur.execute(sql.TRUNCATE_STAGING)
        conn.commit()

        rows = 0
        bar = tqdm(desc=f"staging {state}", unit="row")
        with open(csv_path, encoding="utf-8", newline="") as f, conn.cursor() as cur:
            reader = csv.reader(f)
            header = next(reader)
            if header != COLUMNS:
                raise RuntimeError(
                    f"CSV header mismatch.\n  expected: {COLUMNS}\n  got: {header}"
                )
            with cur.copy(sql.COPY_STAGING) as copy:
                for row in reader:
                    if row[STATE_COL] != state:
                        continue
                    copy.write_row(_row_for_copy(row))
                    rows += 1
                    if rows % 50_000 == 0:
                        bar.update(50_000)
            bar.update(rows % 50_000)
            bar.close()
        conn.commit()
    return rows


def transform(year: int, state: str) -> dict[str, int]:
    """Derive entities and relations from staging."""
    counts: dict[str, int] = {}
    with pg_conn() as conn:
        _ensure_schema_terms(conn)
        source_id = _ensure_source(conn, year, state)
        conn.commit()

        steps = [
            ("Prescribers",   sql.INSERT_PRESCRIBERS),
            ("Drugs",         sql.INSERT_DRUGS),
            ("GenericDrugs",  sql.INSERT_GENERIC_DRUGS),
            ("Specialties",   sql.INSERT_SPECIALTIES),
            ("Locations",     sql.INSERT_LOCATIONS),
            ("prescribed",    sql.INSERT_PRESCRIBED),
            ("generic_of",    sql.INSERT_GENERIC_OF),
            ("has_specialty", sql.INSERT_HAS_SPECIALTY),
            ("practices_in",  sql.INSERT_PRACTICES_IN),
        ]
        with conn.cursor() as cur:
            for label, stmt in tqdm(steps, desc="transform", unit="step"):
                cur.execute(stmt, {"source_id": source_id})
                counts[label] = cur.rowcount
                conn.commit()
    return counts


def run(year: int, state: str) -> dict[str, int]:
    from ontology.ingest.prescriber.fetch import download
    from ontology.lineage import pipeline_run

    with pipeline_run(
        "prescriber.load",
        inputs={"year": year, "state": state},
        actor="user:cli",
    ) as plr:
        csv_path = download()
        staged = stage_csv(csv_path, state)
        counts = transform(year, state)
        counts["_staged_rows"] = staged
        plr.outputs.update(counts)
        return counts
