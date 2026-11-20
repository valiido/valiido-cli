"""Text lines, drawings, pictures and renderings of PDF pages.

pdfplumber (pdfminer.six) reads the page content, pypdfium2 rasterises it. Coordinates are points
from the top-left corner of the visible page (crop box, after page rotation), as the page is displayed.

Characters are taken in content stream order and joined into lines and blocks by how far the pen moves
between two glyphs, measured in font sizes: a small step along the baseline is the same word, a larger
one a space, a jump on the same baseline or to the next baseline a new line, anything further a new block.
"""
from hashlib import sha256
from io import BytesIO
import logging
import math
import re
import unicodedata

import pdfplumber
import pypdfium2

# Parser diagnostics may include document paths or excerpts.
for _name in ("pdfminer", "pdfplumber", "pypdfium2"):
    _logger = logging.getLogger(_name)
    _logger.addHandler(logging.NullHandler())
    _logger.propagate = False

SPACE_DIST = 0.15      # pen steps (in font sizes) above this separate two words
SPACE_MAX_DIST = 0.8   # above this a step along the baseline starts a new line
BASE_MAX_DIST = 0.8    # baseline shifts below this stay on the line
PARAGRAPH_DIST = 1.5   # baseline shifts above this start a new block
FAKE_BOLD_MAX_DIST = 0.1  # the same glyph again this close is overprinted bold
# Line starts that may be list markers rather than an indented paragraph.
BULLET_START = re.compile("[*0-9\u00b7\u2022\u2023\u2043\u204c\u204d\u2219\u25c9\u25cb\u25cf\u25d8\u25e6"
                          "\u2619-\u261f\u2660-\u2667\u2765\u2767\u29be\u29bf\U0001f446-\U0001f449"
                          "\U0001f597-\U0001f5a3\U0001fbc1-\U0001fbc3\ufffd]")
CID = re.compile(r"\(cid:(\d+)\)")


class Box:
    """Axis-aligned rectangle; empty when it has no area."""
    __slots__ = ("x0", "y0", "x1", "y1")

    def __init__(self, x0, y0, x1, y1):
        self.x0, self.y0, self.x1, self.y1 = x0, y0, x1, y1

    def __iter__(self):
        return iter((self.x0, self.y0, self.x1, self.y1))

    def __getitem__(self, index):
        return (self.x0, self.y0, self.x1, self.y1)[index]

    def __repr__(self):
        return f"Box({self.x0:g}, {self.y0:g}, {self.x1:g}, {self.y1:g})"

    @property
    def width(self):
        return self.x1 - self.x0

    @property
    def height(self):
        return self.y1 - self.y0

    @property
    def is_empty(self):
        return self.x0 >= self.x1 or self.y0 >= self.y1

    @property
    def center(self):
        return (self.x0 + self.x1) / 2, (self.y0 + self.y1) / 2

    def contains(self, point):
        x, y = point
        return self.x0 <= x < self.x1 and self.y0 <= y < self.y1

    def intersects(self, other):
        return not self.is_empty and not other.is_empty and \
            self.x0 < other.x1 and other.x0 < self.x1 and self.y0 < other.y1 and other.y0 < self.y1

    def __and__(self, other):
        return Box(max(self.x0, other.x0), max(self.y0, other.y0), min(self.x1, other.x1), min(self.y1, other.y1))

    def __or__(self, other):
        if self.is_empty:
            return Box(*other)
        if other.is_empty:
            return Box(*self)
        return Box(min(self.x0, other.x0), min(self.y0, other.y0), max(self.x1, other.x1), max(self.y1, other.y1))


def rgb(color):
    """pdfminer colour (gray, RGB or CMYK components) -> RGB; None for patterns and unknown values."""
    if isinstance(color, (int, float)):
        color = (color,)
    if not isinstance(color, (tuple, list)) or not all(isinstance(v, (int, float)) for v in color):
        return None
    if len(color) == 1:
        return (color[0],) * 3
    if len(color) == 3:
        return tuple(color)
    if len(color) == 4:
        c, m, y, k = color
        return ((1 - c) * (1 - k), (1 - m) * (1 - k), (1 - y) * (1 - k))
    return None


def _text(value):
    # Glyphs without a Unicode mapping keep their character code where it is a character.
    def code(match):
        n = int(match[1])
        return chr(n) if 32 <= n < 0x110000 and not 0xD800 <= n < 0xE000 else "\ufffd"
    return CID.sub(code, value)


