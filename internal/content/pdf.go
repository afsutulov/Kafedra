package content

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"sort"
	"strings"
	"unicode/utf16"
)

// Собственное формирование одностраничных PDF-документов (сертификаты).
// Кириллица в PDF требует внедрённого шрифта: используется шрифт TrueType,
// установленный в операционной системе; он разбирается здесь же, без сторонних библиотек.

// ttf — разобранный шрифт TrueType.
type ttf struct {
	data            []byte
	unitsPerEm      int
	ascent, descent int
	capHeight       int
	bbox            [4]int
	advance         []uint16
	cmap            map[rune]uint16
}

func (f *ttf) u16(off int) int {
	if off < 0 || off+2 > len(f.data) {
		return 0
	}
	return int(binary.BigEndian.Uint16(f.data[off:]))
}

func (f *ttf) s16(off int) int { return int(int16(f.u16(off))) }

func (f *ttf) u32(off int) int {
	if off < 0 || off+4 > len(f.data) {
		return 0
	}
	return int(binary.BigEndian.Uint32(f.data[off:]))
}

func parseTTF(data []byte) (*ttf, error) {
	f := &ttf{data: data, cmap: map[rune]uint16{}}
	if len(data) < 12 || (f.u32(0) != 0x00010000 && string(data[:4]) != "true") {
		return nil, errors.New("поддерживаются только шрифты TrueType (.ttf)")
	}
	tables := map[string][2]int{}
	for i, n := 0, f.u16(4); i < n && i < 200; i++ {
		rec := 12 + 16*i
		if rec+16 > len(data) {
			break
		}
		off, length := f.u32(rec+8), f.u32(rec+12)
		if off < 0 || length < 0 || off+length > len(data) {
			return nil, errors.New("файл шрифта повреждён")
		}
		tables[string(data[rec:rec+4])] = [2]int{off, length}
	}
	for _, need := range []string{"head", "hhea", "hmtx", "maxp", "cmap", "glyf"} {
		if _, ok := tables[need]; !ok {
			return nil, fmt.Errorf("в шрифте нет таблицы %s", need)
		}
	}
	head, hhea := tables["head"][0], tables["hhea"][0]
	f.unitsPerEm = f.u16(head + 18)
	if f.unitsPerEm < 16 || f.unitsPerEm > 16384 {
		return nil, errors.New("файл шрифта повреждён")
	}
	for i := range f.bbox {
		f.bbox[i] = f.s16(head + 36 + 2*i)
	}
	f.ascent, f.descent = f.s16(hhea+4), f.s16(hhea+6)
	f.capHeight = f.ascent * 7 / 10
	if os2, ok := tables["OS/2"]; ok {
		if f.u16(os2[0]+8)&0x0002 != 0 {
			return nil, errors.New("лицензия шрифта запрещает его внедрение в документы")
		}
		if f.u16(os2[0]) >= 2 && os2[1] >= 90 {
			f.capHeight = f.s16(os2[0] + 88)
		}
	}
	numGlyphs, numHM := f.u16(tables["maxp"][0]+4), f.u16(hhea+34)
	if numGlyphs < 1 || numHM < 1 || numHM > numGlyphs || numHM*4 > tables["hmtx"][1] {
		return nil, errors.New("некорректные метрики шрифта")
	}
	hmtx := tables["hmtx"][0]
	f.advance = make([]uint16, numGlyphs)
	last := 0
	for g := 0; g < numGlyphs; g++ {
		if g < numHM {
			last = f.u16(hmtx + 4*g)
		}
		f.advance[g] = uint16(last)
	}
	// таблица соответствия символов: подтаблица формата 4 (Юникод, основная плоскость)
	cm := tables["cmap"][0]
	sub := 0
	for i, n := 0, f.u16(cm+2); i < n && i < 64; i++ {
		rec := cm + 4 + 8*i
		pid, eid, off := f.u16(rec), f.u16(rec+2), cm+f.u32(rec+4)
		if f.u16(off) == 4 && ((pid == 3 && eid == 1) || (pid == 0 && sub == 0)) {
			sub = off
		}
	}
	if sub == 0 {
		return nil, errors.New("в шрифте нет таблицы символов Юникода")
	}
	seg := f.u16(sub+6) / 2
	endP := sub + 14
	startP, deltaP, rangeP := endP+2*seg+2, endP+4*seg+2, endP+6*seg+2
	if seg == 0 || rangeP+2*seg > sub+f.u16(sub+2) || sub+f.u16(sub+2) > cm+tables["cmap"][1] {
		return nil, errors.New("повреждена таблица символов шрифта")
	}
	previous := -1
	for i := 0; i < seg; i++ {
		end, start, delta, ro := f.u16(endP+2*i), f.u16(startP+2*i), f.u16(deltaP+2*i), f.u16(rangeP+2*i)
		if start > end || start <= previous {
			return nil, errors.New("пересекающиеся диапазоны символов шрифта")
		}
		previous = end
		if start == 0xFFFF {
			continue
		}
		for c := start; c <= end && c < 0xFFFF; c++ {
			g := 0
			if ro == 0 {
				g = (c + delta) & 0xFFFF
			} else if g = f.u16(rangeP + 2*i + ro + 2*(c-start)); g != 0 {
				g = (g + delta) & 0xFFFF
			}
			if g > 0 && g < numGlyphs {
				f.cmap[rune(c)] = uint16(g)
			}
		}
	}
	if f.cmap['А'] == 0 || f.cmap['я'] == 0 {
		return nil, errors.New("в шрифте нет кириллицы")
	}
	return f, nil
}

