"""CLI tests built entirely from synthetic documents."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from zipfile import ZipFile

import pymupdf

TOOL = Path(__file__).with_name("import_tool.py")
W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
S = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
R = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"


class ImportTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)

    def run_tool(self, *args, code=0):
        result = subprocess.run([sys.executable, "-I", str(TOOL), *map(str, args)],
                                capture_output=True, text=True, cwd=self.root)
        self.assertEqual(result.returncode, code, result.stderr)
        return result

    def test_pdf_grid_and_continuation(self):
        path = self.root / "document.pdf"
        with pymupdf.open() as doc:
            for continued in (False, True):
                page = doc.new_page(width=400, height=500)
                y = 390 if not continued else 65
                page.draw_rect((40, y, 360, y + 80))
                for offset in (20, 40, 60):
                    start = 100 if offset == 40 and not continued else 40
                    page.draw_line((start, y + offset), (360, y + offset))
                page.draw_line((100, y), (100, y + 80))
                page.draw_line((220, y + 20), (220, y + 80))
                rows = [
                    ("Key", "Details", ""),
                    ("", "mation", ""),
                    ("", "Independent line", "Z"),
                    ("B", "Second record", "Y"),
                ] if continued else [
                    ("Key", "Details", ""),
                    ("Shared", "First", "X"),
                    ("", "Additional", "X"),
                    ("A", "Infor-", ""),
                ]
                for ri, row in enumerate(rows):
                    for x, value in zip((45, 105, 225), row):
                        if value:
                            page.insert_text((x, y + 14 + ri * 20), value)
                page.insert_text((40, 20), "Repeated furniture")
            doc.save(path)
        self.run_tool("convert", path, "out.html")
        result = (self.root / "out.html").read_text()
        self.assertIn('rowspan="2"', result)
        self.assertIn('colspan="2"', result)
        self.assertIn("Information", result)
        self.assertEqual(result.count("Details"), 1)
        self.assertNotIn("Repeated furniture", result)
        self.assertEqual(result.count("<tr>"), 6)
        self.run_tool("check", path, "out.html", "--report", "pdf-check.json")
        self.run_tool("preview", path, "out.html", "preview.html")
        self.assertIn("data:image/png;base64,", (self.root / "preview.html").read_text())

    def test_pdf_table_records(self):
        path = self.root / "register.pdf"
        with pymupdf.open() as doc:
            for index in range(2):
                page = doc.new_page(width=360, height=450)
                y = 310 if index == 0 else 70
                page.draw_rect((40, y, 320, y + 80))
                page.draw_line((120, y), (120, y + 80))
                for offset in (20, 60):
                    page.draw_line((40, y + offset), (320, y + offset))
                for x, off, value in [(45, 14, "ID"), (125, 14, "Text"),
                                      (45, 34, str(index + 1)), (125, 34, "First"),
                                      (125, 48, "line")]:
                    page.insert_text((x, y + off), value)
            doc.save(path)
        self.run_tool("table", path, "rows.jsonl")
        rows = [json.loads(row) for row in (self.root / "rows.jsonl").read_text().splitlines()]
        self.assertEqual([r["values"] for r in rows],
                         [{"ID": "1", "Text": "First line"}, {"ID": "2", "Text": "First line"}])

    def convert_pdf_tables(self, fragments, decorate=None):
        path = self.root / "fragments.pdf"
        with pymupdf.open() as doc:
            for y, key_edge, rows in fragments:
                page = doc.new_page(width=400, height=500)
                bottom = y + 20 * len(rows)
                page.draw_rect((40, y, 360, bottom))
                page.draw_line((key_edge, y), (key_edge, bottom))
                for index, (key, detail) in enumerate(rows):
                    top = y + 20 * index
                    if index:
                        page.draw_line((40, top), (360, top))
                    for x, value in ((45, key), (key_edge + 5, detail)):
                        if value:
                            page.insert_text((x, top + 14), value)
                page.insert_text((40, 20), "Synthetic header")
                page.insert_text((40, 480), f"Synthetic footer {page.number + 1}")
                if decorate:
                    decorate(page, y, key_edge)
            doc.save(path)
        self.run_tool("convert", path, "out.html")
        self.run_tool("check", path, "out.html")
        return (self.root / "out.html").read_text()

    def test_pdf_table_across_three_pages(self):
        header = ("Key", "Details")
        result = self.convert_pdf_tables([
            (310, 100, [header, ("A", "First")]),
            (130, 102, [header, ("B", "Second")]),
            (130, 100, [header, ("C", "Third")]),
        ])
        self.assertEqual(result.count("<table>"), 1)
        self.assertEqual(result.count("<tr>"), 4)
        self.assertEqual(result.count("Details"), 1)
        self.assertNotIn("Synthetic header", result)
        self.assertNotIn("Synthetic footer", result)

    def test_pdf_row_continues_at_word_boundary(self):
        result = self.convert_pdf_tables([
            (310, 100, [("Key", "Details"), ("A", "First part")]),
            (130, 100, [("", "continues here."), ("B", "Second record")]),
        ])
        self.assertEqual(result.count("<table>"), 1)
        self.assertEqual(result.count("<tr>"), 3)
        self.assertIn("<td>A</td><td>First part continues here.</td>", result)

    def test_pdf_repeated_header_image_extending_into_body(self):
        logo = pymupdf.Pixmap(pymupdf.csRGB, pymupdf.IRect(0, 0, 12, 4), False)
        logo.clear_with(90)

        def add_logo(page, y, key_edge):
            page.insert_image((40, 46, 100, 66), pixmap=logo)

        result = self.convert_pdf_tables([
            (350, 100, [("Key", "Details"), ("A", "First")]),
            (95, 100, [("Section", "Next"), ("B", "Second")]),
        ], decorate=add_logo)
        self.assertEqual(result.count("<table>"), 1)
        self.assertEqual(result.count("<tr>"), 4)
        self.assertNotIn("<img", result)

    def test_pdf_close_extra_separator_across_page_break(self):
        def add_separator(page, y, key_edge):
            if page.number == 1:
                page.draw_line((key_edge + 1.8, y + 20), (key_edge + 1.8, y + 40))

        result = self.convert_pdf_tables([
            (350, 100, [("Key", "Details"), ("A", "First")]),
            (95, 100, [("Key", "Details"), ("B", "Second")]),
        ], decorate=add_separator)
        self.assertEqual(result.count("<table>"), 1)
        self.assertEqual(result.count("<tr>"), 3)
        self.assertIn("<td>B</td><td>Second</td>", result)

    def test_pdf_different_grids_stay_separate(self):
        header = ("Key", "Details")
        result = self.convert_pdf_tables([
            (400, 100, [header, ("A", "First table")]),
            (70, 140, [header, ("B", "Second table")]),
        ])
        self.assertEqual(result.count("<table>"), 2)
        self.assertEqual(result.count("<tr>"), 4)
        self.assertEqual(result.count("Details"), 2)

    def test_docx_table_check_and_preview(self):
        path = self.root / "document.docx"
        with ZipFile(path, "w") as package:
            package.writestr("word/document.xml", f"""<w:document xmlns:w="{W}"><w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Synthetic heading</w:t></w:r></w:p>
