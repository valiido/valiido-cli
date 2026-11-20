"""Read OOXML parts without executing fields, formulas, or relationships."""
import base64
import posixpath
from pathlib import Path
from tempfile import TemporaryDirectory
import re
from datetime import datetime, timedelta
from html import escape
from urllib.parse import urlsplit
from zipfile import ZipFile

from defusedxml.ElementTree import fromstring

W = "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}"
S = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
R = "{http://schemas.openxmlformats.org/officeDocument/2006/relationships}"
A = "{http://schemas.openxmlformats.org/drawingml/2006/main}"


class Package(ZipFile):
    def part(self, path):
        with self.open(path) as stream:
            data = stream.read(32 * 1024 * 1024 + 1)
        if len(data) > 32 * 1024 * 1024:
            raise ValueError("Office part exceeds size limit")
        return data

    def xml(self, path):
        return fromstring(self.part(path), forbid_dtd=True)

    def optional(self, path):
        return self.xml(path) if path in self.namelist() else ()

    def relationships(self, part):
        folder, name = posixpath.split(part)
        return {rel.get("Id"): rel for rel in self.optional(f"{folder}/_rels/{name}.rels")}


def internal(part, rel):
    target = rel.get("Target", "")
    if rel.get("TargetMode") == "External" or urlsplit(target).scheme:
        raise ValueError("Unexpected external Office relationship")
    return posixpath.normpath(posixpath.join(posixpath.dirname(part), target)).lstrip("/")


def val(element, name, default=None):
    node = element.find(W + name)
    return default if node is None else node.get(W + "val", default)


def list_markup(blocks):
    """Render adjacent list items with their nesting and numbering type."""
    out, stack = [], []

    def close():
        out.extend(("</li>", f"</{stack.pop()}>"))

    for tag, level, body in blocks:
        if tag not in ("ul", "ol"):
            while stack:
                close()
            out.append(f"<{tag}>{body}</{tag}>")
            continue
        level = min(level, len(stack))
        while len(stack) > level + 1:
            close()
        if len(stack) == level + 1:
            if stack[-1] == tag:
                out.append("</li>")
            else:
                close()
        if len(stack) == level:
            stack.append(tag)
            out.append(f"<{tag}>")
        out.extend(("<li>", body))
    while stack:
        close()
    return "\n".join(out)


