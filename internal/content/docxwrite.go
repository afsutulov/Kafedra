package content

import (
	"archive/zip"
	"fmt"
	"io"
	"strings"
)

// Para — абзац создаваемого документа Word.
type Para struct {
	Text   string
	Bold   bool
	Align  string // "", center, right, both
	Size   int    // кегль в пунктах (0 — 12)
	Before int    // отступ перед абзацем, пункты
	Indent bool   // красная строка
}

// WriteDOCX создаёт документ Word (.docx) из абзацев: бланки анкет и заявлений.
func WriteDOCX(w io.Writer, paras []Para) error {
	var b strings.Builder
	b.WriteString(xmlHead + `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paras {
		size := p.Size
		if size == 0 {
			size = 12
		}
		b.WriteString("<w:p><w:pPr>")
		fmt.Fprintf(&b, `<w:spacing w:before="%d" w:after="80" w:line="300" w:lineRule="auto"/>`, p.Before*20)
		if p.Indent {
			b.WriteString(`<w:ind w:firstLine="567"/>`)
		}
		if p.Align != "" {
			fmt.Fprintf(&b, `<w:jc w:val="%s"/>`, p.Align)
		}
		b.WriteString("</w:pPr>")
		for i, line := range strings.Split(p.Text, "\n") {
			b.WriteString(`<w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:cs="Times New Roman"/>`)
			if p.Bold {
				b.WriteString("<w:b/>")
			}
			fmt.Fprintf(&b, `<w:sz w:val="%d"/><w:szCs w:val="%d"/></w:rPr>`, size*2, size*2)
			if i > 0 {
				b.WriteString("<w:br/>")
			}
			b.WriteString(`<w:t xml:space="preserve">` + xmlSafe(line) + `</w:t></w:r>`)
		}
		b.WriteString("</w:p>")
	}
	b.WriteString(`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1134" w:right="850" w:bottom="1134" w:left="1701" w:header="708" w:footer="708" w:gutter="0"/></w:sectPr></w:body></w:document>`)
	zw := zip.NewWriter(w)
	for _, part := range []struct{ name, body string }{
		{"[Content_Types].xml", xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
		{"_rels/.rels", xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
		{"word/document.xml", b.String()},
	} {
		f, err := zw.Create(part.name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(f, part.body); err != nil {
			return err
		}
	}
	return zw.Close()
}