class Page:
    def __init__(self, page):
        self.page = page
        left, top, right, bottom = page.cropbox
        self.left, self.top = left, top
        self.rect = Box(0, 0, right - left, bottom - top)
        self._lines = None

    def box(self, obj):
        return Box(obj["x0"] - self.left, obj["top"] - self.top, obj["x1"] - self.left, obj["bottom"] - self.top)

    def _chars(self):
        x0, top, x1, bottom = self.page.mediabox
        for ch in self.page.chars:
            # Glyphs outside the media box are not on the page.
            if ch["x1"] < x0 or ch["x0"] > x1 or ch["bottom"] < top or ch["top"] > bottom:
                continue
            a, b, c, d, e, f = ch["matrix"]
            norm = math.hypot(a, b) or 1.0
            direction = (a / norm, -b / norm)
            box = self.box(ch)
            size = ch["size"] if abs(a) >= abs(b) else box.x1 - box.x0
            adv = ch["adv"]
            font = ch["fontname"]
            yield {"c": _text(ch["text"]), "bbox": box, "size": size, "font": font,
                   "bold": "bold" in font.lower(), "color": ch.get("non_stroking_color"),
                   "dir": direction, "origin": (e, -f), "pen": (e + a * adv, -(f + b * adv))}

    @property
    def lines(self):
        """Text lines in content order: {"bbox", "block", "spans"}; spans hold chars of one style."""
        if self._lines is None:
            self._lines = _layout(self._chars())
        return self._lines

    def spans(self):
        return [s for line in self.lines for s in line["spans"]]

    def words(self):
        """Whitespace-separated words: (x0, y0, x1, y1, text, size)."""
        out = []
        for line in self.lines:
            word = []
            for ch in [ch for s in line["spans"] for ch in s["chars"]] + [None]:
                if ch is not None and ch["c"].strip():
                    word.append(ch)
                    continue
                if word:
                    box = _union(c["bbox"] for c in word)
                    out.append((*box, "".join(c["c"] for c in word), max(c["size"] for c in word)))
                    word = []
        return out

    def drawings(self):
        """Painted paths: (kind, fill, stroke, fill colour, stroke colour, shape).

        The shape of kind "re" is a Box, of kind "l" a list of segments ((x, y), (x, y))."""
        for obj in self.page.rects:
            yield ("re", obj.get("fill"), obj.get("stroke"), obj.get("non_stroking_color"),
                   obj.get("stroking_color"), self.box(obj))
        for obj in self.page.lines + self.page.curves:
            style = (obj.get("fill"), obj.get("stroke"), obj.get("non_stroking_color"), obj.get("stroking_color"))
            for segments in _subpaths(obj.get("path") or (), self.left, self.top):
                corners = {point for segment in segments for point in segment}
                xs, ys = sorted({x for x, _ in corners}), sorted({y for _, y in corners})
                # A closed path of four axis-parallel sides is a rectangle.
                if len(segments) == 4 and len(corners) == 4 and len(xs) == len(ys) == 2 and all(
                    p[0] == q[0] or p[1] == q[1] for p, q in segments
                ):
                    yield ("re", *style, Box(xs[0], ys[0], xs[1], ys[1]))
                else:
                    yield ("l", *style, segments)

    def images(self):
        """Placed pictures: (Box, digest of the image data)."""
        return [(self.box(obj), sha256(obj["stream"].get_rawdata() or b"").hexdigest())
                for obj in self.page.images]

    def drawn_bounds(self):
        """Area covered by text, paths and pictures."""
        boxes = []
        for kind in ("chars", "rects", "lines", "curves", "images"):
            for obj in getattr(self.page, kind):
                box = self.box(obj)
                # Strokes reach half their width beyond the path. pdfminer reports
                # the PDF default width of 1 as 0.
                pad = (obj.get("linewidth") or 1) / 2 if kind != "chars" and obj.get("stroke") else 0
                boxes.append(Box(box.x0 - pad, box.y0 - pad, box.x1 + pad, box.y1 + pad))
        return _union(boxes) & self.rect if boxes else Box(0, 0, 0, 0)


def _subpaths(path, left, top):
    """Straight segments of each subpath; closing a subpath draws back to its start."""
    segments, start, current = [], None, None
    for cmd, *points in path:
        points = [(x - left, y - top) for x, y in points]
        if cmd == "m":
            if segments:
                yield segments
            segments, start, current = [], points[-1], points[-1]
        elif cmd == "l" and current is not None:
            segments.append((current, points[-1]))
            current = points[-1]
        elif cmd == "h" and current is not None and start is not None:
            if current != start:
                segments.append((current, start))
            current = start
        elif points:
            current = points[-1]
    if segments:
        yield segments


