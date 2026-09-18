"""Table structure from the drawn cell borders of a Word-exported PDF page.

Word draws every cell border as a thin black filled rectangle (sometimes as a stroked line) and the cell
shading as separate, lighter fills. Only the black borders define the table: the x/y positions of all
border segments form a fine grid, two neighbouring grid cells belong to the same table cell when no border
separates them. That yields real colspans and rowspans. Shading, link underlines (blue) and text never
create columns.

Cells are kept with page coordinates (x0, x1) so tables continued on the next page can be put on one
common column grid.
"""
import re
from bisect import bisect_left

import pymupdf

DARK = 0.35      # max colour component of a border
THIN = 2.5       # max thickness of a border rectangle
SNAP = 1.6       # border positions closer than this are the same grid line
PAGE_GRID_TOLERANCE = 2.0
MIN_COL = 6.0    # narrower columns without own text are layout spacers
MIN_ROW = 4.0


def _bold(span):
    return bool(span["flags"] & 16) or "bold" in span["font"].lower()


def border_segments(page):
    hs, vs = [], []
    for dr in page.get_drawings():
        col = dr.get("fill") if dr["type"] == "f" else dr.get("color")
        if col is None or max(col) > DARK:
            continue
        for it in dr["items"]:
            if it[0] == "re":
                r = it[1]
                if r.width <= THIN and r.height > THIN:
                    vs.append(((r.x0 + r.x1) / 2, r.y0, r.y1))
                elif r.height <= THIN and r.width > THIN:
                    hs.append(((r.y0 + r.y1) / 2, r.x0, r.x1))
                elif dr["type"] != "f" and r.width > THIN and r.height > THIN:  # stroked rectangle
                    vs += [(r.x0, r.y0, r.y1), (r.x1, r.y0, r.y1)]
                    hs += [(r.y0, r.x0, r.x1), (r.y1, r.x0, r.x1)]
            elif it[0] == "l":
                p, q = it[1], it[2]
                if abs(p.x - q.x) < 1 and abs(p.y - q.y) > THIN:
                    vs.append((p.x, min(p.y, q.y), max(p.y, q.y)))
                elif abs(p.y - q.y) < 1 and abs(p.x - q.x) > THIN:
                    hs.append((p.y, min(p.x, q.x), max(p.x, q.x)))
    return hs, vs


def _components(hs, vs):
    """Group border segments that touch into tables."""
    segs = [("h", s) for s in hs] + [("v", s) for s in vs]
    rects = []
    for k, (pos, a, b) in segs:
        rects.append(pymupdf.Rect(a, pos, b, pos) if k == "h" else pymupdf.Rect(pos, a, pos, b))
    parent = list(range(len(segs)))

    def find(i):
        while parent[i] != i:
            parent[i] = parent[parent[i]]
            i = parent[i]
        return i
    grown = [pymupdf.Rect(r.x0 - 2, r.y0 - 2, r.x1 + 2, r.y1 + 2) for r in rects]
    order = sorted(range(len(segs)), key=lambda i: grown[i].y0)
    for n, i in enumerate(order):
        for index in range(n + 1, len(order)):
            j = order[index]
            if grown[j].y0 > grown[i].y1:
                break
            if grown[i].intersects(grown[j]):
                parent[find(i)] = find(j)
    groups = {}
    for i in range(len(segs)):
        groups.setdefault(find(i), []).append(segs[i])
    out = []
    for g in groups.values():
        gh = [s for k, s in g if k == "h"]
        gv = [s for k, s in g if k == "v"]
        if len(gh) >= 2 and len(gv) >= 2:
            out.append((gh, gv))
    return out


def _cluster(vals, tol=SNAP):
    vals = sorted(vals)
    out = []
    for v in vals:
        if out and v - out[-1][-1] <= tol:
            out[-1].append(v)
        else:
            out.append([v])
    return [sum(c) / len(c) for c in out]


def _coverage(segs, lo, hi):
    """Share of [lo, hi] covered by the union of the (a, b) intervals."""
    if hi <= lo:
        return 1.0
    iv = sorted((max(a, lo), min(b, hi)) for a, b in segs if b > lo and a < hi)
    tot, cur = 0.0, None
    for a, b in iv:
        if cur is None or a > cur[1]:
            if cur:
                tot += cur[1] - cur[0]
            cur = [a, b]
        else:
            cur[1] = max(cur[1], b)
    if cur:
        tot += cur[1] - cur[0]
    return tot / (hi - lo)


