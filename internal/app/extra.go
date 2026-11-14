package app

import (
	"bytes"

	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"io"

	"mime"

	"net/http"

	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"kafedra/internal/content"
	"kafedra/internal/secure"
)

// ---------- импорт пользователей по шаблону (Email, Фамилия, Имя, Отчество, Роль) ----------

func (c *Ctx) templateUsers() {
	c.sendXLSX("Шаблон — пользователи.xlsx",
		content.Sheet{Name: "Пользователи", HeaderRows: 1, Widths: []float64{30, 20, 18, 20, 16, 14, 18}, Rows: [][]any{
			{"Email", "Фамилия", "Имя", "Отчество", "Роль", "Группа", "Логин"},
			{"ivanov@example.ru", "Иванов", "Пётр", "Сергеевич", "Учащийся", "ПИ-21", ""},
			{"smirnova@example.ru", "Смирнова", "Анна", "", "Преподаватель", "", "smirnova"},
		}},
		content.Sheet{Name: "Как заполнять", Widths: []float64{120}, Rows: [][]any{
			{"1. Первую строку (заголовок) не изменяйте. Строки-примеры замените своими. Одна строка — один пользователь."},
			{"2. Обязательные поля: Email, Фамилия, Имя, Роль. Отчество — при наличии."},
			{"3. Роль: «Учащийся» или «Преподаватель»."},
			{"4. Преподаватель курса может загружать только учащихся; название группы он указывает в форме загрузки, столбец «Группа» при этом не используется."},
			{"5. Логин необязателен: если он не указан, составляется из фамилии и инициалов (ivanov.ps)."},
			{"6. Проверяется: формат адреса, заполнение обязательных полей, повтор адреса в файле и совпадение с адресом уже существующего пользователя."},
			{"7. Строки с ошибками не создаются; причина по каждой строке указана в файле результата."},
		}})
}

var userHeaders = map[string]string{
	"email": "email", "e-mail": "email", "эл. почта": "email", "почта": "email", "электронная почта": "email", "адрес электронной почты": "email",
	"фамилия": "last", "имя": "first", "отчество": "middle", "роль": "role", "группа": "group", "логин": "login", "фио": "fio",
}

var roleWords = map[string]string{"учащийся": RoleStudent, "студент": RoleStudent, "слушатель": RoleStudent, "student": RoleStudent,
	"преподаватель": RoleTeacher, "teacher": RoleTeacher}

