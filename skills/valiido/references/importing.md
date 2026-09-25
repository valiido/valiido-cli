# Importing existing customer data

Use this workflow whenever Valiido receives an existing customer dataset. Keep
source files and all derived working files outside source repositories. Protect
temporary files and never place customer content, names, credentials, tokens, or
filenames in skills, documentation, tests, fixtures, logs, or command examples.

## Inspect before writing

1. Inventory every source file, sheet, section, and attachment. Ignore temporary
   office lock files, empty rows, repeated headings, totals, explanatory sample
   rows, and other non-record content.
2. Discover the live commands and schemas for every intended resource type.
3. List all available examples for each target type. Build a source-to-target
   mapping that records the resource type, suitable example if any, field map,
   unmapped fields, and expected record count.
4. Identify duplicate source records and overlap between files before import.
   Do not infer that repeated rows are separate records merely because they
   occur in separate sheets or files.
5. Search both active records and trash before creating anything. Restore and
   adapt an exact prior record instead of creating a duplicate.

## Choose the correct creation path

- Use `copy_from_example` when an example is genuinely the same kind of policy,
  procedure, task, risk, checklist, or other resource. Read the copied record,
  then update that record with the complete customer data. Use `copy_again` only
  for distinct records that legitimately derive from the same example.
- Do not attach an unrelated example just to make a reference exist. Create
  without an example only when the catalog has no suitable match, and include
  that gap in the final report.
- For documents, prefer an exact example, then the closest example in the same
  document domain. Preserve the example reference while replacing its content.
- Spreadsheet rows belong in their native resource types when their fields map
  reliably. Retain a document copy only when the source itself is a controlled
  record whose original layout or context matters.
- Treat a table row as one native record when it has its own identity. Convert
  subordinate tables and checklist rows to real child resources. Keep source
  sheet and row references in the mapping so errors and verification point back
  to the exact source.

## Preserve customer material

- Decide explicitly for each document whether its value lies in editable
  content or in the original artifact, and honor the user's choice. Policies,
  procedures, manuals, plans, and other structured text are extraction-worthy:
  convert readable Word and PDF sources to safe, complete HTML so users can
  search and edit them in Valiido. Preserve headings, paragraphs, lists,
  tables, links, and meaningful metadata; remove generated tables of contents,
  repeated headers and footers, page numbers, blank pages, and conversion
  debris. Visually compare the result with the complete source.
- Keep the source as a PDF document when the original itself is the evidence or
  when extraction would materially lose meaning or authenticity, such as a
  handwritten or scanned protocol, signed record, completed form, certificate,
  drawing, or layout-dependent evidence. Do not choose PDF merely because the
  source file is a PDF. When the user's intent is unclear and the distinction
  affects usefulness, ask before import.
- If a PDF document is the right choice, visually inspect it before upload. If
  an example was copied, set `document_type` to `pdf`, clear the copied rich-text
  content, and assign the uploaded `pdf_file` in the same update so the example
  reference remains attached.
- Use only attachment identifiers returned by `valiido document upload` for the
  active tenant. Never submit a local path or invented identifier.
- Preserve every labeled source value. Map supported values to native fields and
  append remaining values to one structured import comment. If comments are not
  supported, use an appropriate free-text field or report the gap. Never discard
  a value silently.
- Put provenance and unmapped values on the resource they describe: shared
  template context belongs on the parent; item-specific values belong on the
  child. Do not invent an empty child merely to hold a parent-level comment.

## Local converters

Run the bundled commands from the skill directory with isolated Python. Use a
local virtual environment or the same dependencies installed system-wide.

```sh
skill="$PWD"
python3 -m venv scripts/.venv
scripts/.venv/bin/python -I -m pip install -r scripts/requirements.txt
work=$(mktemp -d)
cd "$work"
```

Place working copies of the inputs in the private directory before running the
commands. Each output command creates a new file.

