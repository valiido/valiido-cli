package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

func renderReadable(writer *output, response response, result resultView, kind string, markdown bool) error {
	width, tty := writerTerminal(writer.Writer)
	if width < 1 {
		width = 100
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	view := readableView{markdown: markdown, width: width, relative: tty && !markdown, now: time.Now(),
		bold: tty && !markdown && !noColor && os.Getenv("TERM") != "dumb"}

	summary := response.Summary
	if result.listing && result.count == 0 {
		summary = "No results"
	}
	if response.DryRun && !strings.Contains(strings.ToLower(summary), "preview") {
		summary = "Dry run · " + summary
	}
	if markdown {
		fmt.Fprintln(&view, "### "+markdownCell(summary))
	} else {
		fmt.Fprintln(&view, view.header(fitDisplay(cleanDisplay(summary), width)))
	}
	if result.listing && len(result.rows) > 0 {
		view.writeRows(result, kind)
	} else if !result.listing {
		view.writeFields(result.object, response.URL == "")
	}
	if response.URL != "" && result.object["url"] != response.URL {
		fmt.Fprintln(&view, cleanDisplay(response.URL))
	}
	if response.Warnings != nil {
		for _, warning := range *response.Warnings {
			view.note("Warning", warning)
		}
	}
	for _, crumb := range response.Breadcrumbs {
		if crumb.Action == "load_more" || crumb.Action == "discover" {
			view.note("Next", crumb.Command)
		}
	}
	if crumbs := confirmationBreadcrumbs(result, writer.profile); len(crumbs) > 0 {
		field := ".data.confirmation_token"
		if result.object["batches"] != nil {
			field = ".data.batches[].confirmation_token"
			view.note("Confirm each batch", crumbs[0].Command)
			view.note("Token output", "--jq '"+field+"'")
		} else {
			view.note("Confirm", crumbs[0].Command)
			view.note("Token output", "--json")
		}
	}
	_, err := view.WriteTo(writer)
	return err
}

type readableView struct {
	bytes.Buffer
	markdown, relative, bold bool
	width                    int
	now                      time.Time
}

func (v *readableView) header(value string) string {
	if v.bold {
		return "\x1b[1m" + value + "\x1b[0m"
	}
	return value
}

func (v *readableView) note(label, value string) {
	if value == "" {
		return
	}
	if v.markdown {
		fmt.Fprintf(v, "\n**%s** %s\n", label, markdownCell(value))
	} else {
		fmt.Fprintf(v, "%s  %s\n", label, cleanDisplay(value))
	}
}

type tableColumn struct {
	name  string
	width int
}

func (v *readableView) writeRows(result resultView, kind string) {
	rows, names := v.table(result, kind)
	if v.markdown {
		fmt.Fprintf(v, "\n| %s |\n| %s |\n", strings.Join(names, " | "), strings.TrimSuffix(strings.Repeat("--- | ", len(names)), " | "))
		for _, row := range rows {
			for i := range row {
				row[i] = markdownCell(row[i])
			}
			fmt.Fprintf(v, "| %s |\n", strings.Join(row, " | "))
		}
		return
	}
	columns := make([]tableColumn, len(names))
	for i, name := range names {
		columns[i] = tableColumn{strings.ToUpper(name), len(name)}
		for _, row := range rows {
			row[i] = cleanDisplay(row[i])
			columns[i].width = max(columns[i].width, displayWidth(row[i]))
		}
	}
	// Keep the identifier and label visible. Drop secondary columns when space is tight.
	label := 0
	if names[0] == "ID" || names[0] == "Batch" {
		label = 1
		if columns[0].width+10 > v.width {
			for _, row := range rows {
				fmt.Fprintln(v, row[0])
				fmt.Fprintln(v, fitDisplay(row[1], v.width))
			}
			return
		}
	}
	columns[label].width = min(columns[label].width, 70)
	available := v.width
	for i, column := range columns {
		if i != label {
			available -= column.width + 2
		}
	}
	for len(columns) > label+1 && available < 20 {
		last := len(columns) - 1
		available += columns[last].width + 2
		columns = columns[:last]
	}
	columns[label].width = min(columns[label].width, max(1, available))
	headers := make([]string, len(columns))
	for i, column := range columns {
		headers[i] = column.name
	}
	fmt.Fprintln(v, v.header(tableLine(headers, columns)))
	for _, row := range rows {
		fmt.Fprintln(v, tableLine(row, columns))
	}
}

func tableLine(values []string, columns []tableColumn) string {
	var line strings.Builder
	for i, column := range columns {
		value := fitDisplay(values[i], column.width)
		line.WriteString(value)
		if i < len(columns)-1 {
			line.WriteString(strings.Repeat(" ", column.width-displayWidth(value)+2))
		}
	}
	return strings.TrimRight(line.String(), " ")
}

func (v *readableView) table(result resultView, kind string) ([][]string, []string) {
	rows := make([][]string, 0, len(result.rows))
	names := []string{"ID", "Name"}
	switch {
	case result.object["commands"] != nil:
		names = []string{"Command", "Risk", "Description"}
	case result.object["batches"] != nil:
		names = []string{"Batch", "Result", "Rows"}
	case result.object["skills"] != nil:
		names = []string{"Skill"}
	case result.object["access_levels"] != nil:
		names = []string{"Key", "Name"}
	default:
		hasStatus, hasDate := false, false
		for _, row := range result.rows {
			obj, _ := row.value.(map[string]any)
			hasStatus = hasStatus || statusLabel(obj, kind) != ""
			hasDate = hasDate || rowField(obj, dateFields...) != nil
		}
		if hasStatus {
			names = append(names, "Status")
		}
		if hasDate {
			names = append(names, "Date")
		}
	}
	for _, row := range result.rows {
		obj, _ := row.value.(map[string]any)
		var cells []string
		switch names[0] {
		case "Command":
			cells = []string{fieldDisplay(obj["name"]), fieldDisplay(obj["risk"]), fieldDisplay(obj["description"])}
		case "Batch":
			cells = []string{fieldDisplay(obj["index"]), fieldDisplay(obj["summary"]), fieldDisplay(obj["count"])}
		case "Skill":
			cells = []string{fieldDisplay(row.value)}
		case "Key":
			cells = []string{fieldDisplay(obj["key"]), rowLabel(row.value)}
		default:
			cells = []string{row.id, rowLabel(row.value)}
			for _, name := range names[2:] {
				switch name {
				case "Status":
					cells = append(cells, statusLabel(obj, kind))
				case "Date":
					cells = append(cells, v.date(fieldDisplay(rowField(obj, dateFields...))))
				}
			}
		}
		for i := range cells {
			if cells[i] == "" {
				cells[i] = "—"
			}
		}
		rows = append(rows, cells)
	}
	return rows, names
}

var dateFields = []string{"updated_at", "created_at", "published_at", "analysis_timestamp", "date"}

func rowField(obj map[string]any, keys ...string) any {
	attributes, _ := obj["attributes"].(map[string]any)
	for _, key := range keys {
		if value := obj[key]; value != nil {
			return value
		}
		if value := attributes[key]; value != nil {
			return value
		}
	}
	return nil
}

func statusLabel(obj map[string]any, kind string) string {
	if complete, ok := rowField(obj, "complete").(bool); kind == "tasks" && ok {
		if complete {
			return "Done"
		}
		return "Open"
	}
	if copied, ok := obj["copied"].(bool); ok {
		if copied {
			return "Copied"
		}
		return "Available"
	}
	if active, ok := rowField(obj, "active").(bool); kind == "employees" && ok {
		if active {
			return "Active"
		}
		return "Inactive"
	}
	if running, ok := obj["running"].(bool); ok && running {
		return "Running"
	}
	return strings.ReplaceAll(fieldDisplay(rowField(obj, "status", "kind", "level", "rating")), "_", " ")
}

func (v *readableView) writeFields(obj map[string]any, showURL bool) {
	fields := make(map[string]any, len(obj))
	attributes, _ := obj["attributes"].(map[string]any)
	for key, value := range attributes {
		fields[key] = value
	}
	for key, value := range obj {
		switch key {
		case "attributes":
		case "input":
			input, _ := value.(map[string]any)
			for key, value := range input {
				fields["input "+key] = value
			}
		default:
			fields[key] = value
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		name := strings.TrimPrefix(key, "input ")
		switch {
		case strings.HasSuffix(name, "_token"), name == "signed_id", name == "attachable_sgid",
			name == "url" && !showURL, name == "content" && fields["content_text"] != nil:
			continue
		}
		if fields[key] != nil {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := compactField(fields[key])
		if strings.HasSuffix(key, "_at") || strings.HasSuffix(key, "_timestamp") || key == "date" {
			value = v.date(value)
		}
		label := strings.ReplaceAll(key, "_", " ")
		if v.markdown {
			fmt.Fprintf(v, "- **%s** %s\n", markdownCell(label), markdownCell(value))
		} else {
			label = fitDisplay(cleanDisplay(label), max(1, v.width/2))
			value = cleanDisplay(value)
			if key != "url" && key != "saved_to" && key != "output" {
				value = fitDisplay(value, max(1, v.width-displayWidth(label)-2))
			}
			fmt.Fprintf(v, "%s  %s\n", label, value)
		}
	}
}

func compactField(value any) string {
	switch value := value.(type) {
	case []any:
		items := make([]string, 0, len(value))
		for _, item := range value {
			switch item.(type) {
			case map[string]any, []any:
				return fmt.Sprintf("%d items", len(value))
			}
			items = append(items, fieldDisplay(item))
		}
		return strings.Join(items, ", ")
	case map[string]any:
		if filename, ok := value["filename"].(string); ok {
			return filename
		}
		keys := make([]string, 0, len(value))
		for key, field := range value {
			switch field.(type) {
			case string, json.Number, bool:
				if !strings.HasSuffix(key, "_token") && key != "signed_id" && key != "attachable_sgid" {
					keys = append(keys, key)
				}
			}
		}
		if len(keys) == 0 {
			return fmt.Sprintf("%d fields", len(value))
		}
		sort.Strings(keys)
		items := make([]string, 0, len(keys))
		for _, key := range keys {
			items = append(items, strings.ReplaceAll(key, "_", " ")+" "+fieldDisplay(value[key]))
		}
		return strings.Join(items, ", ")
	}
	if text, ok := value.(string); ok && htmlTag.MatchString(text) {
		// Terminals get the text of rich-text fields; --json keeps the HTML.
		return strings.Join(strings.Fields(html.UnescapeString(htmlTag.ReplaceAllString(text, " "))), " ")
	}
	return fieldDisplay(value)
}

var htmlTag = regexp.MustCompile(`</?[a-zA-Z][^<>]*>`)

func (v *readableView) date(value string) string {
	if !v.relative {
		return value
	}
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	age := v.now.Sub(timestamp)
	span := age.Abs()
	suffix := " ago"
	if age < 0 {
		suffix = " from now"
	}
	switch {
	case span < time.Minute:
		return "now"
	case span < time.Hour:
		return fmt.Sprintf("%dm%s", span/time.Minute, suffix)
	case span < 24*time.Hour:
		return fmt.Sprintf("%dh%s", span/time.Hour, suffix)
	case span < 30*24*time.Hour:
		return fmt.Sprintf("%dd%s", span/(24*time.Hour), suffix)
	default:
		return timestamp.Format(time.DateOnly)
	}
}

func rowLabel(value any) string {
	if obj, ok := value.(map[string]any); ok {
		for _, key := range []string{"label", "title", "name", "summary", "key"} {
			if label, ok := rowField(obj, key).(string); ok && label != "" {
				return label
			}
		}
		return ""
	}
	return compactField(value)
}

func fieldDisplay(value any) string {
	if value == nil {
		return ""
	}
	if str, ok := value.(string); ok {
		return str
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func cleanDisplay(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (unicode.Is(unicode.Cf, r) && r != '\u200d' && r != '\u200c') {
			return ' '
		}
		return r
	}, s)
}

func markdownCell(s string) string {
	return markdownEscapes.Replace(cleanDisplay(s))
}

var markdownEscapes = strings.NewReplacer(
	"\\", "\\\\", "|", "\\|", "&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "\\`",
	"[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_", "!", "\\!", "#", "\\#",
)

// Combine common terminal clusters such as accent marks, emoji joined with
// ZWJ, and flag pairs before measuring or truncating a label.
func displayClusters(s string, visit func(string, int) bool) {
	start, width := 0, 0
	joined, regional := false, false
	for index, r := range s {
		flag := r >= 0x1f1e6 && r <= 0x1f1ff
		combining := unicode.IsMark(r) || r == 0x200d || r == 0x200c ||
			r >= 0x1f3fb && r <= 0x1f3ff || r >= 0xe0020 && r <= 0xe007f
		if index > start && !combining && !joined && !(flag && regional) {
			if !visit(s[start:index], width) {
				return
			}
			start, width = index, 0
		}
		switch {
		case r == 0xfe0f || r == 0x20e3:
			width = max(width, 2)
		case combining:
		case flag || wideRune(r):
			width = max(width, 2)
		default:
			width = max(width, 1)
		}
		joined = r == 0x200d
		regional = flag && !regional
	}
	if s != "" {
		visit(s[start:], width)
	}
}

func wideRune(r rune) bool {
	return r >= 0x1100 && r <= 0x115f || r >= 0x2329 && r <= 0x232a ||
		r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 ||
		r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f ||
		r >= 0xff00 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 ||
		r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3fffd
}

func displayWidth(s string) int {
	width := 0
	displayClusters(s, func(_ string, cells int) bool { width += cells; return true })
	return width
}

func fitDisplay(s string, width int) string {
	if displayWidth(s) <= width {
		return s
	}
	if width < 1 {
		return ""
	}
	var clipped strings.Builder
	remaining := width - 1
	displayClusters(s, func(cluster string, cells int) bool {
		if cells > remaining {
			return false
		}
		clipped.WriteString(cluster)
		remaining -= cells
		return true
	})
	clipped.WriteRune('…')
	return clipped.String()
}
