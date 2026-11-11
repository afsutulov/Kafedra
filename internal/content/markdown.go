// Пакет content — обработка учебных материалов: собственный преобразователь
// разметки Markdown в безопасный HTML и импорт документов DOCX.
package content

import (
	"html"
	"regexp"
	"strings"
)

// Render преобразует текст в разметке Markdown в HTML.
// Любой HTML во входном тексте экранируется: результат безопасен для вставки в страницу.
func Render(src string) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\t", "    ")
	var b strings.Builder
	renderBlocks(&b, strings.Split(src, "\n"), 0)
	return b.String()
}

var (
	reOL    = regexp.MustCompile(`^(\d{1,9})[.)] +`)
	reUL    = regexp.MustCompile(`^[-*+] +`)
	reHR    = regexp.MustCompile(`^ {0,3}([-*_])( *[-*_]){2,} *$`)
	reTblSp = regexp.MustCompile(`^ *\|?( *:?-{1,}:? *\|)+( *:?-{1,}:? *)?$`)
)

const maxDepth = 12

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

func listMarker(s string) (ordered bool, rest string, ok bool) {
	t := strings.TrimLeft(s, " ")
	if m := reUL.FindString(t); m != "" && !reHR.MatchString(s) {
		return false, t[len(m):], true
	}
	if m := reOL.FindString(t); m != "" {
		return true, t[len(m):], true
	}
	return false, "", false
}

func renderBlocks(b *strings.Builder, lines []string, depth int) {
	if depth > maxDepth {
		b.WriteString("<p>" + html.EscapeString(strings.Join(lines, " ")) + "</p>\n")
		return
	}
	i := 0
	for i < len(lines) {
		line := lines[i]
		trim := strings.TrimSpace(line)
		switch {
		case trim == "":
			i++
		case strings.HasPrefix(trim, "```"):
			lang := strings.TrimSpace(strings.TrimPrefix(trim, "```"))
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++
			b.WriteString("<pre><code")
			if lang != "" && regexp.MustCompile(`^[A-Za-z0-9+#_-]{1,20}$`).MatchString(lang) {
				b.WriteString(` class="lang-` + lang + `"`)
			}
			b.WriteString(">" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case strings.HasPrefix(trim, "#"):
			n := len(trim) - len(strings.TrimLeft(trim, "#"))
			if n > 5 || len(trim) == n || trim[n] != ' ' {
				i = paragraph(b, lines, i)
				continue
			}
			tag := "h" + string(rune('1'+n)) // «#» → h2: заголовок h1 занят названием страницы
			b.WriteString("<" + tag + ">" + inline(strings.TrimSpace(strings.TrimRight(trim[n:], "# ")), 0) + "</" + tag + ">\n")
			i++
		case reHR.MatchString(line):
			b.WriteString("<hr>\n")
			i++
		case strings.HasPrefix(trim, ">"):
			var q []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				q = append(q, strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">"), " "))
				i++
			}
			b.WriteString("<blockquote>\n")
			renderBlocks(b, q, depth+1)
			b.WriteString("</blockquote>\n")
		case strings.Contains(line, "|") && i+1 < len(lines) && reTblSp.MatchString(lines[i+1]):
			i = table(b, lines, i)
		default:
			if _, _, ok := listMarker(line); ok {
				i = list(b, lines, i, depth)
			} else {
				i = paragraph(b, lines, i)
			}
		}
	}
}

func paragraph(b *strings.Builder, lines []string, i int) int {
	var p []string
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "```") || (len(p) > 0 && (strings.HasPrefix(t, "#") || strings.HasPrefix(t, ">") || reHR.MatchString(lines[i]))) {
			break
		}
		if _, _, ok := listMarker(lines[i]); ok && len(p) > 0 {
			break
		}
		p = append(p, inline(t, 0))
		i++
	}
	b.WriteString("<p>" + strings.Join(p, "<br>\n") + "</p>\n")
	return i
}

