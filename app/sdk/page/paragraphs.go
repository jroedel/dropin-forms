package page

import (
	"strings"
)

// Paragraphs splits text somebody typed into a box into paragraphs of lines,
// for a template to draw as one <p> per paragraph with a <br> between lines:
// {{range paragraphs .}}<p>{{range $i, $l := .}}{{if $i}}<br>{{end}}{{$l}}{{end}}</p>{{end}}.
//
// Without it, text written in a textarea over two lines -- "Early
// registration: $35" above "Day of the retreat: $40" -- was drawn as one
// run-on sentence, because HTML folds a line break into a space. A blank line
// starts a new paragraph, which is what a blank line means to whoever typed
// it.
//
// It returns the pieces rather than HTML, so html/template still escapes every
// line: nothing typed into a form's builder can become markup on the page.
// The rejected alternative was white-space: pre-line on the help text, which
// would also have kept the line breaks the templates' own indentation puts
// inside every other help paragraph on the site.
func Paragraphs(text string) [][]string {
	var (
		paras [][]string
		lines []string
	)

	for line := range strings.Lines(strings.ReplaceAll(text, "\r\n", "\n")) {
		line = strings.TrimSpace(line)

		if line == "" {
			if len(lines) > 0 {
				paras = append(paras, lines)
				lines = nil
			}

			continue
		}

		lines = append(lines, line)
	}

	if len(lines) > 0 {
		paras = append(paras, lines)
	}

	return paras
}