class Table:
    def __init__(self, bbox, cells, nrows, columns):
        self.bbox = bbox
        self.cells = cells      # dicts: r0, r1 (row range), x0, x1, y0, y1, text, bold
        self.nrows = nrows
        self.columns = columns


BOLD = "\x02"  # marks a bold line inside a cell that is not bold as a whole


def _cell_text(lines, left, right):
    """Visual lines -> text. '\n' = wrapped line, '\n\n' = new paragraph (blank-line gap or hard line break:
    the previous line ended although the next word would still have fitted)."""
    out, prev = [], None
    mixed = not all(ln["bold"] for ln in lines)
    for ln in lines:
        if prev is not None:
            h = max(prev["y1"] - prev["y0"], 6)
            first = ln["text"].split(" ")[0]
            # Word cell padding is about 5.4pt on each side; works for left-aligned and centred text
            fits = (prev["x1"] - prev["x0"]) + 0.3 * prev["size"] + 0.52 * ln["size"] * len(first) < (right - left) - 12
            label = re.match(r"^[A-ZÄÖÜ][\wäöüß-]*:\s", ln["text"]) is not None
            label = label or (ln["text"].startswith(("http://", "https://")) and not prev["text"].startswith("http"))
            label = label or not prev["text"].strip("_ ")  # signature line
            hard = (fits or label or prev["bold"] != ln["bold"]) and not prev["text"].endswith(("-", "/")) \
                and not ln["text"].startswith("•")
            out.append("\n\n" if ln["y0"] - prev["y1"] > 0.6 * h or hard else "\n")
        out.append((BOLD if mixed and ln["bold"] else "") + ln["text"])
        prev = ln
    return "".join(out)


def _lines_in(spans):
    spans = sorted(spans, key=lambda s: (round(s["bbox"][3]), s["bbox"][0]))
    lines = []
    for s in spans:
        y = s["bbox"][3]
        if lines and abs(lines[-1]["y"] - y) < 3.0:
            lines[-1]["spans"].append(s)
        else:
            lines.append({"y": y, "spans": [s]})
    res = []
    for ln in lines:
        sp = sorted(ln["spans"], key=lambda s: s["bbox"][0])
        txt, last_x1 = "", None
        for s in sp:
            t = s["text"]
            if 0xF000 <= ord(t.strip()[:1] or " ") <= 0xF0FF and len(t.strip()) == 1:
                t = "•"  # Symbol-font bullet
            if last_x1 is not None and s["bbox"][0] - last_x1 > 1.0 and not txt.endswith(" ") and not t.startswith(" "):
                txt += " "
            txt += t
            last_x1 = s["bbox"][2]
        txt = re.sub(r"^•\s*", "• ", txt.strip())
        if txt:
            res.append({"text": txt, "y0": min(s["bbox"][1] for s in sp), "y1": max(s["bbox"][3] for s in sp),
                        "x0": min(s["bbox"][0] for s in sp), "x1": max(s["bbox"][2] for s in sp), "size": max(s["size"] for s in sp),
                        "bold": all(_bold(s) for s in sp if any(ch.isalnum() for ch in s["text"]))})
    return res


def _split_spans(raw, cell_at):
    """Word may write the texts of neighbouring cells as one span: cut spans where the characters change cell.
    Returns {cell index: [span pieces]}."""
    out = {}
    for s in raw:
        pieces, cur, cur_cell = [], [], None
        for ch in s["chars"]:
            k = cell_at((ch["bbox"][0] + ch["bbox"][2]) / 2, (ch["bbox"][1] + ch["bbox"][3]) / 2)
            if cur and k != cur_cell:
                pieces.append((cur_cell, cur))
                cur = []
            cur.append(ch)
            cur_cell = k
        if cur:
            pieces.append((cur_cell, cur))
        for k, chars in pieces:
            text = "".join(c["c"] for c in chars)
            if k is None or not text.strip():
                continue
            bbox = (min(c["bbox"][0] for c in chars), s["bbox"][1], max(c["bbox"][2] for c in chars), s["bbox"][3])
            out.setdefault(k, []).append({"text": text, "bbox": bbox, "size": s["size"], "flags": s["flags"], "font": s["font"]})
    return out