```sh
"$skill/scripts/.venv/bin/python" -I "$skill/scripts/import_tool.py" convert procedure.docx procedure.html
"$skill/scripts/.venv/bin/python" -I "$skill/scripts/import_tool.py" check procedure.docx procedure.html --report words.json
"$skill/scripts/.venv/bin/python" -I "$skill/scripts/import_tool.py" preview procedure.docx procedure.html review.html
"$skill/scripts/.venv/bin/python" -I "$skill/scripts/import_tool.py" table inventory.xlsx rows.jsonl --header-row 2
"$skill/scripts/.venv/bin/python" -I "$skill/scripts/import_tool.py" preview inventory.xlsx rows.jsonl inventory-review.html
```

- Use `convert` for editable PDF or DOCX. Extracted images are private sibling
  files. Upload approved images before the write and replace the local image
  references with the corresponding ActionText attachments.
- Run `check` on every rich-text conversion before import, and again on the
  stored HTML during verification. Exit 1 means source words are missing.
  Review the private report and resolve discrepancies before proceeding.
- Use `table` for XLSX, UTF-8 CSV or ruled PDF tables. Select an XLSX sheet with
  `--sheet` or set the CSV delimiter with `--delimiter`. The JSONL output
  contains source references and column values. Review which rows are records,
  map fields and types against the live schema, and assemble the JSON input for
  `resource import --dry-run`.
- Open the local `preview` and get human approval before writing. Check merged
  cells, page boundaries, record counts and images against the source. PDF
  previews render the pages. DOCX and spreadsheet previews show source content
  and structure. Compare Word layout in the original document when it matters.

## Execute safely

- Keep customer JSON in mode-0600 files and use `--input-file`. For example,
  prepare a synthetic task with `valiido tasks create --input-file task.json`.
  Use `--json` when recording results for automation.
- Keep an idempotent local state file that maps a stable source key to the
  tenant-scoped record id and completed operations. On restart, read and verify
  that record before continuing; do not identify records by similar titles.
- For bulk imports, validate the complete JSON file with `--dry-run`, review its
  counts and representative previews, then prepare and confirm every batch.
- A prepared write has not happened yet. When `command run` returns
  `pending_confirmation`, review it and call `command confirm` when the user has
  authorized that exact change. Verify the resulting resource rather than
  treating preparation as success. If confirmation ends with an ambiguous
  network or not-found error, check the record using a read or list before
  preparing another write. A consumed token cannot create a second record.
- Upload retries reuse the same storage location and bytes. If upload
  initialization fails without returning attachment identifiers, record the
  failure in the import state before starting another upload.
- Use `resource import` for homogeneous new records. Use individual commands for
  template copies, updates, relationships, attachments, parent/child resources,
  and mixed imports whose progress must be resumed independently.
- Read current versions immediately before dependent updates. On a conflict,
  stop and compare instead of overwriting another change.
- Do not remove or replace existing customer records unless the request clearly
  authorizes that mutation and the source material remains available.
- Continue with independent source groups after a record-level failure when the
  approved plan permits it. Collect every failure and leave failed operations
  retryable instead of reporting partial work as complete.

## Mandatory quality gate

Before reporting success, verify all of the following through the CLI:

- Actual counts match the source-to-target mapping.
- Separate source records from system-generated records when reconciling counts.
  For example, completing a recurring task can create its next occurrence.
- No header, sample, total, blank, or duplicate rows became resources.
- Every resource with a suitable example has the intended example reference;
  every resource without one has a documented reason.
- At least three representative records per imported resource type were read
  back in full, including one complex record when present.
- Imported documents were opened or rendered and visually checked; rich text has
  no conversion debris and preserves the source structure, while retained PDFs
  are readable, attached to the right record, and have a documented reason for
  remaining artifacts instead of editable content.
- Relationships, comments, enums, dates, and tenant-scoped identifiers survived
  the round trip.
- Account for canonical system behavior when comparing results. Derived fields
  such as risk state may legitimately differ from requested aliases; verify the
  source values remain represented and report any material semantic change.
- No source value was silently lost and no customer material entered a source
  repository or retained command line.

If any check fails, describe the import as incomplete, correct the affected
records, and repeat the gate. Report final counts, unmatched examples, preserved
comments, skipped source rows, and any remaining product gaps.
