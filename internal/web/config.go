package web

import (
	"html/template"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
)

// configData is the model for the read-only config viewer modal.
type configData struct {
	Path  string
	YAML  template.HTML // highlighted YAML, or empty when Error is set
	Error string
}

// handleConfig returns the config-viewer modal fragment: the running config's
// path and its file contents, syntax-highlighted, read-only. It's a fragment so
// htmx can swap it into the page's modal container.
func (s *Web) handleConfig(w http.ResponseWriter, r *http.Request) {
	data := configData{Path: s.cfgPath}
	if raw, err := os.ReadFile(s.cfgPath); err != nil {
		data.Error = err.Error()
	} else {
		data.YAML = highlightYAML(string(raw))
	}

	// The config modal is shared chrome (opened from the topbar gear on any
	// page); render it from any set that has the partial — feedTmpl will do.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := feedTmpl.ExecuteTemplate(w, "configModal", data); err != nil {
		// Status is likely already written; log rather than double-write.
		log.Error().Err(err).Msg("render config modal")
	}
}

// highlightYAML renders YAML source to HTML with token spans, line by line so
// the original formatting and comments are preserved verbatim. It's a small,
// pragmatic highlighter (keys, strings, numbers, booleans, comments) — enough
// for viewing a config, not a full YAML parser.
func highlightYAML(src string) template.HTML {
	lines := strings.Split(src, "\n")
	var b strings.Builder
	for i, line := range lines {
		b.WriteString(highlightLine(line))
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return template.HTML(b.String())
}

func highlightLine(line string) string {
	code, comment := splitComment(line)
	var b strings.Builder
	b.WriteString(highlightCode(code))
	if comment != "" {
		b.WriteString(`<span class="yml-comment">`)
		b.WriteString(template.HTMLEscapeString(comment))
		b.WriteString(`</span>`)
	}
	return b.String()
}

// splitComment splits a line into its code and trailing comment, where a comment
// is a '#' at the line start or preceded by whitespace and not inside quotes
// (so '#' inside a URL or quoted string isn't mistaken for one).
func splitComment(line string) (code, comment string) {
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i], line[i:]
			}
		}
	}
	return line, ""
}

// ymlKey matches an indented (optionally list-dashed) "key:" prefix.
var ymlKey = regexp.MustCompile(`^(\s*(?:- )?)([A-Za-z0-9_.-]+)(:)(.*)$`)

func highlightCode(s string) string {
	m := ymlKey.FindStringSubmatch(s)
	if m == nil {
		// Not a key line (e.g. a flow-map feed entry); still colour its values.
		return highlightValue(s)
	}
	var b strings.Builder
	b.WriteString(template.HTMLEscapeString(m[1])) // indent / dash
	b.WriteString(`<span class="yml-key">`)
	b.WriteString(template.HTMLEscapeString(m[2])) // key
	b.WriteString(`</span>`)
	b.WriteString(m[3]) // colon
	b.WriteString(highlightValue(m[4]))
	return b.String()
}

// highlightValue colours quoted strings, then bare booleans/numbers in the rest.
func highlightValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if c := s[i]; c == '"' || c == '\'' {
			j := i + 1
			for j < len(s) && s[j] != c {
				j++
			}
			if j < len(s) {
				j++ // include the closing quote
			}
			b.WriteString(`<span class="yml-string">`)
			b.WriteString(template.HTMLEscapeString(s[i:j]))
			b.WriteString(`</span>`)
			i = j
			continue
		}
		j := i
		for j < len(s) && s[j] != '"' && s[j] != '\'' {
			j++
		}
		b.WriteString(highlightBareRun(s[i:j]))
		i = j
	}
	return b.String()
}

// ymlToken matches bare scalars worth colouring: booleans/null and numbers.
// The number side needs a leading \b so digits inside an identifier (the "3" in
// gemma3:1b) aren't coloured — only standalone numbers like ports or counts.
var ymlToken = regexp.MustCompile(`\b(?:true|false|null)\b|\b\d+(?:\.\d+)?\b`)

func highlightBareRun(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range ymlToken.FindAllStringIndex(s, -1) {
		b.WriteString(template.HTMLEscapeString(s[last:loc[0]]))
		tok := s[loc[0]:loc[1]]
		cls := "yml-num"
		if tok == "true" || tok == "false" || tok == "null" {
			cls = "yml-bool"
		}
		b.WriteString(`<span class="` + cls + `">`)
		b.WriteString(template.HTMLEscapeString(tok))
		b.WriteString(`</span>`)
		last = loc[1]
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return b.String()
}