def _union(boxes):
    boxes = list(boxes)
    return Box(min(b.x0 for b in boxes), min(b.y0 for b in boxes),
               max(b.x1 for b in boxes), max(b.y1 for b in boxes))


def _layout(chars):
    lines, block, prev, start = [], -1, None, None
    for ch in chars:
        c = ch["c"]
        if not c:
            continue
        if prev is not None and unicodedata.category(c[0]) == "Mn":
            # Combining marks belong to the previous glyph.
            _append(lines[-1], ch)
            continue
        new_line = new_block = True
        add_space = False
        if prev is not None and ch["dir"][0] * prev["dir"][0] + ch["dir"][1] * prev["dir"][1] >= 0.999:
            size = ch["size"] or 1.0
            # The same glyph printed again on top is fake bold.
            if c == prev["c"] and math.dist(ch["origin"], prev["origin"]) / size < FAKE_BOLD_MAX_DIST:
                continue
            nx, ny = ch["dir"]
            dx, dy = ch["origin"][0] - prev["pen"][0], ch["origin"][1] - prev["pen"][1]
            spacing = (nx * dx + ny * dy) / size
            base = (-ny * dx + nx * dy) / size
            new_block = False
            if abs(base) < BASE_MAX_DIST:
                # Small backward steps are overlapping glyphs; large steps a new column.
                new_line = not -SPACE_MAX_DIST < spacing < SPACE_MAX_DIST
                add_space = SPACE_DIST <= spacing < SPACE_MAX_DIST and _spaceable(prev["c"][-1])
            elif abs(base) <= PARAGRAPH_DIST:
                # An indented next line starts a paragraph, unless the line is a list item.
                new_block = ch["origin"][0] - start[0] > 0.5 and not start[1]
            else:
                new_block = True
        if new_block:
            block += 1
        if new_line or new_block:
            lines.append({"block": block, "spans": []})
            start = (ch["origin"][0], bool(BULLET_START.match(c)))
        elif add_space and c != " ":
            gap = Box(prev["bbox"].x1, ch["bbox"].y0, max(ch["bbox"].x0, prev["bbox"].x1), ch["bbox"].y1)
            _append(lines[-1], dict(ch, c=" ", bbox=gap))
        _append(lines[-1], ch)
        prev = ch
    for line in lines:
        line["bbox"] = _union(s["bbox"] for s in line["spans"])
    return lines


def _spaceable(c):
    # Scripts that separate words by spaces (latin to arabic, punctuation, currency).
    return c != " " and (ord(c) < 0x700 or 0x2000 <= ord(c) <= 0x20CF)


def _append(line, ch):
    spans = line["spans"]
    style = (ch["font"], ch["size"], ch["bold"], ch["color"])
    if not spans or spans[-1]["style"] != style:
        spans.append({"style": style, "font": ch["font"], "size": ch["size"], "bold": ch["bold"],
                      "chars": [], "text": "", "bbox": ch["bbox"]})
    span = spans[-1]
    span["chars"].append(ch)
    span["text"] += ch["c"]
    span["bbox"] |= ch["bbox"]


def open_pdf(path):
    return pdfplumber.open(path)


def pages(pdf):
    for page in pdf.pages:
        try:
            yield Page(page)
        finally:
            page.close()


class Renderer:
    """Rasterise pages without annotations, form fields or scripts."""

    def __init__(self, path):
        self.document = pypdfium2.PdfDocument(path)

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.document.close()

    def __len__(self):
        return len(self.document)

    def png(self, index, dpi, clip=None):
        page = self.document[index]
        try:
            scale = dpi / 72
            crop = (0, 0, 0, 0)
            if clip is not None:
                # Whole pixels covering the clip, as with the page rendered and then cut.
                width, height = math.ceil(page.get_width() * scale), math.ceil(page.get_height() * scale)
                x0 = min(max(math.floor(clip.x0 * scale + 0.001), 0), width - 1)
                y0 = min(max(math.floor(clip.y0 * scale + 0.001), 0), height - 1)
                x1 = min(max(math.ceil(clip.x1 * scale - 0.001), x0 + 1), width)
                y1 = min(max(math.ceil(clip.y1 * scale - 0.001), y0 + 1), height)
                crop = tuple((v - 0.001) / scale if v > 0 else 0
                             for v in (x0, height - y1, width - x1, y0))
            bitmap = page.render(scale=scale, crop=crop, draw_annots=False, may_draw_forms=False)
            try:
                out = BytesIO()
                bitmap.to_pil().convert("RGB").save(out, "PNG")
                return out.getvalue()
            finally:
                bitmap.close()
        finally:
            page.close()