def rectangular_strips(members: list[tuple[int, int]]) -> list[dict[str, int]]:
    # A missing border can join cells into a nonrectangular region.
    strips = []
    for row in sorted({row for row, _ in members}):
        columns = sorted(column for member_row, column in members if member_row == row)
        run = [columns[0]]
        for c in columns[1:] + [None]:
            if c is not None and c == run[-1] + 1:
                run.append(c)
                continue
            prev = next((st for st in strips if st["r1"] == row and st["c0"] == run[0] and st["c1"] == run[-1] + 1), None)
            if prev:
                prev["r1"] = row + 1
            else:
                strips.append({"r0": row, "r1": row + 1, "c0": run[0], "c1": run[-1] + 1})
            if c is not None:
                run = [c]
    return strips


def find_tables(page: pymupdf.Page, audit: bool = False) -> list[Table]:
    hs, vs = border_segments(page)
    flags = pymupdf.TEXTFLAGS_RAWDICT & ~pymupdf.TEXT_PRESERVE_IMAGES
    raw = [span for block in page.get_text("rawdict", flags=flags)["blocks"] if block["type"] == 0
           for line in block["lines"] for span in line["spans"] if span["size"] >= 4] if not audit else []
    words = page.get_text("words") if audit else []
    tables = []
    for gh, gv in _components(hs, vs):
        xs = _cluster([s[0] for s in gv])
        ys = _cluster([s[0] for s in gh])
        if len(xs) < 2 or len(ys) < 2:
            continue
        vby = {i: [] for i in range(len(xs))}
        for x, a, b in gv:
            vby[min(range(len(xs)), key=lambda i: abs(xs[i] - x))].append((a, b))
        hby = {i: [] for i in range(len(ys))}
        for y, a, b in gh:
            hby[min(range(len(ys)), key=lambda i: abs(ys[i] - y))].append((a, b))
        R, C = len(ys) - 1, len(xs) - 1
        perimeter = (_coverage(hby[0], xs[0], xs[-1]), _coverage(hby[R], xs[0], xs[-1]),
                     _coverage(vby[0], ys[0], ys[-1]), _coverage(vby[C], ys[0], ys[-1]))
        if min(perimeter) < 0.7:
            continue
        if R * C > 100_000:
            raise ValueError("PDF table grid is too large")
        parent = {(r, c): (r, c) for r in range(R) for c in range(C)}

        def find(k):
            while parent[k] != k:
                parent[k] = parent[parent[k]]
                k = parent[k]
            return k
        for r in range(R):
            for c in range(C):
                if c + 1 < C and _coverage(vby[c + 1], ys[r], ys[r + 1]) < 0.5:
                    parent[find((r, c))] = find((r, c + 1))
                if r + 1 < R and _coverage(hby[r + 1], xs[c], xs[c + 1]) < 0.5:
                    parent[find((r, c))] = find((r + 1, c))
        comps = {}
        for k in parent:
            comps.setdefault(find(k), []).append(k)
        cells = []
        for members in comps.values():
            r0 = min(r for r, _ in members)
            r1 = max(r for r, _ in members) + 1
            c0 = min(c for _, c in members)
            c1 = max(c for _, c in members) + 1
            if len(members) == (r1 - r0) * (c1 - c0):
                cells.append({"r0": r0, "r1": r1, "c0": c0, "c1": c1})
                continue
            cells.extend(rectangular_strips(members))
        grid2cell = {}
        for i, c in enumerate(cells):
            for r in range(c["r0"], c["r1"]):
                for k in range(c["c0"], c["c1"]):
                    grid2cell[(r, k)] = i

        def cell_at(x, y):
            if not (xs[0] <= x <= xs[-1] and ys[0] <= y <= ys[-1]):
                return None
            r = min(bisect_left(ys, y) - 1, R - 1)
            k = min(bisect_left(xs, x) - 1, C - 1)
            return grid2cell.get((max(r, 0), max(k, 0)))
        if audit:
            pieces = {}
            for x0, y0, x1, y1, text, *_ in words:
                index = cell_at((x0 + x1) / 2, (y0 + y1) / 2)
                if index is not None:
                    pieces.setdefault(index, []).append({
                        "bbox": (x0, y0, x1, y1), "text": text, "flags": 0, "font": "",
                        "size": (y1 - y0) / 1.35,
                    })
        else:
            pieces = _split_spans(raw, cell_at)
        for i, c in enumerate(cells):
            c.update(x0=xs[c["c0"]], x1=xs[c["c1"]], y0=ys[c["r0"]], y1=ys[c["r1"]])
            lines = _lines_in(pieces.get(i, []))
            c["text"] = _cell_text(lines, c["x0"], c["x1"])
            c["bold"] = bool(lines) and all(ln["bold"] for ln in lines)
        # drop spacer rows (thin, no text start)
        text_rows = {c["r0"] for c in cells if c["text"]}
        keep_r = [r for r in range(R) if ys[r + 1] - ys[r] >= MIN_ROW or r in text_rows]
        rmap = {}
        for i, r in enumerate(keep_r):
            rmap[r] = i
        out = []
        for c in cells:
            rows = [r for r in range(c["r0"], c["r1"]) if r in rmap]
            if not rows:
                continue
            c["r0"], c["r1"] = rmap[rows[0]], rmap[rows[-1]] + 1
            out.append(c)
        bbox = pymupdf.Rect(xs[0], ys[0], xs[-1], ys[-1])
        tables.append(Table(bbox, out, len(keep_r), xs))
    tables.sort(key=lambda t: t.bbox.y0)
    return tables