class Word:
    part = "word/document.xml"

    def __init__(self, package, image, include_toc=False, source_text=None, part=None):
        self.part = part or self.part
        self.package = package
        self.image = image
        self.include_toc = include_toc
        self.source_text = source_text
        self.group_index = 0
        self.rels = package.relationships(self.part)
        self.styles = {s.get(W + "styleId"): s for s in package.optional("word/styles.xml")}
        nums = package.optional("word/numbering.xml")
        self.formats = {
            (n.get(W + "abstractNumId"), lv.get(W + "ilvl")): val(lv, "numFmt")
            for n in nums if n.tag == W + "abstractNum" for lv in n.findall(W + "lvl")
        }
        self.nums = {n.get(W + "numId"): val(n, "abstractNumId") for n in nums if n.tag == W + "num"}

    def kind(self, paragraph):
        props = paragraph.find(W + "pPr")
        if props is None:
            return "p", 0
        style = val(props, "pStyle", "")
        seen = set()
        for current in self.properties(props, style, seen):
            outline = val(current, "outlineLvl")
            if outline in tuple("012345"):
                return "h" + str(int(outline) + 1), 0
            num = current.find(W + "numPr")
            if num is not None and val(num, "numId") not in (None, "0"):
                level = val(num, "ilvl", "0")
                fmt = self.formats.get((self.nums.get(val(num, "numId")), level))
                return ("ul" if fmt == "bullet" else "ol"), int(level)
        heading = re.fullmatch(r"(?:Heading|Überschrift)\s*([1-6])", style, re.I)
        return ("h" + heading[1], 0) if heading else ("p", 0)

    def properties(self, props, style, seen):
        yield props
        while style in self.styles and style not in seen:
            seen.add(style)
            node = self.styles[style]
            inherited = node.find(W + "pPr")
            if inherited is not None:
                yield inherited
            style = val(node, "basedOn")

    def inline(self, node):
        if node.tag == W + "t":
            return escape((node.text or "").replace("\u00ad", ""))
        if node.tag in (W + "br", W + "cr", W + "tab"):
            return "<br>" if node.tag != W + "tab" else " "
        if node.tag.endswith("}AlternateContent"):
            return self.inline(node[0]) if len(node) else ""
        if node.tag.endswith("}wgp") and self.image:
            picture = group_picture(self.package, self.part, self.group_index)
            self.group_index += 1
            labels = [self.inline(t) for t in node.iter(W + "txbxContent")]
            return self.image(picture) + "<br>" + "".join(labels)
        if node.tag == W + "txbxContent":
            return "<br>".join(self.inline(p) for p in node.findall(W + "p")) + "<br>"
        if node.tag == A + "blip" and self.image:
            rel = self.rels.get(node.get(R + "embed"))
            if rel is not None and rel.get("TargetMode") != "External":
                path = internal(self.part, rel)
                if path.startswith("word/media/"):
                    return self.image(self.package.part(path))
        if node.tag not in {W + k for k in ("p", "r", "hyperlink", "ins", "drawing",
                                               "smartTag", "fldSimple")} and not node.tag.startswith((
            "{http://schemas.openxmlformats.org/markup-compatibility/2006}",
            "{http://schemas.microsoft.com/office/word/2010/wordprocessingGroup}",
            "{http://schemas.microsoft.com/office/word/2010/wordprocessingShape}",
            A, "{http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing}",
            "{http://schemas.openxmlformats.org/drawingml/2006/picture}",
        )):
            return ""
        body = "".join(self.inline(child) for child in node)
        if node.tag == W + "r":
            props = node.find(W + "rPr")
            if props is not None:
                for setting, tag in (("b", "strong"), ("i", "em")):
                    flag = props.find(W + setting)
                    if flag is not None and flag.get(W + "val") not in ("0", "false", "off") and body:
                        body = f"<{tag}>{body}</{tag}>"
        if node.tag == W + "hyperlink":
            rel = self.rels.get(node.get(R + "id"))
            url = rel.get("Target", "") if rel is not None else ""
            if urlsplit(url).scheme in ("http", "https", "mailto"):
                body = f'<a href="{escape(url, quote=True)}">{body}</a>'
        return body

    def blocks(self, parent):
        pending = []
        in_toc = False
        for node in parent:
            if node.tag == W + "p":
                style = node.find(f"{W}pPr/{W}pStyle")
                style_name = style.get(W + "val", "") if style is not None else ""
                field = "".join(v.text or "" for v in node.iter(W + "instrText"))
                starts_toc = bool(re.match(r"\s*TOC\b", field, re.I))
                simple_toc = any(re.match(r"\s*TOC\b", f.get(W + "instr", ""), re.I)
                                 for f in node.iter(W + "fldSimple"))
                in_toc = in_toc or starts_toc
                skip = in_toc or simple_toc or bool(re.match(r"^(TOC|Contents|Verzeichnis)[\s\d]|^TOCHeading$", style_name, re.I))
                if any(v.get(W + "fldCharType") == "end" for v in node.iter(W + "fldChar")):
                    in_toc = False
                if skip and not self.include_toc:
                    continue
                if self.source_text is not None:
                    self.source_text.append(visible_words(node))
                    continue
                body = self.inline(node)
                if body.strip():
                    tag, level = self.kind(node)
                    pending.append((tag, level, body))
            elif node.tag in (W + "tbl", W + "sdt"):
                yield list_markup(pending)
                pending.clear()
                if node.tag == W + "tbl":
                    yield self.table(node)
                elif self.include_toc or not any(v.get(W + "val", "").casefold() == "table of contents"
                             for v in node.iter(W + "docPartGallery")):
                    content = node.find(W + "sdtContent")
                    if content is not None:
                        yield from self.blocks(content)
        yield list_markup(pending)

    def table(self, table):
        rows, active = [], {}
        for row in table.findall(W + "tr"):
            props = row.find(W + "trPr")
            col = int(val(props, "gridBefore", "0")) if props is not None else 0
            if not 0 <= col <= 1000:
                raise ValueError("Invalid Word grid offset")
            previous, active = active, {}
            cells = []
            flag = row.find(f"{W}trPr/{W}tblHeader")
            header = flag is not None and flag.get(W + "val") not in ("0", "false", "off")
            if col:
                cells.append(["th" if header else "td", col, 1, ""])
            for cell in row.findall(W + "tc"):
                props = cell.find(W + "tcPr")
                colspan = int(val(props, "gridSpan", "1")) if props is not None else 1
                if not 1 <= colspan <= 1000:
                    raise ValueError("Invalid Word cell span")
                merge = props.find(W + "vMerge") if props is not None else None
                key = (col, colspan)
                if merge is not None and merge.get(W + "val") != "restart" and key in previous:
                    entry = previous[key]
                    entry[2] += 1
                    entry[3] += "".join(self.blocks(cell))
                else:
                    entry = ["th" if header else "td", colspan, 1, "".join(self.blocks(cell))]
                    cells.append(entry)
                if merge is not None:
                    active[key] = entry
                col += colspan
            rows.append(cells)
        rendered = []
        for row in rows:
            inner = "".join(
                f'<{tag}{span_attrs(cs, rs)}>{body}</{tag}>' for tag, cs, rs, body in row
            )
            rendered.append(f"<tr>{inner}</tr>")
        return "<table><tbody>" + "".join(rendered) + "</tbody></table>"


