from collections.abc import Iterator
from contextlib import contextmanager

import psycopg
from neo4j import Driver, GraphDatabase
from psycopg_pool import ConnectionPool

from ontology.config import settings

_pg_pool: ConnectionPool | None = None
_neo_driver: Driver | None = None


def pg_pool() -> ConnectionPool:
    global _pg_pool
    if _pg_pool is None:
        _pg_pool = ConnectionPool(settings.postgres_dsn, min_size=1, max_size=8, open=True)
    return _pg_pool


@contextmanager
def pg_conn() -> Iterator[psycopg.Connection]:
    with pg_pool().connection() as conn:
        yield conn


def neo4j_driver() -> Driver:
    global _neo_driver
    if _neo_driver is None:
        _neo_driver = GraphDatabase.driver(
            settings.neo4j_uri,
            auth=(settings.neo4j_user, settings.neo4j_password),
        )
    return _neo_driver


def close_all() -> None:
    global _pg_pool, _neo_driver
    if _pg_pool is not None:
        _pg_pool.close()
        _pg_pool = None
    if _neo_driver is not None:
        _neo_driver.close()
        _neo_driver = None
