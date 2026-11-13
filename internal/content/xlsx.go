package content

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// Собственные чтение и запись книг Excel (XLSX — это ZIP-архив с XML-частями).
// Поддерживается то, что нужно шаблонам и выгрузкам: строки, числа, ширина столбцов,
// оформление заголовка. Формулы не создаются и не вычисляются.

// Sheet — лист книги. Значения ячеек: string, int, float64 либо nil (пустая ячейка).
type Sheet struct {
	Name       string
	Rows       [][]any
	Widths     []float64 // ширина столбцов в символах
	HeaderRows int       // сколько первых строк оформить как заголовок
}

var xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// xmlSafe экранирует текст и удаляет символы, недопустимые в XML 1.0.
func xmlSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r != 0xFFFE && r != 0xFFFF && r != 0xFFFD) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, ""))
	return xmlText.Replace(s)
}

func colName(i int) string {
	s := ""
	for i >= 0 {
		s = string(rune('A'+i%26)) + s
		i = i/26 - 1
	}
	return s
}

const xmlHead = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

// WriteXLSX записывает книгу Excel.
func WriteXLSX(w io.Writer, sheets ...Sheet) error {
	if len(sheets) == 0 {
		return errors.New("в книге нет листов")
	}
	zw := zip.NewWriter(w)
	put := func(name, body string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, body)
		return err
	}
	var types, wbSheets, wbRels strings.Builder
	for i, sh := range sheets {
		n := i + 1
		fmt.Fprintf(&types, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		name := []rune(strings.Map(func(r rune) rune {
			if strings.ContainsRune(`[]:*?/\`, r) {
				return ' '
			}
			return r
		}, sh.Name))
		if len(name) == 0 {
			name = []rune("Лист" + strconv.Itoa(n))
		}
		if len(name) > 31 {
			name = name[:31]
		}
		fmt.Fprintf(&wbSheets, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlSafe(string(name)), n, n)
		fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
		var b strings.Builder
		b.WriteString(xmlHead + `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
		if sh.HeaderRows > 0 {
			fmt.Fprintf(&b, `<sheetViews><sheetView workbookViewId="0"><pane ySplit="%d" topLeftCell="A%d" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`, sh.HeaderRows, sh.HeaderRows+1)
		}
		if len(sh.Widths) > 0 {
			b.WriteString("<cols>")
			for c, wd := range sh.Widths {
				fmt.Fprintf(&b, `<col min="%d" max="%d" width="%g" customWidth="1"/>`, c+1, c+1, wd)
			}
			b.WriteString("</cols>")
		}
		b.WriteString("<sheetData>")
		for r, row := range sh.Rows {
			style := 2 // перенос текста
			if r < sh.HeaderRows {
				style = 1
			}
			fmt.Fprintf(&b, `<row r="%d">`, r+1)
			for c, v := range row {
				ref := colName(c) + strconv.Itoa(r+1)
				switch x := v.(type) {
				case nil:
				case int:
					fmt.Fprintf(&b, `<c r="%s" s="%d"><v>%d</v></c>`, ref, style, x)
				case float64:
					fmt.Fprintf(&b, `<c r="%s" s="%d"><v>%s</v></c>`, ref, style, strconv.FormatFloat(x, 'f', -1, 64))
				default:
					s := fmt.Sprint(x)
					if s != "" {
						fmt.Fprintf(&b, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, xmlSafe(s))
					}
				}
			}
			b.WriteString("</row>")
		}
		b.WriteString("</sheetData></worksheet>")
		if err := put(fmt.Sprintf("xl/worksheets/sheet%d.xml", n), b.String()); err != nil {
			return err
		}
	}
	parts := []struct{ name, body string }{
		{"[Content_Types].xml", xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` + types.String() + `</Types>`},
		{"_rels/.rels", xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", xmlHead + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><bookViews><workbookView/></bookViews><sheets>` + wbSheets.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + wbRels.String() +
			fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`, len(sheets)+1)},
		{"xl/styles.xml", xmlHead + `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>` +
			`<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FFD4E3F8"/><bgColor indexed="64"/></patternFill></fill></fills>` +
			`<borders count="2"><border><left/><right/><top/><bottom/><diagonal/></border><border><left style="thin"><color rgb="FF93B8ED"/></left><right style="thin"><color rgb="FF93B8ED"/></right><top style="thin"><color rgb="FF93B8ED"/></top><bottom style="thin"><color rgb="FF93B8ED"/></bottom><diagonal/></border></borders>` +
			`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
			`<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
			`<xf numFmtId="0" fontId="1" fillId="2" borderId="1" xfId="0" applyFont="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>` +
			`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="top" wrapText="1"/></xf></cellXfs>` +
			`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`},
	}
	for _, p := range parts {
		if err := put(p.name, p.body); err != nil {
			return err
		}
	}
	return zw.Close()
}

// Ограничения при чтении книг.
const (
	maxXLSXRows = 5000
	maxXLSXCols = 60
	maxXLSXPart = 32 << 20
)

// ReadXLSX читает первый лист книги Excel и возвращает строки с текстом ячеек.
// Пустые строки в конце отбрасываются, внутри таблицы — сохраняются.
func ReadXLSX(r io.ReaderAt, size int64) ([][]string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, errors.New("файл не является книгой Excel (.xlsx)")
	}
	if len(zr.File) > maxDocxFiles {
		return nil, errors.New("слишком много частей в книге")
	}
	d := &docx{files: map[string]*zip.File{}}
	for _, f := range zr.File {
		if _, exists := d.files[f.Name]; exists {
			return nil, errors.New("повторяющаяся часть книги Excel")
		}
		d.files[f.Name] = f
	}
	read := func(name string) *node {
		b, err := d.read(name, maxXLSXPart)
		if err != nil {
			return nil
		}
		n, err := parseXML(strings.NewReader(string(b)))
		if err != nil {
			return nil
		}
		return n
	}
	// первый лист — по порядку в книге, а не по имени файла
	sheetPath := "xl/worksheets/sheet1.xml"
	if wb := read("xl/workbook.xml").find("sheets"); wb != nil && len(wb.kids) > 0 {
		if rels := read("xl/_rels/workbook.xml.rels").child("Relationships"); rels != nil {
			for _, rel := range rels.kids {
				if rel.attrs["Id"] == wb.kids[0].attrs["id"] {
					t := rel.attrs["Target"]
					if strings.HasPrefix(t, "/") {
						sheetPath = strings.TrimPrefix(path.Clean(t), "/")
					} else {
						sheetPath = path.Clean("xl/" + t)
					}
				}
			}
		}
	}
	sheet := read(sheetPath).find("sheetData")
	if sheet == nil {
		return nil, errors.New("в книге не найден лист с данными")
	}
	var shared []string
	if sst := read("xl/sharedStrings.xml").child("sst"); sst != nil {
		for _, si := range sst.kids {
			if si.name == "si" {
				shared = append(shared, siText(si))
			}
		}
	}
	var rows [][]string
	for _, row := range sheet.kids {
		if row.name != "row" {
			continue
		}
		rn := len(rows) + 1
		if raw := row.attrs["r"]; raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < rn {
				return nil, fmt.Errorf("некорректный номер строки %q", raw)
			}
			rn = v
		}
		if rn > maxXLSXRows {
			return nil, fmt.Errorf("в листе больше %d строк", maxXLSXRows)
		}
		for len(rows) < rn {
			rows = append(rows, nil)
		}
		var cells []string
		next := 0
		for _, c := range row.kids {
			if c.name != "c" {
				continue
			}
			col := next
			if ref := c.attrs["r"]; ref != "" {
				col = 0
				n := 0
				for n < len(ref) && ref[n] >= 'A' && ref[n] <= 'Z' {
					col = col*26 + int(ref[n]-'A') + 1
					n++
					if col > 16384 {
						return nil, fmt.Errorf("строка %d: неверный адрес ячейки", rn)
					}
				}
				rowNumber, err := strconv.Atoi(ref[n:])
				if col == 0 || err != nil || rowNumber != rn {
					return nil, fmt.Errorf("строка %d: неверный адрес ячейки", rn)
				}
				col--
			}
			if col < next {
				return nil, fmt.Errorf("строка %d: повторяющаяся ячейка", rn)
			}
			if col >= maxXLSXCols {
				if c.child("v") != nil || c.child("is") != nil || c.child("f") != nil {
					return nil, fmt.Errorf("строка %d: данные за пределами %d столбцов", rn, maxXLSXCols)
				}
				continue
			}
			if c.child("f") != nil || c.attrs["t"] == "e" {
				return nil, fmt.Errorf("строка %d: замените формулу или ошибку Excel обычным значением", rn)
			}
			next = col + 1
			val := ""
			switch c.attrs["t"] {
			case "s":
				if v := c.child("v"); v != nil {
					if i, err := strconv.Atoi(strings.TrimSpace(v.text)); err == nil && i >= 0 && i < len(shared) {
						val = shared[i]
					} else {
						return nil, fmt.Errorf("строка %d: повреждена строковая ячейка", rn)
					}
				}
			case "inlineStr":
				if is := c.child("is"); is != nil {
					val = siText(is)
				}
			case "", "n":
				if v := c.child("v"); v != nil {
					val = cleanNumber(v.text)
				}
			default:
				if v := c.child("v"); v != nil {
					val = v.text
				}
			}
			for len(cells) <= col {
				cells = append(cells, "")
			}
			cells[col] = strings.TrimSpace(val)
		}
		rows[rn-1] = cells
	}
	for len(rows) > 0 && strings.Join(rows[len(rows)-1], "") == "" {
		rows = rows[:len(rows)-1]
	}
	return rows, nil
}

// siText собирает текст строки (простой или с форматированными фрагментами), без фонетических подсказок.
func siText(si *node) string {
	var b strings.Builder
	var walk func(n *node)
	walk = func(n *node) {
		for _, k := range n.kids {
			switch k.name {
			case "t":
				b.WriteString(k.text)
			case "rPh", "phoneticPr":
			default:
				walk(k)
			}
		}
	}
	walk(si)
	return b.String()
}

// cleanNumber убирает артефакты двоичного представления (0.30000000000000004 → 0.3).
func cleanNumber(s string) string {
	s = strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(s, 64); err == nil && strings.ContainsAny(s, ".eE") {
		return strconv.FormatFloat(f, 'g', 12, 64)
	}
	return s
}