def span_attrs(cols, rows):
    return (f' colspan="{cols}"' if cols > 1 else "") + (f' rowspan="{rows}"' if rows > 1 else "")


def group_picture(package, part, index):
    """Render one grouped drawing, preserving connector positions and labels."""
    from defusedxml.minidom import parseString
    from import_office_convert import export
    from import_pdf_page import Renderer, open_pdf, pages

    document = parseString(package.part(part), forbid_dtd=True)
    group = document.getElementsByTagNameNS("*", "wgp")[index]
    drawing = group
    while drawing.localName != "drawing":
        drawing = drawing.parentNode
    body = document.getElementsByTagNameNS(W[1:-1], "body")[0]
    paragraph = document.createElementNS(W[1:-1], "w:p")
    run = document.createElementNS(W[1:-1], "w:r")
    run.appendChild(drawing.cloneNode(True))
    paragraph.appendChild(run)
    for child in list(body.childNodes):
        if getattr(child, "localName", None) != "sectPr":
            body.removeChild(child)
    for name in ("headerReference", "footerReference"):
        for ref in document.getElementsByTagNameNS(W[1:-1], name):
            ref.parentNode.removeChild(ref)
    body.insertBefore(paragraph, body.firstChild)
    with TemporaryDirectory() as temp:
        path = Path(temp) / "drawing.docx"
        with ZipFile(path, "w") as clipped:
            for name in package.namelist():
                clipped.writestr(name, document.toxml(encoding="utf-8") if name == part else package.part(name))
        pdf = path.with_suffix(".pdf")
        export(path, pdf)
        with open_pdf(pdf) as rendered:
            bounds = next(pages(rendered)).drawn_bounds()
        if bounds.is_empty:
            raise ValueError("Word drawing did not render")
        with Renderer(pdf) as renderer:
            return renderer.png(0, 180, clip=bounds)


def word_parts(package):
    part = Word.part
    body = package.xml(part).find(W + "body")
    rels = package.relationships(part)
    seen = set()
    for kind in ("headerReference", "body", "footerReference"):
        if kind == "body":
            yield part, body
            continue
        for ref in active_references(body, kind):
            target = internal(part, rels[ref.get(R + "id")])
            if target not in seen:
                seen.add(target)
                yield target, package.xml(target)


def active_references(body, kind):
    """Word shows a first-page header or footer only when its section sets titlePg."""
    for section in body.iter(W + "sectPr"):
        title = section.find(W + "titlePg")
        first = title is not None and title.get(W + "val") not in ("0", "false", "off")
        for ref in section.iter(W + kind):
            if ref.get(W + "type") != "first" or first:
                yield ref


def docx(path, image=None, include_toc=False):
    with Package(path) as package:
        return "\n".join(block for part, root in word_parts(package)
                         for block in Word(package, image, include_toc, part=part).blocks(root))


def visible_words(node):
    if node.tag in (W + "del", W + "moveFrom", W + "object"):
        return ""
    if node.tag.endswith("}AlternateContent"):
        return visible_words(node[0]) if len(node) else ""
    if node.tag == W + "txbxContent":
        return " " + " ".join(visible_words(child) for child in node) + " "
    if node.tag == W + "t":
        return (node.text or "").replace("\u00ad", "")
    if node.tag in (W + "br", W + "cr", W + "tab"):
        return "\n"
    return "".join(visible_words(child) for child in node)


