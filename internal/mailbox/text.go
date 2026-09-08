package mailbox

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// blockElements force a line break when flattening HTML, so that a table of
// order lines does not collapse into one unreadable run of words.
var blockElements = map[string]bool{
	"br": true, "p": true, "div": true, "tr": true, "table": true,
	"li": true, "ul": true, "ol": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "section": true, "header": true,
	"footer": true, "article": true,
}

// cellElements are separated by a tab, which keeps "name  price" pairs apart
// on one line where the parsers expect them.
var cellElements = map[string]bool{"td": true, "th": true}

// htmlToText flattens an HTML mail body into plain text.
func htmlToText(source string) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return stripTags(source)
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		switch node.Type {
		case html.TextNode:
			b.WriteString(node.Data)
		case html.ElementNode:
			switch {
			case node.Data == "style" || node.Data == "script" || node.Data == "head":
				return
			case blockElements[node.Data]:
				b.WriteString("\n")
			case cellElements[node.Data]:
				b.WriteString("\t")
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == html.ElementNode && blockElements[node.Data] {
			b.WriteString("\n")
		}
	}
	walk(doc)
	return b.String()
}

var tagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

// stripTags is the crude fallback for HTML the parser refuses.
func stripTags(source string) string {
	return html.UnescapeString(tagPattern.ReplaceAllString(source, " "))
}

var (
	spacePattern      = regexp.MustCompile(`[ \x{00a0}\t]+`)
	blankLinesPattern = regexp.MustCompile(`\n{3,}`)
)

// normalizeWhitespace collapses runs of spaces and blank lines while keeping
// the line structure the parsers anchor on. Non-breaking spaces, which are
// everywhere in HTML mail, become ordinary spaces.
func normalizeWhitespace(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(spacePattern.ReplaceAllString(line, " "))
	}
	return strings.TrimSpace(blankLinesPattern.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