<w:p><w:pPr><w:pStyle w:val="TOC1"/></w:pPr><w:r><w:t>Generated entry</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:tcPr><w:gridSpan w:val="2"/><w:vMerge w:val="restart"/></w:tcPr>
<w:p><w:r><w:t>Sample &lt;tag&gt; sample</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:tcPr><w:gridSpan w:val="2"/><w:vMerge/></w:tcPr><w:p/></w:tc></w:tr>
</w:tbl></w:body></w:document>""")
            package.writestr("word/header1.xml", "<header>Furniture</header>")
        self.run_tool("convert", path, "out.html")
        output = self.root / "out.html"
        html = output.read_text()
        self.assertIn("<h1>Synthetic heading</h1>", html)
        self.assertIn('colspan="2" rowspan="2"', html)
        self.assertIn("Sample &lt;tag&gt; sample", html)
        self.assertNotIn("Generated entry", html)
        self.assertNotIn("Furniture", html)
        self.run_tool("check", path, output)
        output.write_text(html.replace(" sample", "") + "<script>sample</script>")
        result = self.run_tool("check", path, output, "--report", "report.json", code=1)
        self.assertNotIn("sample", result.stdout)
        report = json.loads((self.root / "report.json").read_text())
        self.assertEqual(report["missing_words"], {"sample": 1})
        self.assertEqual((self.root / "report.json").stat().st_mode & 0o777, 0o600)
        with output.open("a") as stream:
            stream.write('<img src="https://example.invalid/x" onerror="alert(1)">')
        self.run_tool("preview", path, output, "preview.html")
        preview = (self.root / "preview.html").read_text()
        self.assertIn("sandbox", preview)
        self.assertNotIn("alert(1)", preview)
        self.assertNotIn("example.invalid", preview)

    def test_docx_extracts_only_embedded_raster_images(self):
        path = self.root / "picture.docx"
        drawing = "http://schemas.openxmlformats.org/drawingml/2006/main"
        with pymupdf.open() as doc:
            page = doc.new_page(width=20, height=20)
            png = page.get_pixmap().tobytes("png")
        with ZipFile(path, "w") as package:
            package.writestr("word/document.xml", f"""<w:document xmlns:w="{W}"
                xmlns:a="{drawing}" xmlns:r="{R}"><w:body><w:p><w:r><w:drawing>
                <a:blip r:embed="local"/><a:blip r:embed="remote"/>
                </w:drawing></w:r></w:p></w:body></w:document>""")
            package.writestr("word/_rels/document.xml.rels", """<Relationships>
                <Relationship Id="local" Target="media/picture.png"/>
                <Relationship Id="remote" TargetMode="External" Target="https://example.invalid/image"/>
                </Relationships>""")
            package.writestr("word/media/picture.png", png)
        self.run_tool("convert", path, "out.html")
        self.assertEqual((self.root / "out.html.image-1.png").read_bytes(), png)
        output = (self.root / "out.html").read_text()
        self.assertIn('src="out.html.image-1.png"', output)
        self.assertNotIn("example.invalid", output)
        self.run_tool("check", path, "out.html", "--structure")
        self.run_tool("preview", path, "out.html", "picture-preview.html")
        self.assertEqual((self.root / "picture-preview.html").read_text().count("data:image/png;base64,"), 2)

        (self.root / "out.html.image-1.png").write_bytes(b"corrupted")
        self.run_tool("check", path, "out.html", "--structure", code=1)

    def test_grouped_drawing_preserves_visual_and_searchable_labels(self):
        from unittest.mock import patch
        sys.path.insert(0, str(TOOL.parent))
        import office
        path = self.root / "diagram.docx"
        with ZipFile(path, "w") as z:
            z.writestr("word/document.xml", f'<w:document xmlns:w="{W}" '
                       'xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006" '
                       'xmlns:g="http://schemas.microsoft.com/office/word/2010/wordprocessingGroup">'
                       '<w:body><w:p><w:r><mc:AlternateContent><mc:Choice><w:drawing><g:wgp>'
                       '<w:txbxContent><w:p><w:r><w:t>Example label</w:t></w:r></w:p></w:txbxContent>'
                       '</g:wgp></w:drawing></mc:Choice><mc:Fallback><w:r><w:t>Duplicate fallback</w:t>'
                       '</w:r></mc:Fallback></mc:AlternateContent></w:r></w:p></w:body></w:document>')
        with patch.object(office, "group_picture", return_value=b"synthetic") as render:
            html = office.docx(path, lambda data: "<img>" if data == b"synthetic" else "")
        render.assert_called_once()
        self.assertIn("<img>",html)
        self.assertEqual(html.count("Example label"),1)
        self.assertNotIn("Duplicate fallback",html)

    def test_referenced_word_parts_and_list(self):
        path = self.root / "sample.docx"
        with ZipFile(path, "w") as z:
            z.writestr("word/document.xml", f'<w:document xmlns:w="{W}" xmlns:r="{R}"><w:body>'
                       '<w:p><w:pPr><w:numPr><w:numId w:val="1"/></w:numPr></w:pPr>'
                       '<w:r><w:t>Item</w:t></w:r></w:p><w:sectPr><w:footerReference r:id="f"/>'
                       '<w:headerReference r:id="h"/></w:sectPr></w:body></w:document>')
            z.writestr("word/_rels/document.xml.rels", '<Relationships>'
                       '<Relationship Id="h" Target="header1.xml"/><Relationship Id="f" Target="footer1.xml"/>'
                       '</Relationships>')
            for tag, text in (("header", "Revision 2"), ("footer", "Released")):
                z.writestr(f"word/{tag}1.xml", f'<w:{tag} xmlns:w="{W}"><w:p><w:r>'
                           f'<w:t>{text}</w:t></w:r></w:p></w:{tag}>')
        self.run_tool("convert", path, "result.html")
        out = self.root / "result.html"
        self.assertIn("<li>", out.read_text())
        self.run_tool("check", path, out, "--structure")
        original = out.read_text()
        out.write_text(original.replace("<li>", "<p>").replace("</li>", "</p>"))
        self.run_tool("check", path, out, "--structure", code=1)
        out.write_text(original.replace("Released", ""))
        self.run_tool("check", path, out, code=1)

    def test_first_page_header_needs_title_page(self):
        sys.path.insert(0, str(TOOL.parent))
        import office
        for title_page, shown in (("", False), ('<w:titlePg w:val="0"/>', False), ("<w:titlePg/>", True)):
            path = self.root / "first.docx"
            with ZipFile(path, "w") as z:
                z.writestr("word/document.xml", f'<w:document xmlns:w="{W}" xmlns:r="{R}"><w:body>'
                           '<w:p><w:r><w:t>Body text</w:t></w:r></w:p><w:sectPr>'
                           '<w:headerReference w:type="first" r:id="first"/>'
                           f'<w:headerReference w:type="default" r:id="default"/>{title_page}'
                           '</w:sectPr></w:body></w:document>')
                z.writestr("word/_rels/document.xml.rels", '<Relationships>'
                           '<Relationship Id="first" Target="header1.xml"/>'
                           '<Relationship Id="default" Target="header2.xml"/></Relationships>')
                for name, text in (("header1", "Synthetic cover header"), ("header2", "Synthetic running header")):
                    z.writestr(f"word/{name}.xml", f'<w:hdr xmlns:w="{W}"><w:p><w:r>'
                               f'<w:t>{text}</w:t></w:r></w:p></w:hdr>')
            html = office.docx(path)
            self.assertIn("Synthetic running header", html)
            self.assertEqual("Synthetic cover header" in html, shown, title_page)

    def test_excel_calendar_formats_and_numeric_literals(self):
        sys.path.insert(0, str(TOOL.parent))
        from office import display_cell
        self.assertEqual(display_cell("59", "14", False), "1900-02-28")
        self.assertEqual(display_cell("61", "14", False), "1900-03-01")
        self.assertEqual(display_cell("2.5", "yyyy-mm-dd hh:mm", True), "1904-01-03 12:00:00")
        for fmt in ('0 "days"', '0.00 USD', '0%', 'hh:mm'):
            self.assertEqual(display_cell("36526", fmt, False), "36526")

    @unittest.skipUnless(__import__("shutil").which("soffice"), "requires LibreOffice")
    def test_legacy_workbook_merges_and_check(self):
        sys.path.insert(0, str(TOOL.parent))
        from office_bridge import export
        xlsx = self.root / "sample.xlsx"
        with ZipFile(xlsx, "w") as z:
            z.writestr("[Content_Types].xml", '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
                       '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>'
                       '<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>'
                       '<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>'
                       '<Override PartName="/xl/worksheets/s.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>')
            for part, kind, target in (("_rels/.rels", "officeDocument", "xl/workbook.xml"),
                                       ("xl/_rels/workbook.xml.rels", "worksheet", "worksheets/s.xml")):
                styles_rel = f'<Relationship Id="b" Type="{R}/styles" Target="styles.xml"/>' if kind == "worksheet" else ""
                z.writestr(part, '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
                           f'<Relationship Id="a" Type="{R}/{kind}" Target="{target}"/>{styles_rel}</Relationships>')
            z.writestr("xl/styles.xml", f'<styleSheet xmlns="{S}"><cellXfs count="2">'
                       '<xf numFmtId="0"/><xf numFmtId="14" applyNumberFormat="1"/></cellXfs></styleSheet>')
            z.writestr("xl/workbook.xml", f'<workbook xmlns="{S}" xmlns:r="{R}"><sheets>'
                       '<sheet name="Grid" sheetId="1" r:id="a"/></sheets></workbook>')
            z.writestr("xl/worksheets/s.xml", f'<worksheet xmlns="{S}"><sheetData><row r="1">'
                       '<c r="A1" t="inlineStr"><is><t>First&#10;Second</t></is></c>'
                       '<c r="C1" s="1"><v>36526</v></c><c r="D1"><v>36526</v></c></row></sheetData>'
                       '<mergeCells count="1"><mergeCell ref="A1:B1"/></mergeCells></worksheet>')
        legacy = self.root / "sample.xls"
        export(xlsx, legacy)
        self.assertTrue(legacy.read_bytes().startswith(bytes.fromhex("d0cf11e0")))
        self.run_tool("table", legacy, "grid.json", "--workbook")
        out = self.root / "grid.json"
        sheet = json.loads(out.read_text())["sheets"][0]
        self.assertEqual(sheet["rows"]["1"]["A"], "First\nSecond")
        self.assertEqual(sheet["rows"]["1"]["C"], "2000-01-01")
        self.assertEqual(sheet["rows"]["1"]["D"], "36526")
        self.assertEqual(sheet["merges"], ["A1:B1"])
        self.run_tool("check", legacy, out)
        out.write_text(out.read_text().replace("A1:B1", "A1:C1"))
        self.run_tool("check", legacy, out, code=1)

    def test_xlsx_multiple_sheets_and_csv_multiline(self):
        path = self.root / "register.xlsx"
        with ZipFile(path, "w") as package:
            package.writestr("xl/workbook.xml", f'<workbook xmlns="{S}" xmlns:r="{R}"><workbookPr date1904="1"/><sheets>'
                             '<sheet name="First" sheetId="1" r:id="a"/>'
                             '<sheet name="Second" state="hidden" sheetId="2" r:id="b"/></sheets></workbook>')
            package.writestr("xl/_rels/workbook.xml.rels",
                             f'<Relationships><Relationship Id="a" Type="{R}/worksheet" Target="worksheets/a.xml"/>'
                             f'<Relationship Id="b" Type="{R}/worksheet" Target="/xl/worksheets/b.xml"/>'
                             '</Relationships>')
            package.writestr("xl/styles.xml", f'<styleSheet xmlns="{S}"><numFmts>'
                             '<numFmt numFmtId="164" formatCode="dd/mm;@"/></numFmts>'
                             '<cellXfs><xf numFmtId="0"/><xf numFmtId="164"/></cellXfs></styleSheet>')
            for sheet in ("a", "b"):
                package.writestr(f"xl/worksheets/{sheet}.xml",
                                 f'<worksheet xmlns="{S}"><sheetData><row r="2">'
                                 '<c r="A2" t="inlineStr"><is><t>Key</t></is></c>'
                                 '<c r="B2" t="inlineStr"><is><t>Text</t></is></c></row><row r="3">'
                                 '<c r="A3" s="1"><f>1+1</f><v>2</v></c><c r="B3" t="inlineStr"><is>'
                                 '<t>First&#10;line</t></is></c></row><row r="4">'
                                 '<c r="A4" s="1" t="str"><v>36526</v></c></row></sheetData></worksheet>')
        self.run_tool("table", path, "workbook.json", "--workbook")
        book = json.loads((self.root / "workbook.json").read_text())
        self.assertEqual(book["sheets"][1]["state"], "hidden")
        self.assertEqual(book["sheets"][0]["rows"]["3"]["B"], "First\nline")
        self.run_tool("check", path, "workbook.json")
        self.run_tool("table", path, "rows.jsonl", "--header-row", "2")
        rows = [json.loads(row) for row in (self.root / "rows.jsonl").read_text().splitlines()]
        self.assertEqual([r["source"] for r in rows], [{"sheet": name, "row": n}
                                                      for name in ("First", "Second") for n in (3, 4)])
        self.assertEqual(rows[0]["values"], {"Key": "1904-01-03", "Text": "First\nline"})
        self.assertEqual(rows[1]["values"]["Key"], "36526")
        self.run_tool("preview", path, "workbook.json", "book-preview.html")
        self.assertEqual((self.root / "book-preview.html").read_text().count("36526"), 4)
        self.run_tool("preview", path, "rows.jsonl", "register-preview.html")
        self.assertIn("Second · 3", (self.root / "register-preview.html").read_text())
        source = self.root / "data.csv"
        source.write_text('ID;Name\n1;"First\nsecond"\n', encoding="utf-8")
        self.run_tool("table", source, "csv.jsonl", "--delimiter", ";")
        row = json.loads((self.root / "csv.jsonl").read_text())
        self.assertEqual(row["values"], {"ID": "1", "Name": "First\nsecond"})


if __name__ == "__main__":
    unittest.main()