def _same_row_text(a, b):
    def normalized(row):
        return [re.sub(r"\s+", " ", cell["text"].replace(BOLD, "")).strip().casefold() for cell in row]

    return normalized(a) == normalized(b)


def row_cells(cells, r):
    return sorted([c for c in cells if c["r0"] == r], key=lambda c: c["x0"])


def append_continuation(prev, nxt, warnings):
    """Append table `nxt` (top of the next page) to `prev` (bottom of the previous page).

    Drop a repeated header and join an aligned row with an empty key to
    the preceding keyed row.
    """
    cells = [dict(c) for c in nxt.cells]
    header = row_cells(cells, 0)
    if header and all(c["r1"] == 1 for c in header) and _same_row_text(row_cells(prev.cells, 0), header):
        warnings.append("Repeated table header omitted.")
        cells = [c for c in cells if c["r0"] > 0]
        for c in cells:
            c["r0"] -= 1
            c["r1"] -= 1
    if not cells:
        return
    last_r = prev.nrows - 1
    last = [c for c in prev.cells if c["r0"] <= last_r < c["r1"]]
    first = [c for c in cells if c["r0"] == 0]
    last.sort(key=lambda cell: cell["x0"])
    first.sort(key=lambda cell: cell["x0"])
    aligned = bool(first) and len(last) == len(first) and all(
        abs(pc[edge] - nc[edge]) <= PAGE_GRID_TOLERANCE
        for pc, nc in zip(last, first) for edge in ("x0", "x1")
    )
    split = (
        aligned and last[0]["text"].strip() and not first[0]["text"].strip()
        and any(c["text"].strip() for c in first[1:])
    )
    off = prev.nrows
    if split and all(c["r1"] == 1 for c in first):
        for target, continuation in zip(last, first):
            if continuation["text"].strip():
                target["text"] = (target["text"].rstrip() + "\n" + continuation["text"]).lstrip()
        cells = [c for c in cells if c["r0"] > 0]
        off -= 1
        warnings.append("split row joined across page break")
    for c in cells:
        c["r0"] += off
        c["r1"] += off
    prev.cells.extend(cells)
    prev.nrows = max(c["r1"] for c in prev.cells)


def to_rows(table):
    """Rows of HTML cells {text, bold, colspan, rowspan} on one column grid (spacer columns dropped)."""
    cells = table.cells
    xs = _cluster([c["x0"] for c in cells] + [c["x1"] for c in cells], tol=3.0)  # grids of several pages

    def xi(x):
        return min(range(len(xs)), key=lambda i: abs(xs[i] - x))
    for c in cells:
        c["c0"], c["c1"] = xi(c["x0"]), xi(c["x1"])
    text_cols = {c["c0"] for c in cells if c["text"].strip()}
    keep = [k for k in range(len(xs) - 1) if xs[k + 1] - xs[k] >= MIN_COL or k in text_cols]
    live = [c for c in cells if any(c["c0"] <= k < c["c1"] for k in keep)]
    starts = sorted({c["r0"] for c in live})  # rows in which no cell starts are absorbed by the rowspans
    rows = []
    for r in starts:
        out = []
        for c in sorted((c for c in live if c["r0"] == r), key=lambda c: c["c0"]):
            span = sum(1 for k in keep if c["c0"] <= k < c["c1"])
            rspan = sum(1 for s in starts if c["r0"] <= s < c["r1"])
            out.append({"text": c["text"], "bold": c["bold"], "colspan": span, "rowspan": rspan,
                        "x0": c["x0"], "x1": c["x1"]})
        rows.append(out)
    return rows
