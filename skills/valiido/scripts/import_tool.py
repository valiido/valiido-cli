"""Local conversion and review for Valiido imports."""
import argparse
import base64
from collections import Counter
from collections.abc import Callable, Iterator
import csv
import hashlib
from html import escape
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import re
import sys
import unicodedata
from urllib.parse import quote, unquote

# Isolated Python mode omits the script directory. Add only bundled code.
sys.path.append(str(Path(__file__).resolve().parent))

TAGS = set("p div br h1 h2 h3 h4 h5 h6 strong b em i u s ul ol li table thead tbody "
           "tfoot tr th td blockquote pre code a img figure figcaption span".split())
HIDDEN = {"script", "style", "template", "noscript", "head", "svg", "math", "iframe", "object"}
INLINE = {"a", "strong", "b", "em", "i", "u", "s", "code", "span"}
IMAGE_FORMATS = ((b"\x89PNG\r\n\x1a\n", "png"), (b"\xff\xd8\xff", "jpeg"),
                 (b"GIF87a", "gif"), (b"GIF89a", "gif"))


class Markup(HTMLParser):
    def __init__(self, image: Callable[[str], str] | None = None) -> None:
        super().__init__()
        self.image = image
        self.hidden = []
        self.text = []
        self.html = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag in HIDDEN:
            self.hidden.append(tag)
        if self.hidden or tag not in TAGS:
            return
        if tag not in INLINE:
            self.text.append("\n")
        attrs = dict(attrs)
        if tag == "img":
            if self.image:
                self.html.append(self.image(attrs.get("src") or ""))
            return
        spans = "".join(f' {key}="{attrs[key]}"' for key in ("rowspan", "colspan")
                        if tag in ("td", "th") and re.fullmatch(r"[1-9][0-9]{0,3}", attrs.get(key) or ""))
        self.html.append(f"<{tag}{spans}>")

    def handle_endtag(self, tag: str) -> None:
        if self.hidden:
            if tag == self.hidden[-1]:
                self.hidden.pop()
            return
        if tag in TAGS and tag not in ("br", "img"):
            if tag not in INLINE:
                self.text.append("\n")
            self.html.append(f"</{tag}>")

    def handle_data(self, data: str) -> None:
        if not self.hidden:
            self.text.append(data)
            self.html.append(escape(data))


def markup(html: str, image: Callable[[str], str] | None = None) -> Markup:
    parser = Markup(image)
    parser.feed(html)
    parser.close()
    return parser


def data_image(data: bytes) -> str:
    image_format = embedded_image_format(data)
    if image_format is None:
        return ""
    encoded = base64.b64encode(data).decode("ascii")
    return f'<img src="data:image/{image_format};base64,{encoded}" alt="">'


def embedded_image_format(data: bytes) -> str | None:
    return next((name for signature, name in IMAGE_FORMATS if data.startswith(signature)), None)