// userRowsFromSheet разбирает лист шаблона пользователей. Столбцы определяются по заголовкам,
// поэтому их порядок не важен. fixedGroup задан — группа берётся из формы (импорт преподавателем).
func userRowsFromSheet(sheet [][]string, fixedGroup string) ([]importRow, error) {
	head := -1
	cols := map[string]int{}
	for i, r := range sheet {
		if strings.TrimSpace(strings.Join(r, "")) == "" {
			continue
		}
		for j, v := range r {
			key := strings.ToLower(strings.TrimSpace(v))
			if k := strings.Index(key, "("); k > 0 { // «Логин (необязательно)»
				key = strings.TrimSpace(key[:k])
			}
			if name, ok := userHeaders[key]; ok {
				if _, dup := cols[name]; dup {
					return nil, fmt.Errorf("повторяется заголовок «%s»", v)
				}
				cols[name] = j
			}
		}
		head = i
		break
	}
	_, hasLast := cols["last"]
	_, hasFIO := cols["fio"]
	if head < 0 || (!hasLast && !hasFIO) {
		return nil, errors.New("не найдена строка заголовка: первая строка должна содержать названия столбцов шаблона (Email, Фамилия, Имя…)")
	}
	_, hasRole := cols["role"]
	_, hasFirst := cols["first"]
	_, hasEmail := cols["email"]
	if hasLast && (!hasRole || !hasFirst || !hasEmail) {
		return nil, errors.New("в шаблоне нужны столбцы «Email», «Фамилия», «Имя» и «Роль»")
	}
	get := func(r []string, name string) string {
		if j, ok := cols[name]; ok {
			return cell(r, j)
		}
		return ""
	}
	var rows []importRow
	for i := head + 1; i < len(sheet); i++ {
		r := sheet[i]
		if strings.TrimSpace(strings.Join(r, "")) == "" {
			continue
		}
		row := importRow{Line: i + 1, Email: get(r, "email"), Login: get(r, "login"), Group: get(r, "group")}
		var missing []string
		if hasLast {
			last, first := get(r, "last"), get(r, "first")
			if last == "" {
				missing = append(missing, "Фамилия")
			}
			if first == "" {
				missing = append(missing, "Имя")
			}
			row.Name = strings.Join(strings.Fields(last+" "+first+" "+get(r, "middle")), " ")
		} else {
			row.Name = get(r, "fio")
			if row.Name == "" {
				missing = append(missing, "ФИО")
			}
		}
		if row.Email == "" {
			missing = append(missing, "Email")
		}
		roleValue := get(r, "role")
		role, ok := roleWords[strings.ToLower(roleValue)]
		if roleValue == "" && fixedGroup != "" && !hasLast {
			role, ok = RoleStudent, true // старый шаблон списка группы
		} else if roleValue == "" {
			missing = append(missing, "Роль")
		}
		row.Role = role
		switch {
		case len(missing) > 0:
			row.Result = "ошибка: не заполнены обязательные поля: " + strings.Join(missing, ", ")
		case !ok:
			row.Result = "ошибка: неизвестная роль «" + get(r, "role") + "» (допустимо: Учащийся, Преподаватель)"
			row.Role = RoleStudent
		}
		if fixedGroup != "" {
			row.Group = fixedGroup
			if ok && role != RoleStudent && row.Result == "" {
				row.Result = "ошибка: преподаватель курса может добавлять только учащихся"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (c *Ctx) templateUsersCSV() {
	var buf bytes.Buffer
	buf.WriteString("\ufeff")
	w := csv.NewWriter(&buf)
	w.Comma = ';'
	w.UseCRLF = true
	w.Write([]string{"Email", "Фамилия", "Имя", "Отчество", "Роль", "Группа", "Логин"})
	w.Flush()
	c.sendFile("Шаблон — пользователи.csv", "text/csv; charset=utf-8", buf.Bytes())
}

func (c *Ctx) readUserImport(fixedGroup string) ([]importRow, error) {
	if c.R.MultipartForm == nil || len(c.R.MultipartForm.File["file"]) != 1 {
		return nil, errors.New("выберите один файл CSV или XLSX")
	}
	fh := c.R.MultipartForm.File["file"][0]
	switch strings.ToLower(filepath.Ext(fh.Filename)) {
	case ".xlsx":
		rows, err := c.readUploadXLSX()
		if err != nil {
			return nil, err
		}
		return userRowsFromSheet(rows, fixedGroup)
	case ".csv":
		if fh.Size > 4<<20 {
			return nil, errors.New("файл CSV больше 4 МиБ")
		}
		f, err := fh.Open()
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		if err != nil {
			return nil, err
		}
		if len(b) > 4<<20 {
			return nil, errors.New("файл CSV больше 4 МиБ")
		}
		return readUserCSV(strings.TrimPrefix(string(b), "\ufeff"), fixedGroup)
	default:
		return nil, errors.New("поддерживаются файлы CSV и XLSX")
	}
}

func readUserCSV(raw, fixedGroup string) ([]importRow, error) {
	if !utf8.ValidString(raw) {
		// Русская версия Excel сохраняет «CSV (разделители — точка с запятой)» в Windows-1251.
		raw = decodeCP1251(raw)
	}
	bestScore := -1
	var best [][]string
	for _, delimiter := range []rune{';', ','} {
		r := csv.NewReader(strings.NewReader(raw))
		r.Comma, r.FieldsPerRecord, r.TrimLeadingSpace = delimiter, -1, true
		var sheet [][]string
		score := 0
		for {
			record, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				sheet = nil
				break
			}
			line, _ := r.FieldPos(0)
			for i, v := range record {
				record[i] = strings.TrimSpace(v)
			}
			if strings.Join(record, "") == "" {
				continue
			}
			if len(sheet) == 0 {
				for _, name := range record {
					if _, ok := userHeaders[strings.ToLower(name)]; ok {
						score++
					}
				}
				if score == 0 && len(record) == 5 {
					score = 1 // прежний CSV без заголовка
				}
			}
			for len(sheet) < line {
				sheet = append(sheet, nil)
			}
			sheet[line-1] = record
			if len(sheet) > 15000 {
				return nil, errors.New("слишком много строк CSV")
			}
		}
		if len(sheet) > 0 && score > bestScore {
			bestScore, best = score, sheet
		}
	}
	if best == nil {
		return nil, errors.New("не удалось прочитать CSV: проверьте кавычки и разделитель")
	}
	if bestScore > 1 {
		for i, r := range best {
			if len(r) == 0 {
				continue
			}
			if fixedGroup == "" && (strings.EqualFold(cell(r, 0), "логин") || strings.EqualFold(cell(r, 0), "login")) && strings.EqualFold(cell(r, 1), "ФИО") {
				best[i] = nil
				break
			}
			return userRowsFromSheet(best, fixedGroup)
		}
	}
	// Сохраняем чтение прежних CSV, содержащих логин, ФИО, роль, группу и почту.
	if fixedGroup != "" {
		return userRowsFromSheet(best, fixedGroup)
	}
	var rows []importRow
	for i, r := range best {
		if len(r) == 0 {
			continue
		}
		role, ok := roleWords[strings.ToLower(cell(r, 2))]
		switch strings.ToLower(cell(r, 2)) {
		case "admin", "администратор":
			role, ok = RoleAdmin, true
		case "":
			role, ok = RoleStudent, true
		}
		row := importRow{Line: i + 1, Login: cell(r, 0), Name: cell(r, 1), Role: role, Group: cell(r, 3), Email: cell(r, 4)}
		if len(r) != 5 || !ok || row.Login == "" {
			row.Result = "ошибка: используйте шаблон или прежний формат из пяти полей"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// cp1251High — символы Windows-1251 с кодами 0x80–0xBF (0xC0–0xFF — буквы А…я подряд).
var cp1251High = [64]rune{
	0x0402, 0x0403, 0x201A, 0x0453, 0x201E, 0x2026, 0x2020, 0x2021, 0x20AC, 0x2030, 0x0409, 0x2039, 0x040A, 0x040C, 0x040B, 0x040F,
	0x0452, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0xFFFD, 0x2122, 0x0459, 0x203A, 0x045A, 0x045C, 0x045B, 0x045F,
	0x00A0, 0x040E, 0x045E, 0x0408, 0x00A4, 0x0490, 0x00A6, 0x00A7, 0x0401, 0x00A9, 0x0404, 0x00AB, 0x00AC, 0x00AD, 0x00AE, 0x0407,
	0x00B0, 0x00B1, 0x0406, 0x0456, 0x0491, 0x00B5, 0x00B6, 0x00B7, 0x0451, 0x2116, 0x0454, 0x00BB, 0x0458, 0x0405, 0x0455, 0x0457,
}

// decodeCP1251 переводит текст из Windows-1251 в UTF-8.
func decodeCP1251(raw string) string {
	var b strings.Builder
	b.Grow(len(raw) * 2)
	for i := 0; i < len(raw); i++ {
		switch ch := raw[i]; {
		case ch < 0x80:
			b.WriteByte(ch)
		case ch < 0xC0:
			b.WriteRune(cp1251High[ch-0x80])
		default:
			b.WriteRune(rune(ch-0xC0) + 0x0410)
		}
	}
	return b.String()
}

// markDuplicateEmails отмечает повторы адреса внутри файла.
func markDuplicateEmails(rows []importRow) {
	seen := map[string]int{}
	for i := range rows {
		key := strings.ToLower(rows[i].Email)
		if key == "" || rows[i].Result != "" {
			continue
		}
		if line, dup := seen[key]; dup {
			rows[i].Result = fmt.Sprintf("ошибка: адрес повторяется в файле (строка %d)", line)
			continue
		}
		seen[key] = rows[i].Line
	}
}

// ---------- аналитика курса и экспорт результатов ----------

type anRow struct {
	Name, Email, Group   string
	Points, Max, Percent float64
	Passed, HasData      bool
}

func inPeriod(t, from, to time.Time) bool {
	return (from.IsZero() && to.IsZero()) || (!t.IsZero() && (from.IsZero() || !t.Before(from)) && (to.IsZero() || t.Before(to.AddDate(0, 0, 1))))
}

// analytics собирает результаты учащихся: по одному тесту либо по курсу в целом.
// Период отбирает попытки по времени завершения, работы и оценки — по времени выставления.
func (c *Ctx) analytics(co *Course, group, itemID string, from, to time.Time) ([]anRow, string) {
	names := c.groupNames()
	students := c.students(co, group)
	var rows []anRow
	if it, ok := c.Items.Get(itemID); ok && it.CourseID == co.ID && it.Kind == KindTest {
		best := map[string]*Attempt{}
		passed := map[string]bool{}
		for _, at := range c.Attempts.Find(func(x *Attempt) bool { return x.ItemID == it.ID && x.Done }) {
			if !inPeriod(at.Finished, from, to) {
				continue
			}
			if b := best[at.UserID]; b == nil || at.Points > b.Points {
				cp := at
				best[at.UserID] = &cp
			}
			passed[at.UserID] = passed[at.UserID] || at.passed(&it)
		}
		for _, u := range students {
			r := anRow{Name: u.FullName, Email: u.Email, Group: names[u.GroupID], Max: it.MaxPoints}
			if b := best[u.ID]; b != nil {
				r.Points, r.Max, r.HasData = b.Points, b.grading(&it).MaxPoints, true
				r.Percent, r.Passed = percent(r.Points, r.Max), passed[u.ID]
			}
			rows = append(rows, r)
		}
		return rows, "Тест: " + it.Title
	}
	g := c.gradebook(co.ID)
	items := map[string]Item{}
	for _, it := range c.courseItems(co.ID, true) {
		items[it.ID] = it
	}
	pts := map[string]map[string]float64{}
	put := func(u, col string, v float64, best bool) {
		if pts[u] == nil {
			pts[u] = map[string]float64{}
		}
		if old, ok := pts[u][col]; !ok || !best || v > old {
			pts[u][col] = v
		}
	}
	for _, at := range c.Attempts.Find(func(x *Attempt) bool { return x.CourseID == co.ID && x.Done }) {
		if _, ok := items[at.ItemID]; ok && inPeriod(at.Finished, from, to) {
			put(at.UserID, at.ItemID, at.Points, true)
		}
	}
	for _, s := range c.Subs.Find(func(x *Submission) bool { return x.CourseID == co.ID && x.Graded }) {
		if _, ok := items[s.ItemID]; ok && inPeriod(s.GradedAt, from, to) {
			put(s.UserID, s.ItemID, s.Points, false)
		}
	}
	manual := map[string]bool{}
	for _, col := range g.Cols {
		manual[col.ID] = col.Manual
	}
	for _, gr := range c.Grades.Find(func(x *Grade) bool { return x.CourseID == co.ID }) {
		if manual[gr.ColumnID] && inPeriod(gr.At, from, to) {
			put(gr.UserID, gr.ColumnID, gr.Points, false)
		}
	}
	completion := c.courseCompletion(co.ID, "", from, to)
	for _, u := range students {
		r := anRow{Name: u.FullName, Email: u.Email, Group: names[u.GroupID], Max: g.Max, HasData: len(pts[u.ID]) > 0}
		for _, v := range pts[u.ID] {
			r.Points += v
		}
		r.Percent, r.Passed = percent(r.Points, r.Max), completion.complete(u.ID)
		rows = append(rows, r)
	}
	return rows, "Курс в целом"
}

func (c *Ctx) analyticsFilters(co *Course) (group, itemID string, from, to time.Time, ok bool) {
	q := c.R.URL.Query()
	group, itemID = q.Get("group"), q.Get("test")
	if group != "" && !contains(co.GroupIDs, group) {
		c.Fail(http.StatusBadRequest, "Эта группа не относится к курсу.")
		return
	}
	if itemID != "" {
		it, found := c.Items.Get(itemID)
		if !found || it.CourseID != co.ID || it.Kind != KindTest {
			c.Fail(http.StatusBadRequest, "Этот тест не относится к курсу.")
			return
		}
	}
	for _, field := range []string{"from", "to"} {
		raw := q.Get(field)
		if raw == "" {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", raw, c.Loc)
		if err != nil {
			c.Fail(http.StatusBadRequest, "Некорректная дата периода.")
			return group, itemID, from, to, false
		}
		if field == "from" {
			from = date
		} else {
			to = date
		}
	}
	if !to.IsZero() && from.After(to) {
		c.Fail(http.StatusBadRequest, "Дата начала позже даты окончания.")
		return group, itemID, from, to, false
	}
	return group, itemID, from, to, true
}

func (c *Ctx) analyticsPage() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	group, itemID, from, to, ok := c.analyticsFilters(co)
	if !ok {
		return
	}
	rows, scope := c.analytics(co, group, itemID, from, to)
	var tests []Item
	for _, it := range c.courseItems(co.ID, false) {
		if it.Kind == KindTest {
			tests = append(tests, it)
		}
	}
	passed, with, sum := 0, 0, 0.0
	for _, r := range rows {
		if r.Passed {
			passed++
		}
		if r.HasData {
			with++
			sum += r.Percent
		}
	}
	avg := 0.0
	if with > 0 {
		avg = float64(int(sum/float64(with)*10+0.5)) / 10
	}
	c.Render("analytics", map[string]any{"Course": co, "Tab": "analytics", "Manage": true, "Rows": rows, "Scope": scope, "Tests": tests,
		"Group": group, "Test": itemID, "From": c.R.URL.Query().Get("from"), "To": c.R.URL.Query().Get("to"), "GroupNames": c.groupNames(),
		"Passed": passed, "With": with, "Avg": avg, "Query": c.R.URL.RawQuery})
}

func (c *Ctx) analyticsXLSX() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	group, itemID, from, to, ok := c.analyticsFilters(co)
	if !ok {
		return
	}
	rows, scope := c.analytics(co, group, itemID, from, to)
	out := [][]any{{"ФИО", "Email", "Группа", "Набранные баллы", "Максимальный балл", "Процент выполнения", "Статус"}}
	for _, r := range rows {
		status := "Не пройдено"
		if r.Passed {
			status = "Пройдено"
		}
		var points, pct any
		if r.HasData {
			points, pct = r.Points, r.Percent
		}
		out = append(out, []any{r.Name, r.Email, r.Group, points, r.Max, pct, status})
	}
	period := "весь период"
	if !from.IsZero() || !to.IsZero() {
		period = fmtTime(from, c.Loc, "02.01.2006") + " — " + fmtTime(to, c.Loc, "02.01.2006")
	}
	info := [][]any{{"Курс", co.Title}, {"Область", scope}, {"Группа", orAll(c.groupNames()[group])}, {"Период", period},
		{"Сформировано", time.Now().In(c.Loc).Format("02.01.2006 15:04")},
		{"Статус", "по тесту — набран порог зачёта; по курсу — изучены материалы, тесты сданы на зачёт, задания проверены"}}
	c.Log("экспорт результатов", co.Title, true, scope)
	c.sendXLSX(cleanName("Результаты — "+co.Title)+".xlsx",
		content.Sheet{Name: "Результаты", HeaderRows: 1, Widths: []float64{36, 28, 12, 16, 18, 20, 14}, Rows: out},
		content.Sheet{Name: "Параметры", Widths: []float64{16, 90}, Rows: info})
}

func orAll(s string) string {
	if s == "" {
		return "все группы курса"
	}
	return s
}

// ---------- анкета перед началом обучения ----------

// questBlocked сообщает, что учащийся ещё не допущен к материалам: анкета не проверена.
func (c *Ctx) questBlocked() bool {
	return c.User != nil && c.User.Role == RoleStudent && c.S().QuestRequired && c.User.QuestApproved.IsZero()
}

// questFree — страницы, доступные до проверки анкеты.
func questFree(p string) bool {
	return p == "/questionnaire" || p == "/password" || p == "/notices" || p == "/mfa" ||
		strings.HasPrefix(p, "/profile") || strings.HasPrefix(p, "/docs/")
}

func (c *Ctx) questAddress() string {
	if a := strings.TrimSpace(c.S().QuestEmail); validEmail(a) {
		return a
	}
	for _, t := range c.teacherMails() {
		if validEmail(t.Email) {
			return t.Email
		}
	}
	return ""
}

func (c *Ctx) questPage() {
	if c.User.Role != RoleStudent {
		c.Deny()
		return
	}
	_, _, fontErr := c.certFont()
	c.Render("questionnaire", map[string]any{"Address": c.questAddress(), "Required": c.S().QuestRequired, "HasPDF": fontErr == nil})
}

func (c *Ctx) questSent() {
	if !c.User.QuestSent.IsZero() || !c.User.QuestApproved.IsZero() {
		c.Redirect("/questionnaire", "")
		return
	}
	if err := c.Users.Update(c.User.ID, func(x *User) error { x.QuestSent = time.Now(); return nil }); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить отметку.")
		return
	}
	c.Log("анкета: отмечена отправка", "", true, "")
	for _, admin := range c.Users.Find(func(u *User) bool { return u.Role == RoleAdmin && !u.Disabled }) {
		c.notify(admin.ID, c.User.FullName+" отметил(а) отправку анкеты.", "/admin/users/"+c.User.ID)
	}
	c.Redirect("/questionnaire", "Отметка сохранена. Доступ к обучению откроется после проверки анкеты.")
}

// questDecide — открыть или закрыть доступ учащемуся после проверки анкеты.
func (c *Ctx) questDecide() {
	u, ok := c.Users.Get(c.R.PathValue("id"))
	if !ok || u.Role != RoleStudent || c.User.Role != RoleAdmin {
		c.Deny()
		return
	}
	action := c.F("action")
	if action != "approve" && action != "revoke" {
		c.Fail(http.StatusBadRequest, "Выберите действие по анкете.")
		return
	}
	approve := action == "approve"
	if err := c.Users.Update(u.ID, func(x *User) error {
		if approve {
			x.QuestApproved, x.QuestBy = time.Now(), c.User.ID
		} else {
			x.QuestApproved, x.QuestBy, x.QuestSent = time.Time{}, "", time.Time{}
		}
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить решение.")
		return
	}
	if approve {
		c.Log("анкета: доступ к обучению открыт", u.Login, true, "")
		c.notify(u.ID, "Анкета проверена, доступ к обучению открыт.", "/courses")
	} else {
		c.Log("анкета: доступ к обучению закрыт", u.Login, true, "")
	}
	c.Redirect(c.Back("/"), "Сохранено.")
}

// questApproveGroup — администратор открывает доступ учащимся курса разом: всем, кто отметил
// отправку анкеты, либо (scope=all) всем учащимся курса. Второй режим нужен, когда анкету
// включают на работающей системе: уже обучающиеся иначе остались бы без доступа.
func (c *Ctx) questApproveGroup() {
	co, ok := c.Courses.Get(c.R.PathValue("id"))
	if !ok || c.User.Role != RoleAdmin {
		c.Deny()
		return
	}
	all := c.F("scope") == "all"
	n := 0
	for _, u := range c.students(&co, "") {
		if !u.QuestApproved.IsZero() || (!all && u.QuestSent.IsZero()) {
			continue
		}
		if c.Users.Update(u.ID, func(x *User) error { x.QuestApproved, x.QuestBy = time.Now(), c.User.ID; return nil }) == nil {
			n++
			c.notify(u.ID, "Анкета проверена, доступ к обучению открыт.", "/courses")
		}
	}
	scope := "отметившим отправку анкеты"
	if all {
		scope = "всем учащимся курса"
	}
	c.Log("анкета: доступ к обучению открыт группе", co.Title, true, fmt.Sprintf("%s, учащихся: %d", scope, n))
	c.Redirect("/courses/"+co.ID+"?tab=people", fmt.Sprintf("Доступ открыт учащимся: %d.", n))
}

// ---------- бланки в PDF ----------

func (c *Ctx) sendParasPDF(name string, paras []content.Para) {
	font, _, err := c.certFont()
	var pdf []byte
	if err == nil {
		pdf, err = content.ParasPDF(font, strings.TrimSuffix(name, ".pdf"), paras)
	}
	if err != nil {
		c.Log("формирование бланка PDF", name, false, err.Error())
		c.Fail(http.StatusServiceUnavailable, "Не удалось сформировать PDF (на сервере не настроен шрифт). Скачайте бланк в формате Word или сообщите администратору.")
		return
	}
	c.sendFile(name, "application/pdf", pdf)
}

func (c *Ctx) docAnketaPDF() { c.sendParasPDF("Анкета слушателя.pdf", c.anketaParas()) }

func (c *Ctx) docZayavleniePDF() {
	co, ok := c.course(c.R.PathValue("id"), false)
	if !ok {
		return
	}
	if !c.completed(c.User.ID, co.ID) {
		c.Redirect("/courses/"+co.ID, "!Заявление доступно после выполнения всех обязательных заданий курса.")
		return
	}
	c.sendParasPDF("Заявление на выдачу сертификата.pdf", c.zayavlenieParas(co))
}

// ---------- скан подписанного заявления ----------

func (c *Ctx) applicationUpload() {
	co, ok := c.course(c.R.PathValue("id"), false)
	if !ok {
		return
	}
	back := "/courses/" + co.ID
	if !c.completed(c.User.ID, co.ID) {
		c.Redirect(back, "!Заявление принимается после выполнения всех обязательных заданий курса.")
		return
	}
	if c.R.MultipartForm == nil || len(c.R.MultipartForm.File["file"]) != 1 {
		c.Redirect(back, "!Выберите один файл со сканом или фотографией заявления.")
		return
	}
	fh := c.R.MultipartForm.File["file"][0]
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fh.Filename), "."))
	if !contains([]string{"pdf", "jpg", "jpeg", "png"}, ext) {
		c.Redirect(back, "!Принимаются скан или фотография в формате PDF, JPG или PNG.")
		return
	}
	if fh.Size > 20<<20 {
		c.Redirect(back, "!Файл больше 20 МБ. Уменьшите разрешение скана.")
		return
	}
	if _, issued := c.Certs.First(func(ct *Certificate) bool { return ct.UserID == c.User.ID && ct.CourseID == co.ID }); issued {
		c.Redirect(back, "!Сертификат уже выдан.")
		return
	}
	if err := validateRestrictedUpload(fh, ext); err != nil {
		c.Redirect(back, "!"+err.Error())
		return
	}
	m, err := c.saveUpload(fh, co.ID, "application", "")
	if err != nil {
		c.Redirect(back, "!Файл не принят: "+err.Error()+".")
		return
	}
	id := co.ID + ":" + c.User.ID
	old, had := c.CertApps.Get(id)
	if err := c.CertApps.Put(id, CertApplication{ID: id, CourseID: co.ID, UserID: c.User.ID, FileID: m.ID, Uploaded: time.Now()}); err != nil {
		c.removeFile(m.ID)
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить заявление.")
		return
	}
	if had {
		c.removeFile(old.FileID)
	}
	c.Log("заявление на сертификат загружено", co.Title, true, "")
	for _, t := range co.TeacherIDs {
		c.notify(t, c.User.FullName+" загрузил(а) заявление на сертификат по курсу «"+co.Title+"».", "/courses/"+co.ID+"?tab=people")
	}
	c.Redirect(back, "Заявление загружено.")
}

func (c *Ctx) applicationDecision() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	back := "/courses/" + co.ID + "?tab=people"
	id := co.ID + ":" + c.F("user")
	ap, ok := c.CertApps.Get(id)
	if _, issued := c.Certs.First(func(ct *Certificate) bool { return ct.UserID == ap.UserID && ct.CourseID == co.ID }); !ok || issued || ap.FileID != c.F("file") {
		c.Redirect(back, "!Заявление изменилось или сертификат уже выдан.")
		return
	}
	action := c.F("action")
	if action != "approve" && action != "reject" {
		c.Fail(http.StatusBadRequest, "Выберите решение по заявлению.")
		return
	}
	m, ok := c.Files.Get(ap.FileID)
	if action == "approve" && (!ok || !c.checkApplicationFile(ap, m, true)) {
		c.Redirect(back, "!Файл заявления недоступен. Попросите учащегося загрузить его повторно.")
		return
	}
	if err := c.CertApps.Update(id, func(x *CertApplication) error {
		x.Decision, x.ReviewedBy = action, c.User.ID
		x.Reviewed = time.Now()
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить решение.")
		return
	}
	c.Log("проверка заявления", co.Title, true, action+" "+ap.UserID)
	msg := "Заявление проверено. Сертификат можно получить на странице курса."
	if action == "reject" {
		msg = "Заявление требуется загрузить повторно."
	}
	c.notify(ap.UserID, msg, "/courses/"+co.ID)
	c.Redirect(back, "Решение сохранено.")
}

func (a *App) checkApplicationFile(ap CertApplication, m FileMeta, verifyHash bool) bool {
	if !fileIDPattern.MatchString(m.ID) || (m.Scope != "application" && m.Scope != "sub") ||
		m.OwnerID != ap.UserID || m.CourseID != ap.CourseID {
		return false
	}
	path := a.filePath(m.ID)
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() != m.Size {
		return false
	}
	if !verifyHash {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return copyVerifiedFile(io.Discard, f, &m) == nil
}

// ---------- отправка сертификата по электронной почте ----------

func mimeWord(s string) string { return mime.QEncoding.Encode("utf-8", s) }

// buildCertMail формирует письмо с вложенным PDF.
func buildCertMail(from, to, subject, text, fileName string, pdf []byte, id string) []byte {
	boundary := "kafedra-" + secure.NewID()
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@kafedra>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n",
		from, to, mimeWord(subject), time.Now().Format(time.RFC1123Z), id, boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", boundary)
	writeBase64(&b, []byte(text))
	fmt.Fprintf(&b, "--%s\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=%q; filename*=UTF-8''%s\r\n\r\n",
		boundary, "certificate.pdf", url.PathEscape(fileName))
	writeBase64(&b, pdf)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

func writeBase64(b *bytes.Buffer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
}
