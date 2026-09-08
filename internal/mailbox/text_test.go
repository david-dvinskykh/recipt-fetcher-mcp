package mailbox

import (
	"os"
	"strings"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestHTMLToTextKeepsLineStructure(t *testing.T) {
	const body = `<html><head><style>.x{color:red}</style></head><body>
<p>Dziękujemy za zakup!</p>
<table><tr><td>Klocki</td><td>149,99 z&#322;</td></tr></table>
<div>Do zap&#322;aty: 149,99 z&#322;</div>
</body></html>`

	text := normalizeWhitespace(htmlToText(body))
	if strings.Contains(text, "color:red") {
		t.Error("style content leaked into the text")
	}
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Dziękujemy") && strings.Contains(line, "Do zapłaty") {
			t.Fatalf("block elements were not turned into line breaks: %q", line)
		}
	}
	if !strings.Contains(text, "Do zapłaty: 149,99 zł") {
		t.Fatalf("entities were not decoded:\n%s", text)
	}
}

func TestNormalizeWhitespace(t *testing.T) {
	got := normalizeWhitespace("  a  b \r\n\r\n\r\n c  \n")
	if got != "a b\n\nc" {
		t.Fatalf("normalizeWhitespace = %q", got)
	}
}

func TestStripTagsFallback(t *testing.T) {
	if got := stripTags("<b>a</b>&amp;<i>b</i>"); !strings.Contains(got, "a") || !strings.Contains(got, "&") {
		t.Fatalf("stripTags = %q", got)
	}
}