def write(path: Path, data: str | bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(data.encode("utf-8") if isinstance(data, str) else data)


def convert(args: argparse.Namespace) -> None:
    count = 0
    if args.output.exists() or args.output.is_symlink():
        raise FileExistsError

    def save_image(data: bytes) -> str:
        nonlocal count
        image_format = embedded_image_format(data)
        if image_format is None:
            print("Unsupported embedded image format omitted.", file=sys.stderr)
            return ""
        count += 1
        name = f"{args.output.name}.image-{count}.{image_format}"
        write(args.output.with_name(name), data)
        return f'<img src="{quote(name)}" alt="">'

    html = render(args.source, save_image)
    write(args.output, html)


def render(source: Path, image: Callable[[bytes], str]) -> str:
    if source.suffix.lower() == ".docx":
        from office import docx
        return docx(source, image)
    if source.suffix.lower() == ".pdf":
        from pdf_import import render as pdf_render
        html, warnings = pdf_render(source, image)
        for warning in warnings:
            print(warning, file=sys.stderr)
        return html
    raise ValueError("Unsupported document format")


def pdf_rows(source: Path) -> Iterator[tuple[str, int, list[str]]]:
    from pdf_import import cell_text, inspect
    from tablegrid import to_rows

    tables, warnings = inspect(source)
    for warning in warnings:
        print(warning, file=sys.stderr)
    for index, (page, _, table) in enumerate((entry for entry in tables if entry[1] == "table"), 1):
        active = {}
        for number, row in enumerate(to_rows(table), 1):
            values = {}
            col = 0
            for cell in row:
                while col in active:
                    col += 1
                value = cell_text(cell["text"])
                for offset in range(cell["colspan"]):
                    slot = col + offset
                    if slot in active:
                        raise ValueError("Overlapping PDF table cells")
                    values[slot] = value if offset == 0 else ""
                    if cell["rowspan"] > 1:
                        active[slot] = (number + cell["rowspan"], values[slot])
                col += cell["colspan"]
            # Empty continuation-only lines cannot become records.
            if any(values.values()):
                values.update({slot: value for slot, (_, value) in active.items() if slot not in values})
                yield f"table-{index}@{page}", number, [values.get(c, "") for c in range(max(values) + 1)]
            active = {slot: span for slot, span in active.items() if span[0] > number + 1}


def tabular(source: Path, delimiter: str = ",") -> Iterator[tuple[str | None, int, list[str]]]:
    suffix = source.suffix.lower()
    if suffix in (".xlsx", ".xls"):
        from office import xlsx
        yield from xlsx(source)
    elif suffix == ".csv":
        with source.open(encoding="utf-8-sig", newline="") as stream:
            reader = csv.reader(stream, delimiter=delimiter, strict=True)
            for row in reader:
                yield None, reader.line_num, row
    elif suffix == ".pdf":
        yield from pdf_rows(source)
    else:
        raise ValueError("Unsupported table format")


def records(args):
    group, headers = None, None
    for sheet, row, values in tabular(args.source, args.delimiter):
        if args.sheet is not None and args.sheet != sheet:
            continue
        if sheet != group:
            group, headers = sheet, None
        values = [value.strip() for value in values]
        while values and not values[-1]:
            values.pop()
        if not values:
            continue
        if headers is None:
            if row < args.header_row:
                continue
            headers = [name or f"column_{i}" for i, name in enumerate(values, 1)]
            if len(set(headers)) != len(headers):
                raise ValueError("Duplicate table column names")
            continue
        if values == headers:
            continue
        if len(values) > len(headers):
            raise ValueError("Data row has more columns than header")
        data = {key: values[i] if i < len(values) else "" for i, key in enumerate(headers)}
        yield {"source": {"sheet": sheet, "row": row}, "values": data}


def table(args: argparse.Namespace) -> None:
    if args.workbook:
        from office import workbook
        if args.sheet or args.header_row != 1 or args.source.suffix.lower() not in (".xls", ".xlsx"):
            raise ValueError("Workbook mode requires a complete Excel file")
        write(args.output, json.dumps(workbook(args.source), ensure_ascii=False, indent=2) + "\n")
        return
    rows = list(records(args))
    if not rows:
        raise ValueError("No records found")
    write(args.output, "".join(json.dumps(row, ensure_ascii=False) + "\n" for row in rows))


def tokens(text: str) -> list[str]:
    return re.findall(r"[^\W_]+", unicodedata.normalize("NFKC", text).casefold())


def source_text(path: Path, delimiter: str) -> Iterator[str]:
    suffix = path.suffix.lower()
    if suffix == ".pdf":
        from pdf_import import inspect, blocks, cell_text
        items, _ = inspect(path, audit=True)
        for tag, _, value in blocks(items):
            if tag == "table":
                yield from (cell_text(cell["text"]) for cell in value.cells)
            else:
                yield "".join(markup(value).text)
    elif suffix == ".docx":
        from office import docx_source
        yield from docx_source(path)
    else:
        for _, _, values in tabular(path, delimiter):
            yield from values


class Structure(HTMLParser):
    def __init__(self, html):
        super().__init__()
        self.nodes = []
        self.images = []
        self.feed(html)
        self.close()

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag in ("table", "tr", "td", "th", "ul", "ol", "li", "a", "br",
                   "h1", "h2", "h3", "h4", "h5", "h6"):
            self.nodes.append((tag, attrs.get("colspan"), attrs.get("rowspan"), attrs.get("href")))
        if tag == "img":
            self.images.append(attrs.get("src", ""))


def check_word_structure(source, html):
    expected_images = []
    def digest_image(data):
        if embedded_image_format(data):
            expected_images.append(hashlib.sha256(data).digest())
        return ""

    reference = Structure(render(source, digest_image))
    actual = Structure(html.read_text(encoding="utf-8"))
    images = []
    for src in actual.images:
        name = unquote(src)
        if not re.fullmatch(re.escape(html.name) + r"\.image-[1-9]\d*\.(png|jpeg|gif)", name):
            return False
        path = html.with_name(name)
        if not path.is_file() or path.is_symlink() or path.stat().st_size > 32 * 1024 * 1024:
            return False
        images.append(hashlib.sha256(path.read_bytes()).digest())
    return reference.nodes == actual.nodes and expected_images == images


def check(args: argparse.Namespace) -> bool:
    if args.source.suffix.lower() in (".xls", ".xlsx") and args.html.suffix.lower() == ".json":
        from office import workbook
        original = workbook(args.source)
        valid = original == json.loads(args.html.read_text(encoding="utf-8"))
        payload = json.dumps({"ok": valid, "sheets": len(original["sheets"]),
                              "cells": sum(len(r) for s in original["sheets"] for r in s["rows"].values()),
                              "images": len(original["media"])}) + "\n"
        if args.report:
            write(args.report, payload)
        else:
            print(payload, end="")
        return not valid
    expected = Counter(word for text in source_text(args.source, args.delimiter) for word in tokens(text))
    if not expected and not args.structure:
        raise ValueError("No source text found")
    actual = Counter(tokens("".join(markup(args.html.read_text(encoding="utf-8")).text)))
    missing = expected - actual
    result = {"ok": not missing, "source_words": expected.total(),
              "missing_words": dict(sorted(missing.items()))}
    if args.structure:
        if args.source.suffix.lower() != ".docx":
            raise ValueError("Word structure check requires DOCX")
        result["structure_ok"] = check_word_structure(args.source, args.html)
        result["ok"] = result["ok"] and result["structure_ok"]
    payload = json.dumps(result, ensure_ascii=False) + "\n"
    if args.report:
        write(args.report, payload)
    else:
        # Missing terms can contain customer material. Keep them in a private report.
        print(json.dumps({"ok": result["ok"], "source_words": expected.total(),
                          "missing_count": missing.total()}))
    return not result["ok"]


def trusted_data_image(src: str) -> str:
    if re.fullmatch(r"data:image/(?:png|jpeg|gif);base64,[A-Za-z0-9+/=]+", src):
        return f'<img src="{src}" alt="">'
    return ""


def preview_record(record):
    source = record["source"]
    title = escape(f'{source["sheet"] or "CSV"} · {source["row"]}')
    fields = "".join(f"<tr><th>{escape(key)}</th><td>{escape(value)}</td></tr>"
                     for key, value in record["values"].items())
    return f"<h3>{title}</h3><table>{fields}</table>"


def preview_image(result: Path, src: str) -> str:
    name = unquote(src)
    if not re.fullmatch(re.escape(result.name) + r"\.image-[1-9]\d*\.(?:png|jpeg|gif)", name):
        return ""
    path = result.with_name(name)
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 32 * 1024 * 1024:
        return ""
    return data_image(path.read_bytes())


def preview_source(source: Path, delimiter: str) -> str:
    if source.suffix.lower() == ".pdf":
        from pdf_import import pymupdf
        with pymupdf.open(source, filetype="pdf") as doc:
            return "\n".join(data_image(page.get_pixmap(dpi=100, annots=False).tobytes("png"))
                                 for page in doc)
    if source.suffix.lower() == ".docx":
        from office import docx
        return "".join(markup(docx(source, data_image, include_toc=True),
                                  trusted_data_image).html)
    previous = object()
    parts = []
    for sheet, _, values in tabular(source, delimiter):
        if sheet != previous:
            if parts:
                parts.append("</table>")
            parts.append(f"<h3>{escape(sheet or 'CSV')}</h3><table>")
            previous = sheet
        parts.append("<tr>" + "".join(f"<td>{escape(value)}</td>" for value in values) + "</tr>")
    return "".join(parts) + ("</table>" if parts else "")


def preview_result(path: Path) -> str:
    if path.suffix.lower() == ".json":
        from office import workbook_html
        return workbook_html(json.loads(path.read_text(encoding="utf-8")), data_image)
    if path.suffix.lower() == ".jsonl":
        with path.open(encoding="utf-8") as stream:
            return "".join(preview_record(json.loads(line)) for line in stream if line.strip())
    return "".join(markup(path.read_text(encoding="utf-8"), lambda src: preview_image(path, src)).html)


def preview_frame(body: str, title: str) -> str:
    document = """<meta charset="utf-8"><meta http-equiv="Content-Security-Policy"
 content="default-src 'none'; img-src data:; style-src 'unsafe-inline'">
<style>body{font:16px system-ui;overflow-wrap:anywhere}img{max-width:100%;display:block}
table{border-collapse:collapse}td,th{border:1px solid #aaa;padding:.3rem;white-space:pre-wrap}</style>""" + body
    return f'<section><h2>{title}</h2><iframe title="{title}" sandbox srcdoc="{escape(document)}"></iframe></section>'


def preview(args: argparse.Namespace) -> None:
    original = preview_source(args.source, args.delimiter)
    result = preview_result(args.html)
    write(args.output, """<!doctype html><html lang="en"><meta charset="utf-8">
<title>Import review</title><style>
body{font:16px system-ui;margin:1rem}main{display:grid;grid-template-columns:1fr 1fr;gap:1rem}
section{min-width:0}iframe{box-sizing:border-box;width:100%;height:85vh;border:1px solid #aaa}
</style><main>""" + preview_frame(original, "Source") + preview_frame(result, "Result") + "</main></html>")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for action in (convert, table, check, preview):
        command = sub.add_parser(action.__name__)
        command.add_argument("source", type=Path)
        if action in (check, preview):
            command.add_argument("html", type=Path)
        if action is not check:
            command.add_argument("output", type=Path)
        else:
            command.add_argument("--report", type=Path)
            command.add_argument("--structure", action="store_true", help="Verify local DOCX tables, lists, links and image bytes")
        if action is not convert:
            command.add_argument("--delimiter", default=",", choices=(",", ";", "\t", "|"))
        if action is table:
            command.add_argument("--workbook", action="store_true", help="Preserve all sheets, cells, formulas, merges and embedded pictures as JSON")
            command.add_argument("--sheet")
            command.add_argument("--header-row", type=int, default=1)
        command.set_defaults(action=action)
    args = parser.parse_args()
    try:
        return int(args.action(args) or 0)
    except Exception as error:
        # Parser messages can include paths and source content.
        print(f"Import tool failed ({type(error).__name__}).", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