def docx_source(path):
    with Package(path) as package:
        text = []
        for part, root in word_parts(package):
            for _ in Word(package, None, source_text=text, part=part).blocks(root):
                pass
        return text


def workbook(path):
    if path.suffix.lower() == ".xls":
        from import_office_convert import export
        with TemporaryDirectory() as work:
            converted = Path(work) / "workbook.xlsx"
            export(path, converted)
            return workbook(converted)
    with Package(path) as package:
        part = "xl/workbook.xml"
        root = package.xml(part)
        rels = package.relationships(part)
        shared = ["".join(t.text or "" for t in item.iter(S + "t"))
                  for item in package.optional("xl/sharedStrings.xml")]
        styles = package.optional("xl/styles.xml")
        format_codes = {x.get("numFmtId"): x.get("formatCode") for group in styles
                        if group.tag == S + "numFmts" for x in group}
        formats = [format_codes.get(x.get("numFmtId"), x.get("numFmtId", "0"))
                   for group in styles if group.tag == S + "cellXfs" for x in group]
        settings = root.find(S + "workbookPr")
        result = {"date1904": settings is not None and settings.get("date1904") in ("1", "true"),
                  "date_encoding": "iso8601", "sheets": [], "media": {}}
        for item in root.iter(S + "sheet"):
            rel = rels[item.get(R + "id")]
            if not rel.get("Type", "").endswith("/worksheet"):
                continue
            sheet_part = internal(part, rel)
            data = package.xml(sheet_part)
            sheet = {"name": item.get("name"), "state": item.get("state", "visible"), "rows": {},
                     "merges": [m.get("ref") for m in data.iter(S + "mergeCell")],
                     "formulas": {}, "formats": {}, "images": [], "header_footer": {},
                     "comments": {}, "validations": []}
            for rule in data.iter(S + "dataValidation"):
                sheet["validations"].append({**rule.attrib, **{
                    child.tag.split("}")[-1]: child.text for child in rule}})

            for group in data.findall(S + "headerFooter"):
                sheet["header_footer"].update({n.tag[len(S):]: n.text for n in group if n.text})
            for number, row in enumerate(data.iter(S + "row"), 1):
                rn = int(row.get("r", number))
                values = {}
                column = 0
                for cell in row.findall(S + "c"):
                    column += 1
                    ref = cell.get("r") or column_name(column) + str(rn)
                    match = re.fullmatch(r"([A-Z]{1,3})([1-9]\d*)", ref)
                    if not match or int(match[2]) != rn:
                        raise ValueError("Invalid spreadsheet cell")
                    column = column_number(match[1])
                    value = cell.findtext(S + "v", "")
                    kind = cell.get("t")
                    if kind == "s":
                        value = shared[int(value)]
                    elif kind == "inlineStr":
                        value = "".join(t.text or "" for t in cell.iter(S + "t"))
                    elif kind == "b":
                        value = {"0": "false", "1": "true"}.get(value, value)
                    formula = cell.find(S + "f")
                    if formula is not None:
                        sheet["formulas"][ref] = {"text": formula.text, **formula.attrib}
                    if value or formula is not None:
                        fmt = formats[int(cell.get("s", 0))] if formats else None
                        if cell.get("s") and fmt is not None:
                            sheet["formats"][ref] = fmt
                        if kind in (None, "n") and value:
                            value = display_cell(value, fmt, result["date1904"])
                        values[match[1]] = value
                if values:
                    sheet["rows"][str(rn)] = values
            sheet_rels = package.relationships(sheet_part)
            for link in sheet_rels.values():
                if link.get("Type", "").endswith("/comments"):
                    comments = package.xml(internal(sheet_part, link))
                    authors = [a.text or "" for a in comments.iter(S + "author")]
                    for note in comments.iter(S + "comment"):
                        sheet["comments"][note.get("ref")] = {
                            "author": authors[int(note.get("authorId"))],
                            "text": "".join(t.text or "" for t in note.iter(S + "t"))}
            for draw in data.iter(S + "drawing"):
                drawing = internal(sheet_part, sheet_rels[draw.get(R + "id")])
                links = package.relationships(drawing)
                for anchor in package.xml(drawing):
                    origin = next((n for n in anchor if n.tag.endswith("}from")), ())
                    position = {n.tag.split("}")[-1]: n.text for n in origin}
                    for blip in anchor.iter(A + "blip"):
                        rid = blip.get(R + "embed")
                        if rid:
                            media = internal(drawing, links[rid])
                            sheet["images"].append({"part": media, "anchor": position})
            result["sheets"].append(sheet)
        for name in package.namelist():
            if name.startswith("xl/media/") and not name.endswith("/"):
                result["media"][name] = base64.b64encode(package.part(name)).decode("ascii")
        return result


