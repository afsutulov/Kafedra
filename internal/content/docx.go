package content

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Ограничения, защищающие от «архивных бомб» и чрезмерных документов.
const (
	maxDocxFiles = 5000
	maxDocxXML   = 64 << 20
	maxDocxImage = 20 << 20
	maxDocxImgs  = 300
)

type node struct {
	name  string
	attrs map[string]string
	kids  []*node
	text  string
}

func (n *node) child(name string) *node {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}
	return nil
}

func (n *node) find(name string) *node { // поиск в глубину
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
		if r := k.find(name); r != nil {
			return r
		}
	}
	return nil
}

func parseXML(r io.Reader) (*node, error) {
	d := xml.NewDecoder(r)
	d.Strict = false
	root := &node{name: "#root"}
	stack := []*node{root}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return root, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) > 256 {
				return nil, errors.New("слишком глубокая вложенность XML")
			}
			n := &node{name: t.Name.Local, attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.attrs[a.Name.Local] = a.Value
			}
			top := stack[len(stack)-1]
			top.kids = append(top.kids, n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			top := stack[len(stack)-1]
			if top.name == "t" || top.name == "instrText" || top.name == "v" {
				top.text += string(t)
			}
		}
	}
}

type docx struct {
	files     map[string]*zip.File
	rels      map[string]string
	headings  map[string]int  // styleId → уровень заголовка
	numFmt    map[string]bool // "numId/ilvl" → нумерованный список
	saveImage func(name string, data []byte) (string, error)
	images    int
	Notes     []string
}

func (d *docx) read(name string, limit int64) ([]byte, error) {
	f := d.files[name]
	if f == nil {
		return nil, fmt.Errorf("в документе отсутствует %s", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("часть документа %s превышает допустимый размер", name)
	}
	return b, nil
}

func (d *docx) xml(name string) *node {
	b, err := d.read(name, maxDocxXML)
	if err != nil {
		return nil
	}
	n, err := parseXML(strings.NewReader(string(b)))
	if err != nil {
		return nil
	}
	return n
}

var reHeading = regexp.MustCompile(`(?i)^(heading|заголовок)\s*([1-9])$`)

// ImportDOCX преобразует документ Word (.docx) в текст Markdown.
// Рисунки передаются в saveImage, которая возвращает адрес сохранённого файла.
// Вторым значением возвращаются замечания об элементах, которые не удалось перенести.
func ImportDOCX(r io.ReaderAt, size int64, saveImage func(name string, data []byte) (string, error)) (string, []string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", nil, errors.New("файл не является документом DOCX")
	}
	if len(zr.File) > maxDocxFiles {
		return "", nil, errors.New("слишком много частей в документе")
	}
	d := &docx{files: map[string]*zip.File{}, rels: map[string]string{}, headings: map[string]int{}, numFmt: map[string]bool{}, saveImage: saveImage}
	for _, f := range zr.File {
		d.files[f.Name] = f
	}
	doc := d.xml("word/document.xml")
	if doc == nil {
		return "", nil, errors.New("не удалось прочитать содержимое документа (word/document.xml)")
	}
	if rels := d.xml("word/_rels/document.xml.rels"); rels != nil {
		if root := rels.child("Relationships"); root != nil {
			for _, k := range root.kids {
				d.rels[k.attrs["Id"]] = k.attrs["Target"]
			}
		}
	}
	if st := d.xml("word/styles.xml").find("styles"); st != nil {
		for _, s := range st.kids {
			if s.name != "style" {
				continue
			}
			name := s.child("name")
			if name == nil {
				continue
			}
			if m := reHeading.FindStringSubmatch(strings.TrimSpace(name.attrs["val"])); m != nil {
				d.headings[s.attrs["styleId"]], _ = strconv.Atoi(m[2])
			} else if strings.EqualFold(name.attrs["val"], "Title") || strings.EqualFold(name.attrs["val"], "Название") {
				d.headings[s.attrs["styleId"]] = 1
			}
		}
	}
	if nm := d.xml("word/numbering.xml").find("numbering"); nm != nil {
		abs := map[string]map[string]bool{}
		for _, a := range nm.kids {
			if a.name == "abstractNum" {
				m := map[string]bool{}
				for _, l := range a.kids {
					if l.name == "lvl" {
						if f := l.child("numFmt"); f != nil {
							m[l.attrs["ilvl"]] = f.attrs["val"] != "bullet" && f.attrs["val"] != "none"
						}
					}
				}
				abs[a.attrs["abstractNumId"]] = m
			}
		}
		for _, n := range nm.kids {
			if n.name == "num" {
				if a := n.child("abstractNumId"); a != nil {
					for lvl, ord := range abs[a.attrs["val"]] {
						d.numFmt[n.attrs["numId"]+"/"+lvl] = ord
					}
				}
			}
		}
	}
	body := doc.find("body")
	if body == nil {
		return "", nil, errors.New("в документе нет содержимого")
	}
	var out strings.Builder
	d.blocks(&out, body)
	return strings.TrimSpace(out.String()) + "\n", d.Notes, nil
}