// PDF — холст одностраничного документа. Координаты в пунктах (1/72 дюйма), начало — левый нижний угол.
type PDF struct {
	W, H   float64
	font   *ttf
	used   map[uint16]rune
	ops    bytes.Buffer
	images []pdfImage
}

type pdfImage struct {
	w, h       int
	rgb, alpha []byte
}

// NewPDF создаёт холст заданного размера с внедряемым шрифтом TrueType.
func NewPDF(w, h float64, fontData []byte) (*PDF, error) {
	f, err := parseTTF(fontData)
	if err != nil {
		return nil, err
	}
	return &PDF{W: w, H: h, font: f, used: map[uint16]rune{}}, nil
}

func (p *PDF) glyphs(s string, register bool) []uint16 {
	var out []uint16
	for _, r := range s {
		g := p.font.cmap[r]
		if g == 0 {
			switch r { // типографские знаки, которых может не быть в шрифте
			case '«', '»', '“', '”', '„':
				g = p.font.cmap['"']
			case '—', '–':
				g = p.font.cmap['-']
			case '№':
				g = p.font.cmap['N']
			case ' ':
				g = p.font.cmap[' ']
			}
		}
		if g == 0 {
			g = p.font.cmap['?']
		}
		if register && g != 0 {
			if _, exists := p.used[g]; !exists {
				p.used[g] = r
			}
		}
		out = append(out, g)
	}
	return out
}

// TextWidth возвращает ширину строки при заданном кегле.
func (p *PDF) TextWidth(s string, size float64) float64 {
	w := 0
	for _, g := range p.glyphs(s, false) {
		if int(g) < len(p.font.advance) {
			w += int(p.font.advance[g])
		}
	}
	return float64(w) * size / float64(p.font.unitsPerEm)
}

// Color — цвет RGB, компоненты 0…1.
type Color [3]float64

// Hex преобразует цвет вида 0x566a88.
func Hex(v int) Color {
	return Color{float64(v>>16&255) / 255, float64(v>>8&255) / 255, float64(v&255) / 255}
}

// Text выводит строку. align: -1 — от точки x вправо, 0 — по центру относительно x, 1 — влево от x.
func (p *PDF) Text(x, y, size float64, s string, align int, c Color, bold bool) {
	gl := p.glyphs(s, true)
	if len(gl) == 0 {
		return
	}
	w := p.TextWidth(s, size)
	switch align {
	case 0:
		x -= w / 2
	case 1:
		x -= w
	}
	var hex strings.Builder
	for _, g := range gl {
		fmt.Fprintf(&hex, "%04X", g)
	}
	mode := "0 Tr"
	if bold { // полужирное начертание: заливка с обводкой
		mode = fmt.Sprintf("2 Tr %.3f w %.3f %.3f %.3f RG", size*0.03, c[0], c[1], c[2])
	}
	fmt.Fprintf(&p.ops, "BT /F1 %.2f Tf %s %.3f %.3f %.3f rg %.2f %.2f Td <%s> Tj ET\n", size, mode, c[0], c[1], c[2], x, y, hex.String())
}

