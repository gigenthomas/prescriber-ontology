from pathlib import Path

import httpx
from tqdm import tqdm

from ontology.config import settings

MESH_BASE = "https://nlmpubs.nlm.nih.gov/projects/mesh"


def mesh_url(year: int, file: str = "desc") -> str:
    return f"{MESH_BASE}/{year}/xmlmesh/{file}{year}.xml"


class MeshFetchError(RuntimeError):
    pass


def download(year: int | None = None, file: str = "desc") -> Path:
    year = year or settings.mesh_year
    url = mesh_url(year, file)
    settings.raw_dir.mkdir(parents=True, exist_ok=True)
    out = settings.raw_dir / f"{file}{year}.xml"

    if out.exists() and out.stat().st_size > 1_000_000:
        return out

    tmp = out.with_suffix(out.suffix + ".part")
    with httpx.stream("GET", url, follow_redirects=True, timeout=None) as r:
        r.raise_for_status()
        ctype = r.headers.get("content-type", "")
        if "html" in ctype.lower():
            raise MeshFetchError(
                f"{url} returned content-type {ctype!r} — NLM likely served its "
                f"'URL not found' HTML page. Check the year is published."
            )
        total = int(r.headers.get("content-length", 0)) or None
        with open(tmp, "wb") as f, tqdm(
            total=total, unit="B", unit_scale=True, desc=f"{file}{year}.xml"
        ) as bar:
            for chunk in r.iter_bytes(chunk_size=1024 * 256):
                f.write(chunk)
                bar.update(len(chunk))

    with open(tmp, "rb") as f:
        head = f.read(512)
    if b"<!doctype html" in head.lower() or b"<html" in head.lower():
        tmp.unlink()
        raise MeshFetchError(
            f"{url} downloaded HTML instead of XML — NLM returned an error page."
        )
    if not head.lstrip().startswith(b"<?xml"):
        tmp.unlink()
        raise MeshFetchError(f"{url} did not return XML (first bytes: {head[:80]!r})")

    tmp.replace(out)
    return out