func splitRow(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	cells := strings.Split(s, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func table(b *strings.Builder, lines []string, i int) int {
	head := splitRow(lines[i])
	i += 2
	b.WriteString("<div class=\"table-wrap\"><table>\n<thead><tr>")
	for _, c := range head {
		b.WriteString("<th>" + inline(c, 0) + "</th>")
	}
	b.WriteString("</tr></thead>\n<tbody>\n")
	for i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
		cells := splitRow(lines[i])
		b.WriteString("<tr>")
		for j := range head {
			c := ""
			if j < len(cells) {
				c = cells[j]
			}
			b.WriteString("<td>" + inline(c, 0) + "</td>")
		}
		b.WriteString("</tr>\n")
		i++
	}
	b.WriteString("</tbody></table></div>\n")
	return i
}

func list(b *strings.Builder, lines []string, i, depth int) int {
	base := indentOf(lines[i])
	ordered, _, _ := listMarker(lines[i])
	tag := "ul"
	if ordered {
		tag = "ol"
	}
	b.WriteString("<" + tag + ">\n")
	for i < len(lines) {
		ord, rest, ok := listMarker(lines[i])
		if !ok || indentOf(lines[i]) != base || ord != ordered {
			break
		}
		i++
		var sub []string
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" {
				// пустая строка продолжает пункт, только если далее идёт вложенный текст
				if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" && indentOf(lines[i+1]) > base {
					sub = append(sub, "")
					i++
					continue
				}
				break
			}
			if indentOf(lines[i]) <= base {
				break
			}
			sub = append(sub, lines[i])
			i++
		}
		b.WriteString("<li>" + inline(rest, 0))
		if len(sub) > 0 {
			min := 1 << 30
			for _, s := range sub {
				if strings.TrimSpace(s) != "" && indentOf(s) < min {
					min = indentOf(s)
				}
			}
			for k, s := range sub {
				if len(s) >= min {
					sub[k] = s[min:]
				}
			}
			b.WriteString("\n")
			renderBlocks(b, sub, depth+1)
		}
		b.WriteString("</li>\n")
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			if i+1 < len(lines) {
				if o, _, ok := listMarker(lines[i+1]); ok && indentOf(lines[i+1]) == base && o == ordered {
					i++
					continue
				}
			}
			break
		}
	}
	b.WriteString("</" + tag + ">\n")
	return i
}

// SafeURL сообщает, допустим ли адрес для ссылок и изображений.
func SafeURL(u string) bool {
	if strings.ContainsAny(u, " \"'<>`\n") {
		return false
	}
	l := strings.ToLower(u)
	switch {
	case strings.HasPrefix(l, "https://"), strings.HasPrefix(l, "http://"), strings.HasPrefix(l, "mailto:"), strings.HasPrefix(l, "#"):
		return true
	case strings.HasPrefix(l, "/") && !strings.HasPrefix(l, "//") && !strings.Contains(l, "\\"):
		return true
	}
	return false
}

// inline обрабатывает строчную разметку. Весь текст экранируется.
func inline(s string, depth int) string {
	if depth > 6 {
		return html.EscapeString(s)
	}
	var b strings.Builder
	text := func(t string) { b.WriteString(html.EscapeString(t)) }
	for len(s) > 0 {
		// ближайший управляющий символ
		k := strings.IndexAny(s, "`![*_~\\h")
		if k < 0 {
			text(s)
			break
		}
		text(s[:k])
		s = s[k:]
		switch {
		case s[0] == '\\' && len(s) > 1 && strings.ContainsRune("\\`*_[]()#!~|>-", rune(s[1])):
			text(s[1:2])
			s = s[2:]
		case s[0] == '`':
			if e := strings.IndexByte(s[1:], '`'); e >= 0 {
				b.WriteString("<code>" + html.EscapeString(s[1:1+e]) + "</code>")
				s = s[e+2:]
			} else {
				text("`")
				s = s[1:]
			}
		case strings.HasPrefix(s, "!["), s[0] == '[':
			img := s[0] == '!'
			t := s
			if img {
				t = s[1:]
			}
			cb := strings.Index(t, "](")
			ce := -1
			if cb > 0 {
				ce = strings.IndexByte(t[cb:], ')')
			}
			if cb < 0 || ce < 0 || strings.Contains(t[1:cb], "[") {
				text(s[:1])
				s = s[1:]
				continue
			}
			label, url := t[1:cb], strings.TrimSpace(t[cb+2:cb+ce])
			if !SafeURL(url) {
				text(label)
			} else if img {
				b.WriteString(`<img src="` + html.EscapeString(url) + `" alt="` + html.EscapeString(label) + `" loading="lazy">`)
			} else {
				b.WriteString(`<a href="` + html.EscapeString(url) + `" rel="noopener noreferrer">` + inline(label, depth+1) + `</a>`)
			}
			s = t[cb+ce+1:]
		case strings.HasPrefix(s, "**"), strings.HasPrefix(s, "~~"), strings.HasPrefix(s, "__"):
			mark := s[:2]
			if e := strings.Index(s[2:], mark); e > 0 {
				tag := "strong"
				if mark == "~~" {
					tag = "del"
				}
				b.WriteString("<" + tag + ">" + inline(s[2:2+e], depth+1) + "</" + tag + ">")
				s = s[e+4:]
			} else {
				text(mark)
				s = s[2:]
			}
		case s[0] == '*':
			if e := strings.IndexByte(s[1:], '*'); e > 0 && s[1] != ' ' {
				b.WriteString("<em>" + inline(s[1:1+e], depth+1) + "</em>")
				s = s[e+2:]
			} else {
				text("*")
				s = s[1:]
			}
		case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
			e := strings.IndexAny(s, " \n<>\"')")
			if e < 0 {
				e = len(s)
			}
			u := strings.TrimRight(s[:e], ".,;:!?")
			if SafeURL(u) && len(u) > 8 {
				b.WriteString(`<a href="` + html.EscapeString(u) + `" rel="noopener noreferrer">` + html.EscapeString(u) + `</a>`)
				s = s[len(u):]
			} else {
				text(s[:1])
				s = s[1:]
			}
		default:
			text(s[:1])
			s = s[1:]
		}
	}
	return b.String()
}
