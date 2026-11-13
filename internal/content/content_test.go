package content

import (
	"archive/zip"
	"bytes"
	"os"
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

func TestOversizedTableFallsBackToEscapedText(t *testing.T) {
	src := strings.Repeat("|", 2400) + "\n|---|\n" + strings.Repeat("|\n", 1200) + "|<script>|\n\nСледующий абзац"
	got := Render(src)
	if len(got) > 2*len(src) || strings.Contains(got, "<script>") || !strings.Contains(got, "<p>Следующий абзац</p>") {
		t.Fatalf("unexpected rendering (%d bytes)", len(got))
	}
}

func TestXLSXRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	err := WriteXLSX(&buf, Sheet{Name: "Группа", HeaderRows: 1, Widths: []float64{30, 12},
		Rows: [][]any{{"ФИО", "Баллы"}, {"Иванов <Пётр> & Ко", 4.5}, {nil, 7}, {"=1+1", nil}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ReadXLSX(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[1][0] != "Иванов <Пётр> & Ко" || rows[1][1] != "4.5" || rows[2][0] != "" || rows[2][1] != "7" || rows[3][0] != "=1+1" {
		t.Fatalf("прочитано: %q", rows)
	}
	// книга, сохранённая табличным редактором: общие строки, произвольное имя файла листа
	data := buildDocx(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="r"><sheets><sheet name="Данные" sheetId="5" r:id="rId7"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId7" Target="worksheets/sheet5.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><t>Вопрос</t></si><si><r><t>Сос</t></r><r><t>тавной</t></r></si></sst>`,
		"xl/worksheets/sheet5.xml":   `<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row><row r="3"><c r="B3"><v>0.30000000000000004</v></c></row></sheetData></worksheet>`,
	})
	rows, err = ReadXLSX(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(rows) != 3 || rows[0][0] != "Вопрос" || rows[0][2] != "Составной" || rows[2][1] != "0.3" {
		t.Fatalf("err=%v rows=%q", err, rows)
	}
}

func FuzzReadXLSX(f *testing.F) {
	f.Add(`<worksheet><sheetData><row r="1"><c r="A1"><v>1</v></c></row></sheetData></worksheet>`)
	f.Fuzz(func(t *testing.T, sheet string) {
		data := buildDocx(t, map[string]string{"xl/worksheets/sheet1.xml": sheet})
		ReadXLSX(bytes.NewReader(data), int64(len(data)))
	})
}

func TestWriteDOCXIsReadable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteDOCX(&buf, []Para{{Text: "АНКЕТА", Bold: true, Align: "center", Size: 14}, {Text: "ФИО: <Иванов> & сын"}}); err != nil {
		t.Fatal(err)
	}
	md, _, err := ImportDOCX(bytes.NewReader(buf.Bytes()), int64(buf.Len()), nil)
	if err != nil || !strings.Contains(md, "АНКЕТА") || !strings.Contains(md, "<Иванов> & сын") {
		t.Fatalf("err=%v md=%q", err, md)
	}
}

func TestPDFWithSystemFont(t *testing.T) {
	var font []byte
	for _, p := range []string{"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf"} {
		if b, err := os.ReadFile(p); err == nil {
			font = b
			break
		}
	}
	if font == nil {
		t.Skip("в системе нет шрифта TrueType для проверки")
	}
	p, err := NewPDF(842, 595, font)
	if err != nil {
		t.Fatal(err)
	}
	p.Text(421, 300, 24, "Сертификат № 1 — «Курс»", 0, Hex(0x202834), true)
	if w := p.TextWidth("Привет", 12); w < 20 || w > 60 {
		t.Fatalf("ширина строки %v", w)
	}
	out := p.Bytes("Сертификат")
	if !bytes.HasPrefix(out, []byte("%PDF-1.5")) || !bytes.Contains(out, []byte("/FontFile2")) || !bytes.HasSuffix(out, []byte("%%EOF\n")) {
		t.Fatal("некорректная структура PDF")
	}
	if _, err := NewPDF(1, 1, []byte("не шрифт")); err == nil {
		t.Fatal("мусор принят за шрифт")
	}
}

func FuzzParseTTF(f *testing.F) {
	f.Add([]byte("\x00\x01\x00\x00\x00\x01"))
	f.Fuzz(func(t *testing.T, data []byte) { parseTTF(data) })
}