func (d *docx) blocks(out *strings.Builder, parent *node) {
	prevList := false
	for _, k := range parent.kids {
		switch k.name {
		case "p":
			line, isList := d.paragraph(k)
			if strings.TrimSpace(line) == "" {
				continue
			}
			if !(isList && prevList) {
				out.WriteString("\n")
			}
			out.WriteString(line + "\n")
			prevList = isList
		case "tbl":
			out.WriteString("\n" + d.table(k))
			prevList = false
		case "sdt":
			if c := k.child("sdtContent"); c != nil {
				d.blocks(out, c)
			}
		}
	}
}

type seg struct {
	text         string
	bold, italic bool
	raw          bool // готовая разметка (ссылка, рисунок)
}

func on(n *node) bool {
	if n == nil {
		return false
	}
	v := n.attrs["val"]
	return v != "0" && v != "false" && v != "none"
}

var mdEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "~", `\~`, "|", `\|`)

func (d *docx) runs(n *node, segs *[]seg, link string) {
	for _, k := range n.kids {
		switch k.name {
		case "r":
			pr := k.child("rPr")
			s := seg{bold: on(pr.child("b")), italic: on(pr.child("i"))}
			for _, c := range k.kids {
				switch c.name {
				case "t":
					s.text += c.text
				case "tab":
					s.text += " "
				case "br", "cr":
					if c.attrs["type"] != "page" {
						s.text += " "
					}
				case "noBreakHyphen":
					s.text += "-"
				case "drawing", "pict", "object":
					if md := d.image(c); md != "" {
						*segs = append(*segs, seg{text: md, raw: true})
					}
				}
			}
			if s.text != "" {
				if link != "" {
					*segs = append(*segs, seg{text: "[" + mdEscaper.Replace(s.text) + "](" + link + ")", raw: true})
				} else {
					*segs = append(*segs, s)
				}
			}
		case "hyperlink":
			target := d.rels[k.attrs["id"]]
			if !SafeURL(target) || strings.HasPrefix(target, "/") {
				target = ""
			}
			d.runs(k, segs, target)
		case "ins", "smartTag", "sdt", "sdtContent", "fldSimple", "oMath", "oMathPara", "bdo", "dir":
			d.runs(k, segs, link)
		}
	}
}

