package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // значок сертификата в формате JPEG
	_ "image/png"  // значок сертификата в формате PNG
	"io"
	"math"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kafedra/internal/content"
	"kafedra/internal/secure"
)

const (
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// sendFile отдаёт сформированный документ как вложение.
func (c *Ctx) sendFile(name, mime string, data []byte) {
	h := c.W.Header()
	h.Set("Content-Type", mime)
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	c.W.Write(data)
}

func (c *Ctx) sendXLSX(name string, sheets ...content.Sheet) {
	var buf bytes.Buffer
	if err := content.WriteXLSX(&buf, sheets...); err != nil {
		c.Fail(http.StatusInternalServerError, "Не удалось сформировать файл.")
		return
	}
	c.sendFile(name, mimeXLSX, buf.Bytes())
}

// readUploadXLSX читает первый лист загруженной книги (поле формы file).
func (c *Ctx) readUploadXLSX() ([][]string, error) {
	if c.R.MultipartForm == nil || len(c.R.MultipartForm.File["file"]) == 0 {
		return nil, errors.New("выберите файл Excel (.xlsx)")
	}
	fh := c.R.MultipartForm.File["file"][0]
	if !strings.EqualFold(filepath.Ext(fh.Filename), ".xlsx") {
		return nil, errors.New("нужен файл формата .xlsx (книга Excel); старый формат .xls сохраните как .xlsx")
	}
	if fh.Size > 8<<20 {
		return nil, errors.New("файл слишком велик")
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return content.ReadXLSX(f, fh.Size)
}

func cell(row []string, i int) string {
	if i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

// ---------- шаблоны Excel ----------

func (c *Ctx) templateStudents() {
	c.sendXLSX("Шаблон — список группы.xlsx",
		content.Sheet{Name: "Группа", HeaderRows: 1, Widths: []float64{42, 22, 32}, Rows: [][]any{
			{"ФИО", "Логин (необязательно)", "Эл. почта (необязательно)"},
			{"Иванов Пётр Сергеевич", "", "ivanov@example.ru"},
			{"Кузнецова Мария Олеговна", "kuznetsova", ""},
		}},
		content.Sheet{Name: "Как заполнять", Widths: []float64{110}, Rows: [][]any{
			{"1. На листе «Группа» оставьте первую строку (заголовок) без изменений. Строки-примеры замените своими."},
			{"2. В каждой строке — один учащийся. Обязательно только ФИО."},
			{"3. Логин можно не указывать: он будет составлен из фамилии и инициалов (например, ivanov.ps)."},
			{"4. Логин — латинские буквы, цифры, точка, дефис, подчёркивание; от 3 до 40 символов."},
			{"5. Название группы вводится в форме загрузки на странице курса, а не в файле."},
			{"6. После загрузки вы получите файл с логинами и временными паролями. Раздайте их учащимся и удалите файл."},
		}})
}

func (c *Ctx) templateTest() {
	c.sendXLSX("Шаблон — вопросы теста.xlsx",
		content.Sheet{Name: "Вопросы", HeaderRows: 1, Widths: []float64{52, 9, 24, 24, 24, 24, 24, 24, 24},
			Rows: [][]any{
				{"Вопрос", "Баллы", "Правильный ответ", "Вариант 1", "Вариант 2", "Вариант 3", "Вариант 4", "Вариант 5", "Вариант 6"},
				{"Какой протокол работает на транспортном уровне?", 1, "1", "TCP", "HTTP", "IP"},
				{"Отметьте компилируемые языки", 2, "1;2", "Go", "C++", "Python"},
				{"Сколько бит в байте?", 1, "8; восемь"},
			}},
		content.Sheet{Name: "Как заполнять", Widths: []float64{120}, Rows: [][]any{
			{"1. На листе «Вопросы» оставьте первую строку (заголовок) без изменений. Строки-примеры замените своими."},
			{"2. Один вопрос — одна строка. «Баллы» можно не заполнять: по умолчанию 1."},
			{"3. Вопрос с выбором ответа: укажите номер из заголовка столбца «Вариант 1…6». Пустые столбцы не меняют номера вариантов."},
			{"4. Несколько правильных вариантов: перечислите их номера через точку с запятой, например 1;3."},
			{"5. Вопрос с вводом ответа: варианты не заполняйте, в столбце «Правильный ответ» укажите допустимые ответы через точку с запятой."},
			{"6. Регистр букв и лишние пробелы при проверке введённого ответа не учитываются."},
			{"7. Если в файле есть ошибка, не загружается ни один вопрос — система укажет номер строки."},
		}})
}

// ---------- массовое создание учётных записей ----------

type importRow struct {
	Login, Name, Email, Role, Group string
	Line                            int
	pw, hash                        string
	Result                          string
	Created                         bool
}

var translit = map[rune]string{'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z", 'и': "i", 'й': "y",
	'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts",
	'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya"}

func latin(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if t, ok := translit[r]; ok {
			b.WriteString(t)
		} else if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// loginFromName составляет логин из ФИО: фамилия и инициалы (ivanov.ps).
func loginFromName(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return ""
	}
	login := latin(parts[0])
	ini := ""
	for _, p := range parts[1:] {
		if l := latin(p); l != "" {
			ini += l[:1]
		}
	}
	if len(login) > 30 {
		login = login[:30]
	}
	if login == "" {
		login = "student"
	}
	if ini != "" {
		login += "." + ini[:min(len(ini), 39-len(login))]
	}
	for len(login) < 3 {
		login += "x"
	}
	return login
}

func validateImportRow(r *importRow) string {
	switch {
	case r.Name == "" || utf8.RuneCountInString(r.Name) > 200:
		return "ошибка: укажите ФИО длиной до 200 символов"
	case r.Login != "" && !reLogin.MatchString(strings.ToLower(r.Login)):
		return "ошибка: некорректный логин"
	case roleName(r.Role) == "":
		return "ошибка: неизвестная роль"
	}
	if r.Email != "" {
		addr, err := mail.ParseAddress(r.Email)
		if err != nil || addr.Address != r.Email || len(r.Email) > 254 {
			return "ошибка: некорректный адрес электронной почты"
		}
	}
	return ""
}

// bulkCreate создаёт учётные записи. Пароли хешируются до захвата блокировки записи,
// чтобы длительный импорт не задерживал работу остальных пользователей.
// courseID задан — группы присоединяются к этому курсу (импорт преподавателем).
func (c *Ctx) bulkCreate(rows []importRow, roles, courseID string) (created int, allowed bool) {
	select {
	case c.importSlot <- struct{}{}:
		defer func() { <-c.importSlot }()
	default:
		for i := range rows {
			rows[i].Result = "ошибка: другой импорт ещё выполняется; повторите позже"
		}
		return 0, true
	}
	pwLen := max(c.S().PwMinLen, 14)
	for i := range rows {
		if c.R.Context().Err() != nil {
			return 0, false
		}
		r := &rows[i]
		if r.Result == "" {
			r.Result = validateImportRow(r)
		}
		if r.Result == "" {
			rows[i].pw = secure.GeneratePassword(pwLen)
			rows[i].hash = c.Keys.HashPassword(rows[i].pw)
		}
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.recheck(roles) {
		return 0, false
	}
	var course *Course
	if courseID != "" {
		co, ok := c.Courses.Get(courseID)
		if !ok || !c.canManage(&co) || (c.User.Role == RoleTeacher && !c.S().TeacherImport) {
			return 0, false
		}
		course = &co
	}
	groups := map[string]string{}
	for _, g := range c.Groups.All() {
		groups[strings.ToLower(g.Name)] = g.ID
	}
	taken := map[string]bool{}
	for _, u := range c.Users.All() {
		taken[u.Login] = true
	}
	for i := range rows {
		r := &rows[i]
		if r.Result != "" {
			continue
		}
		if r.Name == "" {
			r.Result = "ошибка: не указано ФИО"
			continue
		}
		r.Login = strings.ToLower(r.Login)
		if r.Login == "" {
			base := loginFromName(r.Name)
			r.Login = base
			for n := 2; taken[r.Login]; n++ {
				suffix := strconv.Itoa(n)
				r.Login = base[:min(len(base), 40-len(suffix))] + suffix
			}
		}
		if taken[r.Login] {
			r.Result = "ошибка: логин уже занят"
			continue
		}
		gid := ""
		if r.Role == RoleStudent && r.Group != "" {
			key := strings.ToLower(r.Group)
			gid = groups[key]
			if gid != "" && c.User.Role == RoleTeacher && (course == nil || !contains(course.GroupIDs, gid)) {
				r.Result = "ошибка: группа «" + r.Group + "» уже существует и не относится к вашим курсам в рамках выбранного курса"
				continue
			}
			if gid == "" {
				gid = secure.NewID()
				if err := c.Groups.Put(gid, Group{ID: gid, Name: r.Group}); err != nil {
					r.Result = "ошибка: не удалось создать группу"
					continue
				}
				groups[key] = gid
			}
			if course != nil && !contains(course.GroupIDs, gid) {
				if err := c.Courses.Update(course.ID, func(x *Course) error {
					x.GroupIDs = append(x.GroupIDs, gid)
					return nil
				}); err != nil {
					r.Result = "ошибка: не удалось добавить группу на курс"
					continue
				}
				course.GroupIDs = append(course.GroupIDs, gid)
			}
		}
		if _, err := c.createUser(r.Login, r.Name, r.Email, r.Role, gid, r.hash); err != nil {
			r.Result = "ошибка: " + err.Error()
			continue
		}
		taken[r.Login] = true
		r.Created, r.Result = true, "создан"
		created++
	}
	return created, true
}

// studentsImport — загрузка списка группы преподавателем из шаблона Excel.
func (c *Ctx) studentsImport() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	back := "/courses/" + co.ID + "?tab=people"
	if c.User.Role == RoleTeacher && !c.S().TeacherImport {
		c.Redirect(back, "!Добавление учащихся преподавателями отключено администратором.")
		return
	}
	group := c.F("group")
	if group == "" || len([]rune(group)) > 60 {
		c.Redirect(back, "!Укажите название группы.")
		return
	}
	sheet, err := c.readUploadXLSX()
	if err != nil {
		c.Redirect(back, "!"+err.Error())
		return
	}
	var rows []importRow
	for i, r := range sheet {
		name := cell(r, 0)
		if strings.TrimSpace(strings.Join(r, "")) == "" || (i == 0 && strings.EqualFold(name, "ФИО")) {
			continue
		}
		row := importRow{Line: i + 1, Name: name, Login: cell(r, 1), Email: cell(r, 2), Role: RoleStudent, Group: group}
		if len(r) > 3 && strings.TrimSpace(strings.Join(r[3:], "")) != "" {
			row.Result = "ошибка: в строке есть столбцы вне шаблона"
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		c.Redirect(back, "!В файле нет ни одной строки с ФИО.")
		return
	}
	if len(rows) > 500 {
		c.Redirect(back, "!За один раз можно загрузить не более 500 учащихся.")
		return
	}
	created, allowed := c.bulkCreate(rows, staff, co.ID)
	if !allowed {
		c.Deny()
		return
	}
	c.Log("импорт группы из Excel", co.Title+" / "+group, created == len(rows), fmt.Sprintf("строк: %d, создано: %d", len(rows), created))
	out := [][]any{{"Строка файла", "ФИО", "Логин", "Временный пароль", "Результат"}}
	for _, r := range rows {
		pw := ""
		if r.Created {
			pw = r.pw
		}
		out = append(out, []any{r.Line, r.Name, r.Login, pw, r.Result})
	}
	c.sendXLSX("Учётные записи — "+group+".xlsx", content.Sheet{Name: "Учётные записи", HeaderRows: 1, Widths: []float64{12, 40, 22, 24, 48}, Rows: out})
}

// ---------- тест из Excel ----------

var reSplit = regexp.MustCompile(`[;,\s]+`)

// questionsFromSheet разбирает лист шаблона «Вопросы теста».
func questionsFromSheet(sheet [][]string) ([]Question, error) {
	var out []Question
	for i, r := range sheet {
		text := cell(r, 0)
		if strings.TrimSpace(strings.Join(r, "")) == "" || (i == 0 && strings.EqualFold(text, "Вопрос")) {
			continue
		}
		if text == "" {
			return nil, fmt.Errorf("строка %d: отсутствует текст вопроса", i+1)
		}
		q := Question{ID: secure.NewID(), Text: text, Points: 1}
		if raw := cell(r, 1); raw != "" {
			points, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
			if err != nil || math.IsNaN(points) || points < 0.1 || points > 100 {
				return nil, fmt.Errorf("строка %d: баллы должны быть числом от 0,1 до 100", i+1)
			}
			q.Points = points
		}
		optionByColumn := map[int]int{}
		for j := 3; j < len(r); j++ {
			if v := cell(r, j); v != "" {
				if j >= 9 {
					return nil, fmt.Errorf("строка %d: шаблон поддерживает до шести вариантов", i+1)
				}
				optionByColumn[j-2] = len(q.Options)
				q.Options = append(q.Options, Option{Text: v})
			}
		}
		right := cell(r, 2)
		if len(q.Options) == 0 {
			q.Type = "text"
			for _, a := range strings.Split(right, ";") {
				if a = strings.TrimSpace(a); a != "" {
					q.Answers = append(q.Answers, a)
				}
			}
		} else {
			correct := 0
			for _, s := range reSplit.Split(right, -1) {
				if s == "" {
					continue
				}
				n, err := strconv.Atoi(s)
				idx, exists := optionByColumn[n]
				if err != nil || !exists {
					return nil, fmt.Errorf("строка %d: правильный ответ «%s» ссылается на пустой или неверный столбец варианта", i+1, s)
				}
				if !q.Options[idx].Correct {
					q.Options[idx].Correct = true
					correct++
				}
			}
			q.Type = "multi"
			if correct == 1 {
				q.Type = "single"
			}
		}
		if err := validateQuestion(&q); err != nil {
			return nil, fmt.Errorf("строка %d: %v", i+1, err)
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		return nil, errors.New("в файле нет вопросов")
	}
	if len(out) > 1000 {
		return nil, errors.New("в одном файле не более 1000 вопросов")
	}
	return out, nil
}

func (c *Ctx) questionImportXLSX() {
	it, _, ok := c.item(true)
	if !ok {
		return
	}
	back := "/items/" + it.ID + "/edit#questions"
	if it.Kind != KindTest {
		c.Redirect(back, "!Вопросы загружаются только в тест.")
		return
	}
	sheet, err := c.readUploadXLSX()
	var qs []Question
	if err == nil {
		qs, err = questionsFromSheet(sheet)
	}
	if err != nil {
		c.Redirect(back, "!Вопросы не загружены: "+err.Error()+".")
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.recheck(staff) {
		c.Deny()
		return
	}
	current, ok := c.Items.Get(it.ID)
	co, exists := c.Courses.Get(it.CourseID)
	if !ok || !exists || current.Kind != KindTest || current.CourseID != co.ID || !c.canManage(&co) {
		c.Deny()
		return
	}
	if err := c.Items.Update(it.ID, func(x *Item) error { x.Questions = append(x.Questions, qs...); return nil }); err != nil {
		c.Redirect(back, "!Не удалось сохранить вопросы.")
		return
	}
	c.Redirect(back, fmt.Sprintf("Загружено вопросов из Excel: %d.", len(qs)))
}

// ---------- выгрузка журнала в Excel ----------

func (c *Ctx) journalXLSX() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	group := c.R.URL.Query().Get("group")
	if group != "" && !contains(co.GroupIDs, group) {
		c.Fail(http.StatusBadRequest, "Эта группа не относится к курсу.")
		return
	}
	g, rows := c.journalRows(co, group)
	names := c.groupNames()
	head := []any{"ФИО", "Группа"}
	widths := []float64{36, 12}
	for _, col := range g.Cols {
		head = append(head, fmt.Sprintf("%s (макс. %s)", col.Title, fmtNum(col.Max)))
		widths = append(widths, 18)
	}
	head = append(head, "Итого", "%", "Оценка", "Посещено", "Занятий")
	widths = append(widths, 10, 8, 9, 10, 9)
	out := [][]any{head}
	for _, r := range rows {
		line := []any{r.User.FullName, names[r.User.GroupID]}
		for _, col := range g.Cols {
			if v, ok := r.Points[col.ID]; ok {
				line = append(line, v)
			} else {
				line = append(line, nil)
			}
		}
		mark, _ := strconv.Atoi(r.Mark)
		line = append(line, r.Total, r.Percent, mark, r.Att.Present, r.Att.Total)
		out = append(out, line)
	}
	c.Log("выгрузка журнала", co.Title, true, "Excel")
	title := "Журнал — " + co.Title
	if n := names[group]; n != "" {
		title += " — " + n
	}
	c.sendXLSX(cleanName(title)+".xlsx", content.Sheet{Name: "Журнал", HeaderRows: 1, Widths: widths, Rows: out})
}

// ---------- документы учащегося (Word) ----------

func (c *Ctx) org() string {
	if c.Cfg.OrgName != "" {
		return c.Cfg.OrgName
	}
	return "____________________________________________"
}

func (c *Ctx) sendDOCX(name string, paras []content.Para) {
	var buf bytes.Buffer
	if err := content.WriteDOCX(&buf, paras); err != nil {
		c.Fail(http.StatusInternalServerError, "Не удалось сформировать документ.")
		return
	}
	c.sendFile(name, mimeDOCX, buf.Bytes())
}

const blank = "______________________________________________________________"

func (c *Ctx) groupOf(u *User) string {
	g, _ := c.Groups.Get(u.GroupID)
	return g.Name
}

func (c *Ctx) docAnketa() {
	u := c.User
	p := []content.Para{
		{Text: c.org(), Align: "center"},
		{Text: "АНКЕТА СЛУШАТЕЛЯ", Bold: true, Align: "center", Size: 14, Before: 12},
		{Text: "Заполните анкету и отправьте её преподавателю по электронной почте.", Align: "center", Size: 11},
		{Text: "1. Фамилия, имя, отчество: " + u.FullName, Before: 12},
		{Text: "2. Учебная группа: " + c.groupOf(u)},
		{Text: "3. Имя пользователя (логин) на портале: " + u.Login},
		{Text: "4. Адрес электронной почты: " + orBlank(u.Email)},
		{Text: "5. Опыт по тематике обучения (по желанию):\n" + blank + "\n" + blank},
		{Text: "6. Что вы ожидаете от обучения (по желанию):\n" + blank + "\n" + blank},
		{Text: "Достоверность указанных сведений подтверждаю.", Before: 18},
		{Text: "Дата: «____» __________ 20___ г.      Подпись: ____________ / ________________ /", Before: 12},
	}
	c.sendDOCX("Анкета слушателя.docx", p)
}

func orBlank(s string) string {
	if s == "" {
		return "______________________________"
	}
	return s
}

func (c *Ctx) docSoglasie() {
	u := c.User
	org := c.org()
	p := []content.Para{
		{Text: "СОГЛАСИЕ", Bold: true, Align: "center", Size: 14},
		{Text: "на обработку персональных данных", Bold: true, Align: "center"},
		{Text: "Я, " + u.FullName + ",", Before: 12, Indent: true},
		{Text: "Поля документа и адреса заполняйте по утверждённому порядку оператора. Передавайте подписанный бланк по согласованному защищённому каналу.", Size: 10},
		{Text: "Документ, удостоверяющий личность: серия ________ № ______________, выдан " + blank + ",\nдата выдачи ____ . ____ . ________, зарегистрирован(а) по адресу: " + blank + ","},
		{Text: "в соответствии со статьёй 9 Федерального закона от 27.07.2006 № 152-ФЗ «О персональных данных» свободно, своей волей и в своём интересе даю согласие оператору — " + org +
			" (далее — Оператор), адрес: " + orBlank(c.Cfg.PersonalDataOperatorAddress) + ", на обработку моих персональных данных.", Align: "both", Indent: true},
		{Text: "Цель обработки: организация и сопровождение моего обучения, учёт результатов обучения, оформление и выдача документов об обучении.", Align: "both", Indent: true},
		{Text: "Перечень персональных данных: фамилия, имя, отчество; адрес электронной почты; сведения из анкеты; данные, указанные в настоящем согласии для идентификации; " +
			"учётные данные на учебном портале; сведения о ходе и результатах обучения (изученные материалы, результаты тестов, оценки, посещаемость, выполненные работы).", Align: "both", Indent: true},
		{Text: "Перечень действий: сбор, запись, систематизация, накопление, хранение, уточнение (обновление, изменение), извлечение, использование, передача (предоставление, доступ) преподавателям " +
			"и уполномоченным работникам Оператора, блокирование, удаление, уничтожение. Обработка осуществляется с использованием средств автоматизации и без них.", Align: "both", Indent: true},
		{Text: "Лицо, обрабатывающее данные по поручению оператора (если назначено), его наименование и адрес: " + blank, Align: "both", Indent: true},
		{Text: "Согласие действует со дня его подписания до достижения цели обработки либо до дня его отзыва. Согласие может быть отозвано мной путём направления Оператору письменного заявления.", Align: "both", Indent: true},
		{Text: "Дата: «____» __________ 20___ г.      Подпись: ____________ / ________________ /", Before: 24},
	}
	c.sendDOCX("Согласие на обработку персональных данных.docx", p)
}

// completed сообщает, прошёл ли учащийся все опубликованные элементы курса.
func (a *App) completed(userID, courseID string) bool {
	items := a.courseItems(courseID, true)
	if len(items) == 0 {
		return false
	}
	var byItem = make(map[string][]Attempt)
	for _, at := range a.Attempts.Find(func(at *Attempt) bool {
		return at.UserID == userID && at.CourseID == courseID && at.Done
	}) {
		byItem[at.ItemID] = append(byItem[at.ItemID], at)
	}
	for _, it := range items {
		passed := false
		switch it.Kind {
		case KindTest:
			for _, at := range byItem[it.ID] {
				if at.passed(&it) {
					passed = true
					break
				}
			}
		case KindTask:
			sub, ok := a.Subs.Get(it.ID + ":" + userID)
			passed = ok && sub.Graded && sub.CourseID == courseID
		case KindLesson, KindFile:
			_, passed = a.Progress.Get(userID + ":" + it.ID)
		}
		if !passed {
			return false
		}
	}
	return true
}

func (c *Ctx) teacherNames(co *Course) string {
	var names []string
	for _, id := range co.TeacherIDs {
		if u, ok := c.Users.Get(id); ok {
			names = append(names, u.FullName)
		}
	}
	return strings.Join(names, ", ")
}

func (c *Ctx) docZayavlenie() {
	co, ok := c.course(c.R.PathValue("id"), false)
	if !ok {
		return
	}
	if !c.completed(c.User.ID, co.ID) {
		c.Redirect("/courses/"+co.ID, "!Заявление доступно после прохождения всех разделов курса.")
		return
	}
	u := c.User
	teacher := c.teacherNames(co)
	if teacher == "" {
		teacher = "______________________________"
	}
	p := []content.Para{
		{Text: "Руководителю\n" + c.org() + "\nот слушателя группы " + orBlank(c.groupOf(u)) + "\n" + u.FullName, Align: "right"},
		{Text: "ЗАЯВЛЕНИЕ", Bold: true, Align: "center", Size: 14, Before: 24},
		{Text: "Прошу выдать мне сертификат о прохождении обучения по курсу «" + co.Title + "».", Align: "both", Indent: true, Before: 12},
		{Text: "Обучение пройдено в полном объёме: все разделы курса изучены, контрольные задания выполнены. Преподаватель: " + teacher + ".", Align: "both", Indent: true},
		{Text: "Сертификат прошу (нужное подчеркнуть): выдать лично / направить по электронной почте " + orBlank(u.Email) + ".", Align: "both", Indent: true},
		{Text: "Дата: " + time.Now().In(c.Loc).Format("02.01.2006") + "      Подпись: ____________ / " + u.FullName + " /", Before: 24},
	}
	c.sendDOCX("Заявление на выдачу сертификата.docx", p)
}

// ---------- сертификат (PDF) ----------

var fontCandidates = []string{
	"/usr/share/fonts/truetype/paratype/PTS55F.ttf", "/usr/share/fonts/paratype-pt-sans/PTS55F.ttf", "/usr/share/fonts/ttf/PT/PTS55F.ttf",
	"/usr/share/fonts/truetype/pt-sans/PTS55F.ttf", "/usr/share/fonts/pt-sans/PTS55F.ttf",
	"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf", "/usr/share/fonts/truetype/liberation2/LiberationSans-Regular.ttf",
	"/usr/share/fonts/liberation-sans/LiberationSans-Regular.ttf", "/usr/share/fonts/liberation/LiberationSans-Regular.ttf",
	"/usr/share/fonts/ttf/liberation/LiberationSans-Regular.ttf", "/usr/share/fonts/liberation-fonts/LiberationSans-Regular.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/dejavu/DejaVuSans.ttf",
	"/usr/share/fonts/dejavu-sans-fonts/DejaVuSans.ttf", "/usr/share/fonts/ttf/dejavu/DejaVuSans.ttf",
}

// certFont возвращает шрифт для сертификатов: из конфигурации (certificate_font) либо
// первый найденный в системе шрифт с кириллицей.
func (a *App) certFont() ([]byte, string, error) {
	try := func(p string) ([]byte, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, (20<<20)+1))
		if err != nil {
			return nil, err
		}
		if len(b) > 20<<20 {
			return nil, errors.New("файл шрифта слишком велик")
		}
		_, err = content.NewPDF(1, 1, b)
		return b, err
	}
	if p := a.Cfg.abs(a.Cfg.CertificateFont); p != "" {
		b, err := try(p)
		if err != nil {
			return nil, p, fmt.Errorf("шрифт сертификата %s (certificate_font): %w", p, err)
		}
		return b, p, nil
	}
	for _, p := range fontCandidates {
		if b, err := try(p); err == nil {
			return b, p, nil
		}
	}
	// другие каталоги: первый подходящий шрифт без наклона и жирности
	var found []byte
	var where string
	filepath.WalkDir("/usr/share/fonts", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".ttf") {
			return nil
		}
		low := strings.ToLower(filepath.Base(p))
		if strings.Contains(low, "bold") || strings.Contains(low, "italic") || strings.Contains(low, "oblique") || strings.Contains(low, "mono") {
			return nil
		}
		if b, err := try(p); err == nil {
			found, where = b, p
			return filepath.SkipAll
		}
		return nil
	})
	if found != nil {
		return found, where, nil
	}
	return nil, "", errors.New("в системе не найден шрифт TrueType с кириллицей: установите пакет шрифтов (PT Sans, Liberation, DejaVu) либо укажите файл в параметре certificate_font")
}

// certIcon возвращает значок сертификата: certificate_icon, иначе растровый логотип портала.
func (a *App) certIcon() (image.Image, error) {
	explicit := a.Cfg.CertificateIcon != ""
	for _, p := range []string{a.Cfg.abs(a.Cfg.CertificateIcon), a.Cfg.abs(a.Cfg.LogoFile)} {
		if p == "" {
			continue
		}
		img, err := readCertificateIcon(p)
		if err == nil {
			return img, nil
		}
		if explicit {
			return nil, fmt.Errorf("значок сертификата (certificate_icon): %w", err)
		}
	}
	return nil, nil
}

func readCertificateIcon(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return nil, errors.New("не удалось прочитать значок размером до 8 МиБ")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 ||
		int64(cfg.Width)*int64(cfg.Height) > 4_000_000 {
		return nil, errors.New("ожидается PNG или JPEG размером до 4 мегапикселей")
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}

// emblem рисует встроенный значок (тот же, что в заголовке портала) векторными операторами PDF.
func emblem(p *content.PDF, x, y, size float64) {
	s := size / 96
	p.Raw(fmt.Sprintf("q %.4f 0 0 %.4f %.2f %.2f cm", s, -s, x, y+size) +
		" 0.882 0.925 0.969 rg 0.263 0.329 0.431 RG 4 w 1 j 48 4 m 86 16 l 86 46 l 86 68 71 83 48 92 c 25 83 10 68 10 46 c 10 16 l h B" +
		" 0.337 0.416 0.533 rg 48 28 m 24 39 l 48 50 l 72 39 l h f" +
		" 0.263 0.329 0.431 rg 33 46 m 33 57 l 33 61 40 65 48 65 c 56 65 63 61 63 57 c 63 46 l 48 53 l h f" +
		" 0.914 0.4 0.275 RG 4 w 1 J 72 39 m 72 56 l S" +
		" 0.914 0.4 0.275 rg 76 60 m 76 62.21 74.21 64 72 64 c 69.79 64 68 62.21 68 60 c 68 57.79 69.79 56 72 56 c 74.21 56 76 57.79 76 60 c f Q")
}

// certificatePDF формирует сертификат (альбомный лист A4).
func (a *App) certificatePDF(ct *Certificate) ([]byte, error) {
	font, _, err := a.certFont()
	if err != nil {
		return nil, err
	}
	const W, H = 841.89, 595.28
	p, err := content.NewPDF(W, H, font)
	if err != nil {
		return nil, err
	}
	slate, ink, accent, muted := content.Hex(0x566a88), content.Hex(0x202834), content.Hex(0x6198e7), content.Hex(0x6b7a8f)
	cx := W / 2
	p.Rect(0, H-14, W, 14, 0, slate, true)
	p.Rect(0, 0, W, 14, 0, slate, true)
	p.Rect(28, 28, W-56, H-56, 1.6, slate, false)
	p.Rect(34, 34, W-68, H-68, 0.6, content.Hex(0x93b8ed), false)
	img, err := a.certIcon()
	if err != nil {
		return nil, err
	}
	if img != nil {
		p.Image(img, cx-90, H-132, 180, 76)
	} else {
		emblem(p, cx-38, H-132, 76)
	}
	orgLines, orgSize, err := fitCertificateText(p, ct.OrgName, 13, 8, W-160, 3)
	if err != nil {
		return nil, fmt.Errorf("название организации: %w", err)
	}
	nameLines, nameSize, err := fitCertificateText(p, ct.StudentName, 26, 12, W-160, 2)
	if err != nil {
		return nil, fmt.Errorf("ФИО учащегося: %w", err)
	}
	courseLines, courseSize, err := fitCertificateText(p, "«"+ct.CourseTitle+"»", 18, 9, W-160, 4)
	if err != nil {
		return nil, fmt.Errorf("название курса: %w", err)
	}
	for i, line := range orgLines {
		p.Text(cx, H-153-float64(i)*14, orgSize, line, 0, slate, false)
	}
	p.Text(cx, 365, 40, "СЕРТИФИКАТ", 0, ink, true)
	p.Text(cx, 340, 15, "о прохождении обучения", 0, slate, false)
	p.Text(cx, 313, 12, "Настоящий сертификат подтверждает, что", 0, muted, false)
	for i, line := range nameLines {
		p.Text(cx, 281-float64(i)*28, nameSize, line, 0, ink, true)
	}
	p.Line(cx-230, 239, cx+230, 239, 0.8, accent)
	p.Text(cx, 221, 12, "успешно прошёл(ла) обучение по курсу", 0, muted, false)
	ty := 194.0
	for _, line := range courseLines {
		p.Text(cx, ty, courseSize, line, 0, ink, true)
		ty -= courseSize + 3
	}
	if ct.Percent > 0 {
		p.Text(cx, ty-2, 11, "Итоговый результат: "+fmtNum(ct.Percent)+" %", 0, muted, false)
	}
	// нижний блок: преподаватель слева, дата и номер справа
	by := 86.0
	p.Line(70, by+14, 330, by+14, 0.6, muted)
	teacher := ct.Teachers
	if teacher == "" {
		teacher = "—"
	}
	tl, teacherSize, err := fitCertificateText(p, "Преподаватель: "+teacher, 11, 8, 460, 3)
	if err != nil {
		return nil, fmt.Errorf("ФИО преподавателя: %w", err)
	}
	for i, line := range tl {
		p.Text(70, by-float64(i)*12, teacherSize, line, -1, ink, false)
	}
	p.Text(200, by+19, 8, "подпись", 0, muted, false)
	p.Text(W-70, by+2, 11, "Дата выдачи: "+ct.Issued.In(a.Loc).Format("02.01.2006"), 1, ink, false)
	p.Text(W-70, by-12, 11, "№ "+ct.Number, 1, ink, false)
	return p.Bytes("Сертификат " + ct.Number), nil
}

func fitCertificateText(p *content.PDF, value string, size, minSize, width float64, maxLines int) ([]string, float64, error) {
	for ; size >= minSize; size-- {
		lines := p.Wrap(value, size, width)
		if len(lines) <= maxLines {
			return lines, size, nil
		}
	}
	return nil, 0, errors.New("текст слишком длинный для бланка")
}

// certificateIssue оформляет сертификат учащемуся, завершившему курс.
func (c *Ctx) certificateIssue() {
	co, ok := c.course(c.R.PathValue("id"), false)
	if !ok {
		return
	}
	back := "/courses/" + co.ID
	if !c.completed(c.User.ID, co.ID) {
		c.Redirect(back, "!Сертификат доступен после прохождения всех разделов курса.")
		return
	}
	if old, ok := c.Certs.First(func(x *Certificate) bool { return x.UserID == c.User.ID && x.CourseID == co.ID }); ok {
		c.Redirect("/certificates/"+old.ID, "")
		return
	}
	org, teachers := strings.TrimSpace(c.Cfg.OrgName), c.teacherNames(co)
	if org == "" || teachers == "" {
		c.Redirect(back, "!Для выдачи сертификата укажите организацию в настройках сервера и назначьте преподавателя курса.")
		return
	}
	now := time.Now()
	id := secure.NewID()
	number := fmt.Sprintf("К-%d-%s", now.In(c.Loc).Year(), strings.ToUpper(id))
	g := c.gradebook(co.ID)
	ct := Certificate{ID: id, Number: number, UserID: c.User.ID, CourseID: co.ID,
		StudentName: c.User.FullName, CourseTitle: co.Title, OrgName: org, Teachers: teachers,
		Percent: percent(g.total(c.User.ID), g.Max), Issued: now}
	pdf, err := c.certificatePDF(&ct)
	if err != nil {
		c.Log("выдача сертификата", co.Title, false, err.Error())
		c.Redirect(back, "!Не удалось сформировать сертификат. Сообщите администратору.")
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.recheck(RoleStudent) {
		c.Deny()
		return
	}
	current, exists := c.Courses.Get(co.ID)
	if !exists || !c.canView(&current) || !c.completed(c.User.ID, co.ID) {
		c.Deny()
		return
	}
	if old, ok := c.Certs.First(func(x *Certificate) bool { return x.UserID == c.User.ID && x.CourseID == co.ID }); ok {
		c.Redirect("/certificates/"+old.ID, "")
		return
	}
	g = c.gradebook(co.ID)
	if ct.UserID != c.User.ID || ct.StudentName != c.User.FullName || ct.CourseTitle != current.Title ||
		ct.Teachers != c.teacherNames(&current) || ct.Percent != percent(g.total(c.User.ID), g.Max) {
		c.Redirect(back, "!Данные курса изменились. Повторите запрос сертификата.")
		return
	}
	pdfHash := sha256.Sum256(pdf)
	ct.PDFHash = hex.EncodeToString(pdfHash[:])
	m, err := c.storeCertificatePDF(&ct, pdf)
	if err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить PDF сертификата.")
		return
	}
	ct.PDFFileID = m.ID
	if err := c.Certs.Put(ct.ID, ct); err != nil {
		c.removeFile(m.ID)
		c.Fail(http.StatusServiceUnavailable, "Не удалось оформить сертификат.")
		return
	}
	c.Log("выдача сертификата", co.Title, true, "№ "+ct.Number)
	c.Redirect("/certificates/"+ct.ID, "")
}

func (c *Ctx) certificateGet() {
	ct, ok := c.Certs.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Сертификат не найден.")
		return
	}
	allowed := ct.UserID == c.User.ID || c.User.Role == RoleAdmin
	if co, ok := c.Courses.Get(ct.CourseID); ok && !allowed {
		allowed = c.canManage(&co)
	}
	if !allowed {
		c.Deny()
		return
	}
	var pdf []byte
	var err error
	if ct.PDFFileID == "" {
		pdf, err = c.certificatePDF(&ct) // сертификаты, выданные до сохранения PDF
	} else {
		var buf bytes.Buffer
		m, ok := c.Files.Get(ct.PDFFileID)
		if !ok || m.Scope != "certificate" || m.OwnerID != ct.UserID || m.SHA256 != ct.PDFHash {
			err = errors.New("не найдена запись PDF")
		} else {
			f, openErr := os.Open(c.filePath(m.ID))
			err = openErr
			if err == nil {
				err = copyVerifiedFile(&buf, f, &m)
				f.Close()
				pdf = buf.Bytes()
			}
		}
	}
	if err != nil {
		c.Log("открытие сертификата", ct.Number, false, err.Error())
		c.Fail(http.StatusServiceUnavailable, "Не удалось открыть сертификат. Сообщите администратору.")
		return
	}
	h := c.W.Header()
	h.Del("Content-Security-Policy")
	h.Set("Content-Type", "application/pdf")
	h.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape("Сертификат "+ct.Number+".pdf"))
	c.W.Write(pdf)
}

func (a *App) storeCertificatePDF(ct *Certificate, pdf []byte) (FileMeta, error) {
	return a.writeFile(bytes.NewReader(pdf), FileMeta{
		ID: secure.NewID(), Name: "Сертификат " + ct.Number + ".pdf", Ext: "pdf",
		OwnerID: ct.UserID, CourseID: ct.CourseID, Scope: "certificate", Created: ct.Issued,
	})
}
