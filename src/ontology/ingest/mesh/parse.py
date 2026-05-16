from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path

from lxml import etree


@dataclass
class MeshDescriptor:
    ui: str
    name: str
    tree_numbers: list[str] = field(default_factory=list)
    synonyms: list[str] = field(default_factory=list)
    scope_note: str | None = None


def _text(el: etree._Element | None, path: str) -> str | None:
    node = el.find(path) if el is not None else None
    return node.text.strip() if node is not None and node.text else None


def iter_descriptors(xml_path: Path) -> Iterator[MeshDescriptor]:
    """Stream MeSH DescriptorRecords from the annual descriptor XML."""
    context = etree.iterparse(str(xml_path), events=("end",), tag="DescriptorRecord")
    for _, rec in context:
        ui = _text(rec, "DescriptorUI") or ""
        name = _text(rec, "DescriptorName/String") or ""

        tree_numbers = [
            n.text for n in rec.findall("TreeNumberList/TreeNumber") if n.text
        ]

        synonyms: list[str] = []
        for term in rec.findall("ConceptList/Concept/TermList/Term/String"):
            if term.text and term.text != name:
                synonyms.append(term.text)

        scope_note = _text(rec, "ConceptList/Concept/ScopeNote")

        yield MeshDescriptor(
            ui=ui,
            name=name,
            tree_numbers=tree_numbers,
            synonyms=sorted(set(synonyms)),
            scope_note=scope_note.strip() if scope_note else None,
        )

        rec.clear()
        while rec.getprevious() is not None:
            del rec.getparent()[0]