// Wrap разбивает текст на строки не шире width.
func (p *PDF) Wrap(s string, size, width float64) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(s) {
		if cur != "" && p.TextWidth(cur+" "+word, size) <= width {
			cur += " " + word
			continue
		}
		if cur != "" {
			lines = append(lines, cur)
			cur = ""
		}
		if p.TextWidth(word, size) <= width {
			cur = word
			continue
		}
		var part strings.Builder
		lineWidth := 0.0
		for _, r := range word {
			glyphWidth := p.TextWidth(string(r), size)
			if part.Len() != 0 && lineWidth+glyphWidth > width {
				lines = append(lines, part.String())
				part.Reset()
				lineWidth = 0
			}
			part.WriteRune(r)
			lineWidth += glyphWidth
		}
		cur = part.String()
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// Line проводит отрезок.
func (p *PDF) Line(x1, y1, x2, y2, width float64, c Color) {
	fmt.Fprintf(&p.ops, "%.3f %.3f %.3f RG %.2f w %.2f %.2f m %.2f %.2f l S\n", c[0], c[1], c[2], width, x1, y1, x2, y2)
}

// Rect рисует прямоугольник: контур (fill=false) либо заливку.
func (p *PDF) Rect(x, y, w, h, width float64, c Color, fill bool) {
	if fill {
		fmt.Fprintf(&p.ops, "%.3f %.3f %.3f rg %.2f %.2f %.2f %.2f re f\n", c[0], c[1], c[2], x, y, w, h)
		return
	}
	fmt.Fprintf(&p.ops, "%.3f %.3f %.3f RG %.2f w %.2f %.2f %.2f %.2f re S\n", c[0], c[1], c[2], width, x, y, w, h)
}

// Raw добавляет операторы содержимого страницы как есть (векторная графика).
func (p *PDF) Raw(ops string) { p.ops.WriteString(ops + "\n") }

// Image размещает растровое изображение, вписывая его в прямоугольник с сохранением пропорций.
func (p *PDF) Image(img image.Image, x, y, w, h float64) {
	b := img.Bounds()
	iw, ih := b.Dx(), b.Dy()
	if iw == 0 || ih == 0 {
		return
	}
	step := 1 // крупные изображения прореживаются: для печати значка 600 точек достаточно
	for iw/step > 600 || ih/step > 600 {
		step++
	}
	ow, oh := iw/step, ih/step
	im := pdfImage{w: ow, h: oh, rgb: make([]byte, 0, ow*oh*3), alpha: make([]byte, 0, ow*oh)}
	opaque := true
	for py := 0; py < oh; py++ {
		for px := 0; px < ow; px++ {
			r, g, bl, a := img.At(b.Min.X+px*step, b.Min.Y+py*step).RGBA()
			if a == 0 {
				im.rgb = append(im.rgb, 255, 255, 255)
			} else { // цвета хранятся с предумножением на прозрачность
				im.rgb = append(im.rgb, byte(r*255/a), byte(g*255/a), byte(bl*255/a))
			}
			im.alpha = append(im.alpha, byte(a>>8))
			opaque = opaque && a == 0xffff
		}
	}
	if opaque {
		im.alpha = nil
	}
	p.images = append(p.images, im)
	scale := w / float64(ow)
	if s := h / float64(oh); s < scale {
		scale = s
	}
	dw, dh := float64(ow)*scale, float64(oh)*scale
	fmt.Fprintf(&p.ops, "q %.2f 0 0 %.2f %.2f %.2f cm /Im%d Do Q\n", dw, dh, x+(w-dw)/2, y+(h-dh)/2, len(p.images))
}

func deflate(b []byte) []byte {
	var out bytes.Buffer
	zw := zlib.NewWriter(&out)
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

// Bytes формирует файл PDF.
func (p *PDF) Bytes(title string) []byte {
	var out bytes.Buffer
	var offs []int
	obj := func(body string, stream []byte) {
		offs = append(offs, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s", len(offs), body)
		if stream != nil {
			out.WriteString("\nstream\n")
			out.Write(stream)
			out.WriteString("\nendstream")
		}
		out.WriteString("\nendobj\n")
	}
	stream := func(dict string, data []byte) {
		z := deflate(data)
		obj(fmt.Sprintf("<< %s /Filter /FlateDecode /Length %d >>", dict, len(z)), z)
	}
	f := p.font
	scale := func(v int) int { return v * 1000 / f.unitsPerEm }
	gids := make([]int, 0, len(p.used))
	for g := range p.used {
		gids = append(gids, int(g))
	}
	sort.Ints(gids)
	var widths, bf strings.Builder
	for _, g := range gids {
		fmt.Fprintf(&widths, "%d [%d] ", g, scale(int(f.advance[g])))
		fmt.Fprintf(&bf, "<%04X> <", g)
		for _, u := range utf16.Encode([]rune{p.used[uint16(g)]}) {
			fmt.Fprintf(&bf, "%04X", u)
		}
		bf.WriteString(">\n")
	}
	const firstImage = 11
	xobj := ""
	n := firstImage
	for i, im := range p.images {
		xobj += fmt.Sprintf("/Im%d %d 0 R ", i+1, n)
		n++
		if im.alpha != nil {
			n++
		}
	}
	hexTitle := "FEFF"
	for _, u := range utf16.Encode([]rune(title)) {
		hexTitle += fmt.Sprintf("%04X", u)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>", nil)
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>", nil)
	obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> /XObject << %s>> >> >>", p.W, p.H, xobj), nil)
	stream("", p.ops.Bytes())
	obj("<< /Type /Font /Subtype /Type0 /BaseFont /KafedraFont /Encoding /Identity-H /DescendantFonts [6 0 R] /ToUnicode 9 0 R >>", nil)
	obj(fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /KafedraFont /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor 7 0 R /DW 1000 /W [ %s] /CIDToGIDMap /Identity >>", widths.String()), nil)
	obj(fmt.Sprintf("<< /Type /FontDescriptor /FontName /KafedraFont /Flags 4 /FontBBox [%d %d %d %d] /ItalicAngle 0 /Ascent %d /Descent %d /CapHeight %d /StemV 80 /FontFile2 8 0 R >>",
		scale(f.bbox[0]), scale(f.bbox[1]), scale(f.bbox[2]), scale(f.bbox[3]), scale(f.ascent), scale(f.descent), scale(f.capHeight)), nil)
	stream(fmt.Sprintf("/Length1 %d", len(f.data)), f.data)
	stream("", []byte("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n"+
		chunkBF(bf.String())+"endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n"))
	obj(fmt.Sprintf("<< /Title <%s> /Producer (Kafedra) >>", hexTitle), nil)
	n = firstImage
	for _, im := range p.images {
		dict := fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8", im.w, im.h)
		if im.alpha != nil {
			dict += fmt.Sprintf(" /SMask %d 0 R", n+1)
		}
		stream(dict, im.rgb)
		n++
		if im.alpha != nil {
			stream(fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8", im.w, im.h), im.alpha)
			n++
		}
	}
	// заголовок с двоичной меткой и таблица ссылок
	final := bytes.NewBufferString("%PDF-1.5\n%\xE2\xE3\xCF\xD3\n")
	shift := final.Len()
	final.Write(out.Bytes())
	xref := final.Len()
	fmt.Fprintf(final, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(final, "%010d 00000 n \n", o+shift)
	}
	fmt.Fprintf(final, "trailer\n<< /Size %d /Root 1 0 R /Info 10 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return final.Bytes()
}

// chunkBF оформляет соответствия «глиф → символ» блоками не более 100 записей (требование формата).
func chunkBF(entries string) string {
	lines := strings.Split(strings.TrimSpace(entries), "\n")
	var b strings.Builder
	for i := 0; i < len(lines); i += 100 {
		end := min(i+100, len(lines))
		fmt.Fprintf(&b, "%d beginbfchar\n%s\nendbfchar\n", end-i, strings.Join(lines[i:end], "\n"))
	}
	return b.String()
}
