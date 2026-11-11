package content

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestRenderBasics(t *testing.T) {
	cases := []struct{ in, want string }{
		{"# Тема", "<h2>Тема</h2>"},
		{"**жирный** и *курсив*", "<strong>жирный</strong> и <em>курсив</em>"},
		{"- раз\n- два\n    1. вложенный", "<ol>\n<li>вложенный</li>"},
		{"```go\nif a < b {}\n```", `<pre><code class="lang-go">if a &lt; b {}</code></pre>`},
		{"| A | B |\n|---|---|\n| 1 | 2 |", "<td>2</td>"},
		{"[сайт](https://example.ru)", `<a href="https://example.ru" rel="noopener noreferrer">сайт</a>`},
		{"![схема](/files/abc)", `<img src="/files/abc" alt="схема" loading="lazy">`},
		{"> цитата", "<blockquote>"},
	}
	for _, c := range cases {
		if got := Render(c.in); !strings.Contains(got, c.want) {
			t.Errorf("Render(%q) = %q, нет %q", c.in, got, c.want)
		}
	}
}

func TestRenderIsSafe(t *testing.T) {
	evil := []string{
		`<script>alert(1)</script>`,
		`[x](javascript:alert(1))`,
		`![x](data:text/html,<script>)`,
		`[x](https://a.ru" onclick="alert(1))`,
		"```\"><img src=x onerror=alert(1)>\n</code><script>\n```",
		`<img src=x onerror=alert(1)>`,
		`[a](//evil.example)`,
	}
	for _, e := range evil {
		got := Render(e)
		for _, bad := range []string{"<script", "javascript:", "onerror=alert(1)>", `onclick="`, "data:text", `href="//`} {
			if strings.Contains(got, bad) && !strings.Contains(got, "&lt;") && !strings.Contains(got, "&#34;") {
				t.Errorf("небезопасный вывод для %q: %q", e, got)
			}
		}
		if strings.Contains(got, "<script") || strings.Contains(got, `href="javascript`) || strings.Contains(got, `src="data:`) {
			t.Errorf("небезопасный вывод для %q: %q", e, got)
		}
	}
}

func FuzzRender(f *testing.F) {
	f.Add("# a\n- b\n```\nc\n```\n[x](y) **z** | a |\n|---|")
	f.Fuzz(func(t *testing.T, s string) {
		out := Render(s)
		if strings.Contains(out, "<script") {
			t.Fatalf("тег script в выводе: %q", out)
		}
	})
}

func buildDocx(t testing.TB, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

const wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`

func TestImportDOCX(t *testing.T) {
	doc := `<?xml version="1.0"?><w:document ` + wNS + `><w:body>
<w:p><w:pPr><w:pStyle w:val="1"/></w:pPr><w:r><w:t>Лекция 1</w:t></w:r></w:p>
<w:p><w:r><w:t xml:space="preserve">Обычный </w:t></w:r><w:r><w:rPr><w:b/></w:rPr><w:t>жирный</w:t></w:r><w:r><w:t xml:space="preserve"> текст со *звёздочкой*</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>первый</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>второй</w:t></w:r></w:p>
<w:p><w:hyperlink r:id="rId2"><w:r><w:t>ссылка</w:t></w:r></w:hyperlink></w:p>
<w:p><w:r><w:drawing><a:blip r:embed="rId1"/></w:drawing></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>К1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>К2</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>a</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>b</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body></w:document>`
	data := buildDocx(t, map[string]string{
		"word/document.xml":            doc,
		"word/styles.xml":              `<w:styles ` + wNS + `><w:style w:styleId="1"><w:name w:val="heading 1"/></w:style></w:styles>`,
		"word/numbering.xml":           `<w:numbering ` + wNS + `><w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum><w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num></w:numbering>`,
		"word/_rels/document.xml.rels": `<Relationships><Relationship Id="rId1" Target="media/image1.png"/><Relationship Id="rId2" Target="https://example.ru/doc" TargetMode="External"/></Relationships>`,
		"word/media/image1.png":        "\x89PNG....",
	})
	saved := 0
	md, _, err := ImportDOCX(bytes.NewReader(data), int64(len(data)), func(name string, b []byte) (string, error) {
		saved++
		return "/files/img1", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Лекция 1", "Обычный **жирный** текст со \\*звёздочкой\\*", "1. первый\n1. второй", "[ссылка](https://example.ru/doc)", "![рисунок](/files/img1)", "| К1 | К2 |\n| --- | --- |\n| a | b |"} {
		if !strings.Contains(md, want) {
			t.Errorf("в результате нет %q:\n%s", want, md)
		}
	}
	if saved != 1 {
		t.Errorf("сохранено рисунков: %d", saved)
	}
	if html := Render(md); !strings.Contains(html, "<h2>Лекция 1</h2>") || !strings.Contains(html, "<ol>") {
		t.Errorf("результат импорта не отображается: %s", html)
	}
}

func FuzzImportDOCX(f *testing.F) {
	f.Add(`<w:document><w:body><w:p><w:r><w:t>x</w:t></w:r></w:p></w:body></w:document>`)
	f.Fuzz(func(t *testing.T, doc string) {
		data := buildDocx(t, map[string]string{"word/document.xml": doc})
		ImportDOCX(bytes.NewReader(data), int64(len(data)), nil)
	})
}
