"""Extract readable PDF content and stitch compatible border grids."""
from collections import Counter
from html import escape
import re

import pymupdf

from office import list_markup, span_attrs
from tablegrid import BOLD, PAGE_GRID_TOLERANCE, append_continuation, find_tables, to_rows

# Native diagnostics may include document paths or excerpts.
pymupdf.TOOLS.mupdf_display_errors(False)
pymupdf.TOOLS.mupdf_display_warnings(False)

BULLET = re.compile(r"^([•▪◦–-]|\d+[.)])\s+(.+)")
CONTENTS = re.compile(r"\s*\.{3,}\s*\d+\s*$")
PAGE_NUMBER = re.compile(r"\s*(?:(?:page|seite)\s+)?\d+(?:\s*(?:/|of|von)\s*\d+)?\s*", re.I)


def join_lines(text):
    text = re.sub(r"\u00ad\s*", "", text)
    text = re.sub(
        r"([^\W\d_])-[ \t]*\n[ \n\t]*(?!(?:und|oder|and|or)\b)(?=[^\W\d_])",
        lambda match: match[1] if text[match.end()].islower() else match[1] + "-",
        text,
    )
    return re.sub(r"\s+", " ", text).strip()


def cell_text(text):
    return join_lines(text.replace(BOLD, ""))


def table_html(table):
    rows = []
    for row in to_rows(table):
        cells = []
        for c in row:
            raw = re.sub(r"(?<=-)\n+(?=[^\W\d_])", "\n", c["text"])
            text = "<br>".join(escape(cell_text(p)) for p in raw.split("\n\n"))
            if c["bold"]:
                text = f"<strong>{text}</strong>"
            cells.append(f'<td{span_attrs(c["colspan"], c["rowspan"])}>{text}</td>')
        rows.append("<tr>" + "".join(cells) + "</tr>")
    return "<table><tbody>" + "".join(rows) + "</tbody></table>"


def lines(page):
    flags = pymupdf.TEXTFLAGS_DICT & ~pymupdf.TEXT_PRESERVE_IMAGES
    for block in page.get_text("dict", flags=flags)["blocks"]:
        for line in block.get("lines", []):
            spans = [s for s in line["spans"] if s["text"].strip()]
            if spans:
                text = "".join(s["text"] for s in line["spans"]).strip()
                yield (pymupdf.Rect(line["bbox"]), block["number"], text,
                       max(s["size"] for s in spans), all(s["flags"] & 16 for s in spans))


def margin(box, height):
    if box.y1 < height * 0.10:
        return "top"
    if box.y0 > height * 0.90:
        return "bottom"
    return ""


def boundary_key(line, height):
    box, _, text, *_ = line
    return margin(box, height), re.sub(r"\d+", "#", text.casefold())


def same_grid(previous, current):
    before, after = previous.columns, current.columns
    # A close pair of borders on one fragment can represent a single
    # separator on another. Require matching outer borders and coverage
    # in both directions for the internal separators.
    return all(
        abs(a - b) <= PAGE_GRID_TOLERANCE
        for a, b in ((before[0], after[0]), (before[-1], after[-1]))
    ) and all(
        any(abs(a - b) <= PAGE_GRID_TOLERANCE for b in target)
        for source, target in ((before, after), (after, before)) for a in source
    )


def boundary_picture(box, height, digest):
    # Repeated header artwork can extend past the narrower text margins.
    center = (box.y0 + box.y1) / 2
    if not box.is_empty and center < height * 0.15:
        return "top", digest
    if not box.is_empty and center > height * 0.85:
        return "bottom", digest
    return None