func (d *docx) image(n *node) string {
	blip := n.find("blip")
	id := ""
	if blip != nil {
		id = blip.attrs["embed"]
	} else if im := n.find("imagedata"); im != nil {
		id = im.attrs["id"]
	}
	target := d.rels[id]
	if target == "" || d.saveImage == nil {
		return ""
	}
	name := path.Clean("word/" + target)
	if strings.HasPrefix(target, "/") {
		name = strings.TrimPrefix(path.Clean(target), "/")
	}
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		d.Notes = append(d.Notes, "рисунок формата "+ext+" не перенесён (поддерживаются PNG, JPEG, GIF, WebP)")
		return ""
	}
	if d.images >= maxDocxImgs {
		return ""
	}
	data, err := d.read(name, maxDocxImage)
	if err != nil {
		d.Notes = append(d.Notes, "рисунок не перенесён: "+err.Error())
		return ""
	}
	url, err := d.saveImage(path.Base(name), data)
	if err != nil || !SafeURL(url) {
		d.Notes = append(d.Notes, "рисунок "+path.Base(name)+" не сохранён")
		return ""
	}
	d.images++
	return "![рисунок](" + url + ")"
}

func (d *docx) inlineText(p *node) string {
	var segs []seg
	d.runs(p, &segs, "")
	var merged []seg
	for _, s := range segs {
		if n := len(merged); n > 0 && !s.raw && !merged[n-1].raw && merged[n-1].bold == s.bold && merged[n-1].italic == s.italic {
			merged[n-1].text += s.text
		} else {
			merged = append(merged, s)
		}
	}
	var b strings.Builder
	for _, s := range merged {
		if s.raw {
			b.WriteString(s.text)
			continue
		}
		core := strings.TrimSpace(s.text)
		if core == "" {
			b.WriteString(s.text)
			continue
		}
		lead := s.text[:strings.Index(s.text, core)]
		trail := s.text[len(lead)+len(core):]
		mark := ""
		if s.bold {
			mark = "**"
		}
		if s.italic {
			mark += "*"
		}
		end := mark
		if s.bold && s.italic {
			end = "***"
		}
		b.WriteString(lead + mark + mdEscaper.Replace(core) + end + trail)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (d *docx) paragraph(p *node) (string, bool) {
	text := d.inlineText(p)
	if text == "" {
		return "", false
	}
	pr := p.child("pPr")
	style := ""
	if s := pr.child("pStyle"); s != nil {
		style = s.attrs["val"]
	}
	if lvl := d.headings[style]; lvl > 0 {
		if lvl > 5 {
			lvl = 5
		}
		// в заголовке собственное выделение полужирным избыточно
		return strings.Repeat("#", lvl) + " " + strings.Trim(text, "*"), false
	}
	if np := pr.child("numPr"); np != nil {
		lvl, id := "0", ""
		if l := np.child("ilvl"); l != nil {
			lvl = l.attrs["val"]
		}
		if n := np.child("numId"); n != nil {
			id = n.attrs["val"]
		}
		if id != "" && id != "0" {
			depth, _ := strconv.Atoi(lvl)
			if depth > 6 {
				depth = 6
			}
			marker := "- "
			if d.numFmt[id+"/"+lvl] {
				marker = "1. "
			}
			return strings.Repeat("    ", depth) + marker + text, true
		}
	}
	// защита от случайного превращения обычного абзаца в разметку
	if strings.HasPrefix(text, "#") || strings.HasPrefix(text, ">") {
		text = `\` + text
	}
	return text, false
}

func (d *docx) table(t *node) string {
	var rows [][]string
	width := 0
	for _, tr := range t.kids {
		if tr.name != "tr" {
			continue
		}
		var row []string
		for _, tc := range tr.kids {
			if tc.name != "tc" {
				continue
			}
			var parts []string
			for _, p := range tc.kids {
				if p.name == "p" {
					if s := d.inlineText(p); s != "" {
						parts = append(parts, s)
					}
				}
			}
			row = append(row, strings.Join(parts, " "))
		}
		if len(row) > width {
			width = len(row)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 || width == 0 {
		return ""
	}
	var b strings.Builder
	for i, row := range rows {
		for len(row) < width {
			row = append(row, "")
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
		if i == 0 {
			b.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
		}
	}
	return b.String()
}
