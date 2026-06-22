package markdown

import (
	"strings"
)

// tablesToRecordLists rewrites GitHub-flavored markdown pipe tables into a
// width-agnostic record list BEFORE handing the source to glamour. Reason:
// a fixed-width grid table shreds in the narrow inline
// cockpit, and glamour does not table-wrap safely at narrow widths either. The
// record-list form:
//
//	| Name | Role  | Note |   →   - Name        (first column → bulleted heading)
//	|------|-------|------|         Role: ...   (remaining columns → "Header: value")
//	| Ada  | Eng   | ...  |         Note: ...
//
// Empty cells are skipped; inline styling (the cell text) is preserved verbatim
// so glamour still bolds/links inside the values. We operate line-wise on the raw
// markdown — cheap, no parser, and it leaves all non-table content untouched.
func tablesToRecordLists(src string) string {
	lines := strings.Split(src, "\n")
	var out []string

	i := 0
	for i < len(lines) {
		// A table starts at a pipe row immediately followed by a delimiter row
		// (---|--- with optional colons). Detect that 2-line header signature.
		if isTableRow(lines[i]) && i+1 < len(lines) && isDelimiterRow(lines[i+1]) {
			header := splitRow(lines[i])
			i += 2 // consume header + delimiter
			var body [][]string
			for i < len(lines) && isTableRow(lines[i]) {
				body = append(body, splitRow(lines[i]))
				i++
			}
			out = append(out, renderRecordList(header, body)...)
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

// isTableRow reports whether a line looks like a pipe table row (contains a pipe
// and isn't a code fence). We trim first so leading indentation doesn't fool us.
func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "|") {
		return false
	}
	if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
		return false
	}
	return true
}

// isDelimiterRow matches the table header separator (---|:--:|---), allowing
// colons for alignment and surrounding pipes/spaces. Every cell must be a run of
// dashes (with optional colons) — that's what distinguishes it from a data row.
func isDelimiterRow(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "-") || !strings.Contains(t, "|") {
		return false
	}
	for _, cell := range splitRow(line) {
		c := strings.TrimSpace(cell)
		if c == "" {
			continue
		}
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

// splitRow splits a pipe-table row into trimmed cells, dropping the empty cells
// produced by leading/trailing pipes.
func splitRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// renderRecordList turns a parsed header + body into the markdown record list:
// each row becomes a bullet whose first column is the heading and whose remaining
// columns are indented "Header: value" lines (empty values skipped). The result
// is plain markdown that glamour renders with normal list/emphasis styling.
func renderRecordList(header []string, body [][]string) []string {
	var out []string
	for _, row := range body {
		if len(row) == 0 {
			continue
		}
		// First column → bulleted heading. Fall back to a dash if it's empty so
		// the record still has an anchor line.
		head := row[0]
		if strings.TrimSpace(head) == "" {
			head = "—"
		}
		out = append(out, "- "+head)
		for c := 1; c < len(row); c++ {
			val := strings.TrimSpace(row[c])
			if val == "" {
				continue // skip empty cells
			}
			label := ""
			if c < len(header) {
				label = strings.TrimSpace(header[c])
			}
			if label != "" {
				out = append(out, "  "+label+": "+val)
			} else {
				out = append(out, "  "+val)
			}
		}
	}
	// A trailing blank line so the list closes cleanly against following prose.
	out = append(out, "")
	return out
}