def inspect(path, images=False, audit=False):
    """Ordered items and page-aware table fragments from the source."""
    pages, counts = [], Counter()
    with pymupdf.open(path, filetype="pdf") as doc:
        picture_counts = Counter()
        picture_info = []
        if images:
            for page in doc:
                if page.rotation:
                    page.remove_rotation()
                entries = []
                for info in page.get_image_info(hashes=True):
                    box = pymupdf.Rect(info["bbox"]) & page.rect
                    entries.append((box, boundary_picture(box, page.rect.height, info["digest"])))
                picture_info.append(entries)
                picture_counts.update({key for _, key in entries if key is not None})
        for page in doc:
            if page.rotation:
                page.remove_rotation()
            height = page.rect.height
            text = list(lines(page))
            counts.update({boundary_key(line, height) for line in text if margin(line[0], height)})
            tables = find_tables(page, audit=audit)
            pictures = []
            if images:
                for box, key in picture_info[page.number]:
                    if not box.is_empty and not margin(box, height) and (
                        key is None or picture_counts[key] == 1
                    ):
                        pictures.append((box, page.get_pixmap(clip=box, dpi=120, alpha=False,
                                                              annots=False).tobytes("png")))
            pages.append((height, text, tables, pictures))

    result, warnings = [], []
    previous = None
    for number, (height, text, tables, pictures) in enumerate(pages, 1):
        contents = sum(bool(CONTENTS.search(line[2])) for line in text) >= 2
        items = [(t.bbox, "table", t) for t in tables]
        items += [(box, "image", image) for box, image in pictures]
        for line in text:
            box, _, value, *_ = line
            side, key = boundary_key(line, height)
            if side and (counts[side, key] > 1 or PAGE_NUMBER.fullmatch(value)):
                continue
            if CONTENTS.search(value) or (contents and re.fullmatch(
                r"(?:table of contents|contents|inhaltsverzeichnis)", value, re.I
            )):
                continue
            center = (box.tl + box.br) / 2
            if any(center in t.bbox for t in tables):
                continue
            items.append((box, "line", line))
        items.sort(key=lambda x: (x[0].y0, x[0].x0))
        # Only body items delimit a continuation. Keep unclassified margin text
        # in the output, but don't let it break a table across pages.
        body = [i for i, (box, kind, _) in enumerate(items)
                if kind != "line" or not margin(box, height)]
        first, last = (body[0], body[-1]) if body else (None, None)
        carried = previous
        previous = None
        for index, (_, kind, item) in enumerate(items):
            if kind == "table" and index == first and carried and same_grid(carried, item):
                append_continuation(carried, item, warnings)
                item = carried
            else:
                result.append((number, kind, item))
            if kind == "table" and index == last:
                previous = item
    return result, warnings


def blocks(items):
    pending = []
    for page, kind, line in items:
        if kind != "line":
            if pending:
                yield from paragraphs(pending)
                pending.clear()
            yield kind, 0, line
            continue
        pending.append((page, *line))
    if pending:
        yield from paragraphs(pending)


def paragraphs(lines):
    from statistics import median

    baseline = median(line[4] for line in lines)
    group = None
    previous = None
    for page, box, block, text, size, bold in lines:
        match = BULLET.match(text)
        if match:
            tag = "ol" if match[1][0].isdigit() else "ul"
            text = match[2]
        elif size >= baseline * 1.18 or (bold and size >= 13 and len(text) < 120):
            tag = "h1" if size >= baseline * 1.5 else "h2"
        else:
            tag = "p"
        if group and not match and tag == "p" and group[0] in ("p", "ul", "ol") and (
            previous[0] == page and (previous[1] == block or
                                     group[-1].endswith(("-", "\u00ad")) and box.y0 - previous[2] < size)
        ):
            group[-1] += "\n" + text
        else:
            if group:
                yield group[0], 0, escape(join_lines(group[1]))
            group = [tag, text]
        previous = (page, block, box.y1)
    if group:
        yield group[0], 0, escape(join_lines(group[1]))


def render(path, image):
    items, warnings = inspect(path, images=True)
    pending, out = [], []
    for tag, level, body in blocks(items):
        if tag in ("table", "image"):
            out.append(list_markup(pending))
            pending.clear()
            out.append(table_html(body) if tag == "table" else image(body))
        else:
            pending.append((tag, level, body))
    out.append(list_markup(pending))
    return "\n".join(out), warnings

