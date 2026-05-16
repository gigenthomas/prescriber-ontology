from pathlib import Path

import httpx
from tqdm import tqdm

from ontology.config import settings


class PrescriberFetchError(RuntimeError):
    pass


def download() -> Path:
    """Download the Part D Prescriber-by-Drug CSV. Cached on disk after first run."""
    settings.raw_dir.mkdir(parents=True, exist_ok=True)
    out = settings.raw_dir / f"mup_dpr_{settings.prescriber_year}.csv"

    # Cached if it's at least ~500 MB (full national file is ~3 GB; sanity threshold).
    if out.exists() and out.stat().st_size > 500_000_000:
        return out

    tmp = out.with_suffix(out.suffix + ".part")
    with httpx.stream("GET", settings.prescriber_url, follow_redirects=True, timeout=None) as r:
        r.raise_for_status()
        ctype = r.headers.get("content-type", "")
        if "csv" not in ctype.lower() and "octet" not in ctype.lower():
            raise PrescriberFetchError(
                f"{settings.prescriber_url} returned content-type {ctype!r} — expected CSV."
            )
        total = int(r.headers.get("content-length", 0)) or None
        with open(tmp, "wb") as f, tqdm(
            total=total, unit="B", unit_scale=True, desc=out.name
        ) as bar:
            for chunk in r.iter_bytes(chunk_size=1024 * 512):
                f.write(chunk)
                bar.update(len(chunk))

    tmp.replace(out)
    return out