def column_number(name):
    column = 0
    for c in name:
        column = column * 26 + ord(c) - ord("A") + 1
    if not 1 <= column <= 16384:
        raise ValueError("Invalid spreadsheet column")
    return column


def column_name(number):
    name = ""
    while number:
        number, digit = divmod(number - 1, 26)
        name = chr(ord("A") + digit) + name
    return name


def xlsx(path):
    for sheet in workbook(path)["sheets"]:
        for number, row in sheet["rows"].items():
            if any(not value and name + number in sheet["formulas"] for name, value in row.items()):
                raise ValueError("Spreadsheet formula has no cached value; use workbook mode")
            indices = {column_number(k): v for k, v in row.items()}
            yield sheet["name"], int(number), [indices.get(i, "") for i in range(1, max(indices) + 1)]


def display_cell(value, fmt, date1904):
    # Ignore literals, locale tags and the text section of custom number formats.
    code = re.sub(r'"[^"]*"|\\.|_.|\*.|\[[^]]*\]', "", fmt or "").split(";")[0].lower()
    date_tokens = [token for token in re.findall(r"[a-z]+", code) if re.fullmatch(r"[ymdhs]+", token)]
    if code not in ("14", "15", "16", "17", "22") and not any(re.search(r"[yd]|m{3}", token) for token in date_tokens):
        return value
    try:
        days = float(value)
        if not date1904 and 60 <= days < 61:
            return value  # Excel's fictitious 1900-02-29
        origin = datetime(1904, 1, 1) if date1904 else datetime(1899, 12, 30 if days >= 61 else 31)
        dt = origin + timedelta(days=days)
        return dt.date().isoformat() if dt.time() == datetime.min.time() else dt.isoformat(" ")
    except (ValueError, OverflowError):
        return value


def workbook_html(book, image):
    """Render the sparse cell grid, including blank parts of merged form fields."""
    html = []
    for sheet in book["sheets"]:
        html.append(f'<h2>{escape(sheet["name"])}</h2>')
        for entry in sheet["images"]:
            html.append(image(base64.b64decode(book["media"][entry["part"]], validate=True)))
        rows = {int(r): {column_number(c): v for c, v in cells.items()}
                for r, cells in sheet["rows"].items()}
        spans, covered = {}, set()
        height = max(rows, default=0)
        width = max((max(row) for row in rows.values()), default=0)
        for area in sheet["merges"]:
            match = re.fullmatch(r"([A-Z]+)(\d+):([A-Z]+)(\d+)", area)
            if not match:
                raise ValueError("Invalid merged range")
            left, top = column_number(match[1]), int(match[2])
            right, bottom = column_number(match[3]), int(match[4])
            width, height = max(width, right), max(height, bottom)
            if width * height > 1000000:
                raise ValueError("Grid exceeds preview limit")
            spans[top, left] = span_attrs(right-left+1, bottom-top+1)
            covered.update((r, c) for r in range(top, bottom+1) for c in range(left, right+1)
                           if (r, c) != (top, left))
        if width * height > 1000000:
            raise ValueError("Grid exceeds preview limit")
        html.append("<table><tbody>")
        for r in range(1, height+1):
            html.append("<tr>")
            for c in range(1, width+1):
                if (r, c) not in covered:
                    value = rows.get(r, {}).get(c, "")
                    if "date_encoding" not in book:  # Workbooks exported with raw serials.
                        value = display_cell(value, sheet["formats"].get(column_name(c) + str(r)), book["date1904"])
                    html.append(f'<td{spans.get((r, c), "")}>' + escape(value).replace("\n", "<br>") + "</td>")
            html.append("</tr>")
        html.append("</tbody></table>")
        for cell, note in sheet.get("comments", {}).items():
            html.append(f'<p>{escape(cell)} · {escape(note["author"])} · {escape(note["text"])}</p>')
    return "\n".join(html)
