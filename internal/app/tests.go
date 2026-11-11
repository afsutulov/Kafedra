package app

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kafedra/internal/secure"
)

// ---------- вопросы ----------

func validateQuestion(q *Question) error {
	q.Text = strings.TrimSpace(q.Text)
	if q.Text == "" {
		return errors.New("пустой текст вопроса")
	}
	if q.Points <= 0 {
		q.Points = 1
	}
	correct := 0
	for _, o := range q.Options {
		if o.Correct {
			correct++
		}
	}
	switch q.Type {
	case "single":
		if len(q.Options) < 2 || correct != 1 {
			return errors.New("нужно не менее двух вариантов и ровно один правильный")
		}
	case "multi":
		if len(q.Options) < 2 || correct < 1 {
			return errors.New("нужно не менее двух вариантов и хотя бы один правильный")
		}
	case "text":
		if len(q.Answers) == 0 {
			return errors.New("укажите хотя бы один допустимый ответ")
		}
		q.Options = nil
	default:
		return errors.New("неизвестный тип вопроса")
	}
	return nil
}

// ParseQuestions разбирает банк вопросов в текстовом формате:
//
//	? Текст вопроса
//	+ правильный вариант
//	- неправильный вариант
//	= допустимый ответ (для вопроса с вводом)
//	* 2            (баллы за вопрос, необязательно)
//
// Вопросы разделяются пустой строкой. Тип определяется автоматически.
func ParseQuestions(src string) ([]Question, error) {
	var out []Question
	var cur *Question
	flush := func(line int) error {
		if cur == nil {
			return nil
		}
		correct := 0
		for _, o := range cur.Options {
			if o.Correct {
				correct++
			}
		}
		switch {
		case len(cur.Answers) > 0 && len(cur.Options) == 0:
			cur.Type = "text"
		case correct == 1:
			cur.Type = "single"
		default:
			cur.Type = "multi"
		}
		if err := validateQuestion(cur); err != nil {
			return fmt.Errorf("вопрос №%d (перед строкой %d): %v", len(out)+1, line, err)
		}
		cur.ID = secure.NewID()
		out = append(out, *cur)
		cur = nil
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			if err := flush(i + 1); err != nil {
				return nil, err
			}
			continue
		}
		mark, rest := line[0], strings.TrimSpace(line[1:])
		if mark == '?' {
			if err := flush(i + 1); err != nil {
				return nil, err
			}
			cur = &Question{Text: rest, Points: 1}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("строка %d: вопрос должен начинаться со знака «?»", i+1)
		}
		switch mark {
		case '+', '-':
			cur.Options = append(cur.Options, Option{Text: rest, Correct: mark == '+'})
		case '=':
			cur.Answers = append(cur.Answers, rest)
		case '*':
			cur.Points = atof(rest, 1, 0.1, 100)
		default: // продолжение текста вопроса (например, строки кода)
			if len(cur.Options) == 0 && len(cur.Answers) == 0 {
				cur.Text += "\n" + strings.TrimRight(raw, " ")
			} else {
				return nil, fmt.Errorf("строка %d: ожидается «+», «-», «=» или «?»", i+1)
			}
		}
	}
	if err := flush(len(lines)); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("вопросы не найдены")
	}
	return out, nil
}

func (c *Ctx) questionSave() {
	it, _, ok := c.item(true)
	if !ok || it.Kind != KindTest {
		return
	}
	back := "/items/" + it.ID + "/edit#questions"
	q := Question{ID: c.F("qid"), Type: c.F("type"), Text: c.F("text"), Points: atof(c.F("points"), 1, 0.1, 100)}
	for i := 0; i < 8; i++ {
		if t := c.F("opt_text_" + strconv.Itoa(i)); t != "" {
			q.Options = append(q.Options, Option{Text: t, Correct: c.F("opt_ok_"+strconv.Itoa(i)) == "1"})
		}
	}
	for _, a := range strings.Split(c.R.FormValue("answers"), "\n") {
		if a = strings.TrimSpace(a); a != "" {
			q.Answers = append(q.Answers, a)
		}
	}
	if q.Type != "text" {
		q.Answers = nil
	}
	if err := validateQuestion(&q); err != nil {
		c.Redirect(back, "!Вопрос не сохранён: "+err.Error()+".")
		return
	}
	c.Items.Update(it.ID, func(x *Item) error {
		for i := range x.Questions {
			if x.Questions[i].ID == q.ID {
				x.Questions[i] = q
				return nil
			}
		}
		q.ID = secure.NewID()
		x.Questions = append(x.Questions, q)
		return nil
	})
	c.Redirect(back, "Вопрос сохранён.")
}

func (c *Ctx) questionDelete() {
	it, _, ok := c.item(true)
	if !ok {
		return
	}
	qid := c.F("qid")
	c.Items.Update(it.ID, func(x *Item) error {
		for i := range x.Questions {
			if x.Questions[i].ID == qid {
				x.Questions = append(x.Questions[:i], x.Questions[i+1:]...)
				break
			}
		}
		if len(x.Questions) == 0 {
			x.Visible = false
		}
		return nil
	})
	c.Redirect("/items/"+it.ID+"/edit#questions", "Вопрос удалён.")
}

func (c *Ctx) questionImport() {
	it, _, ok := c.item(true)
	if !ok || it.Kind != KindTest {
		return
	}
	back := "/items/" + it.ID + "/edit#questions"
	qs, err := ParseQuestions(c.R.FormValue("bulk"))
	if err != nil {
		c.Redirect(back, "!Импорт не выполнен: "+err.Error()+".")
		return
	}
	c.Items.Update(it.ID, func(x *Item) error { x.Questions = append(x.Questions, qs...); return nil })
	c.Redirect(back, fmt.Sprintf("Добавлено вопросов: %d.", len(qs)))
}

// ---------- прохождение теста ----------

func norm(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(s, "ё", "е"))), " ")
}

// scoreQuestion возвращает баллы за ответ на вопрос.
func scoreQuestion(q *Question, a Answer) float64 {
	switch q.Type {
	case "text":
		for _, ok := range q.Answers {
			if norm(a.Text) != "" && norm(a.Text) == norm(ok) {
				return q.Points
			}
		}
	case "single":
		if len(a.Opts) == 1 && a.Opts[0] >= 0 && a.Opts[0] < len(q.Options) && q.Options[a.Opts[0]].Correct {
			return q.Points
		}
	case "multi":
		total, good, bad := 0, 0, 0
		for _, o := range q.Options {
			if o.Correct {
				total++
			}
		}
		seen := map[int]bool{}
		for _, i := range a.Opts {
			if i < 0 || i >= len(q.Options) || seen[i] {
				continue
			}
			seen[i] = true
			if q.Options[i].Correct {
				good++
			} else {
				bad++
			}
		}
		if total > 0 && good > bad { // частичный зачёт со штрафом за неверные варианты
			return q.Points * float64(good-bad) / float64(total)
		}
	}
	return 0
}

// finishAttempt завершает попытку и вычисляет результат.
func (a *App) finishAttempt(id string) {
	cur, ok := a.Attempts.Get(id)
	if !ok {
		return
	}
	it, _ := a.Items.Get(cur.ItemID) // до Update: внутри него обращаться к хранилищу нельзя
	a.Attempts.Update(id, func(x *Attempt) error {
		if x.Done {
			return nil
		}
		x.Score, x.MaxScore = 0, 0
		for i := range x.Questions {
			q := &x.Questions[i]
			x.MaxScore += q.Points
			x.Score += scoreQuestion(q, x.Answers[q.ID])
		}
		if x.MaxScore > 0 {
			x.Points = float64(int(x.Score/x.MaxScore*it.MaxPoints*100+0.5)) / 100
		}
		x.Done, x.Finished = true, time.Now()
		if !x.Deadline.IsZero() && x.Finished.After(x.Deadline) {
			x.Finished = x.Deadline
		}
		return nil
	})
}

func (a *App) attemptsOf(itemID, userID string) []Attempt {
	return sortBy(a.Attempts.Find(func(x *Attempt) bool { return x.ItemID == itemID && x.UserID == userID }),
		func(x, y *Attempt) bool { return x.Started.Before(y.Started) })
}

func testOpen(it *Item, now time.Time) (bool, string) {
	if !it.OpenAt.IsZero() && now.Before(it.OpenAt) {
		return false, "Тест ещё не открыт."
	}
	if !it.CloseAt.IsZero() && now.After(it.CloseAt) {
		return false, "Срок прохождения теста истёк."
	}
	return true, ""
}

func (c *Ctx) testView(it *Item, co *Course, data map[string]any) {
	if c.canManage(co) {
		data["AttemptCount"] = c.Attempts.Count(func(a *Attempt) bool { return a.ItemID == it.ID && a.Done })
		c.Render("test", data)
		return
	}
	now := time.Now()
	atts := c.attemptsOf(it.ID, c.User.ID)
	best := -1.0
	for _, a := range atts {
		if !a.Done {
			if !a.Deadline.IsZero() && now.After(a.Deadline) {
				c.finishAttempt(a.ID)
				c.Redirect("/items/"+it.ID, "")
				return
			}
			data["Active"] = a
		} else if a.Points > best {
			best = a.Points
		}
	}
	open, why := testOpen(it, now)
	left := it.MaxAttempts - len(atts)
	if it.MaxAttempts > 0 && left <= 0 {
		open, why = false, "Все попытки использованы."
	}
	data["Attempts"], data["Open"], data["Why"], data["Left"] = atts, open, why, left
	data["Best"], data["HasBest"] = best, best >= 0
	n := len(it.Questions)
	if it.PoolSize > 0 && it.PoolSize < n {
		n = it.PoolSize
	}
	data["QCount"] = n
	c.Render("test", data)
}

func (c *Ctx) attemptStart() {
	it, co, ok := c.item(false)
	if !ok || it.Kind != KindTest {
		return
	}
	back := "/items/" + it.ID
	now := time.Now()
	atts := c.attemptsOf(it.ID, c.User.ID)
	for _, a := range atts {
		if !a.Done {
			c.Redirect("/attempts/"+a.ID, "")
			return
		}
	}
	if open, why := testOpen(it, now); !open {
		c.Redirect(back, "!"+why)
		return
	}
	if it.MaxAttempts > 0 && len(atts) >= it.MaxAttempts {
		c.Redirect(back, "!Все попытки использованы.")
		return
	}
	if len(it.Questions) == 0 {
		c.Redirect(back, "!В тесте нет вопросов.")
		return
	}
	qs := append([]Question(nil), it.Questions...)
	if it.Shuffle || (it.PoolSize > 0 && it.PoolSize < len(qs)) {
		rand.Shuffle(len(qs), func(i, j int) { qs[i], qs[j] = qs[j], qs[i] })
	}
	if it.PoolSize > 0 && it.PoolSize < len(qs) {
		qs = qs[:it.PoolSize]
	}
	if it.Shuffle {
		for i := range qs {
			o := append([]Option(nil), qs[i].Options...)
			rand.Shuffle(len(o), func(a, b int) { o[a], o[b] = o[b], o[a] })
			qs[i].Options = o
		}
	}
	at := Attempt{ID: secure.NewID(), ItemID: it.ID, CourseID: co.ID, UserID: c.User.ID, Started: now, Questions: qs,
		Answers: map[string]Answer{}, IP: c.IP}
	if it.TimeLimit > 0 {
		at.Deadline = now.Add(time.Duration(it.TimeLimit) * time.Minute)
	}
	if !it.CloseAt.IsZero() && (at.Deadline.IsZero() || it.CloseAt.Before(at.Deadline)) {
		at.Deadline = it.CloseAt
	}
	c.Attempts.Put(at.ID, at)
	c.Log("начало тестирования", co.Title+" / "+it.Title, true, fmt.Sprintf("попытка %d", len(atts)+1))
	c.Redirect("/attempts/"+at.ID, "")
}

func (c *Ctx) readAnswers(at *Attempt) map[string]Answer {
	out := map[string]Answer{}
	for _, q := range at.Questions {
		var a Answer
		if q.Type == "text" {
			a.Text = c.F("q_" + q.ID)
			if len(a.Text) > 500 {
				a.Text = a.Text[:500]
			}
		} else {
			for _, v := range c.R.Form["q_"+q.ID] {
				if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(q.Options) {
					a.Opts = append(a.Opts, i)
				}
			}
		}
		out[q.ID] = a
	}
	return out
}

// ownAttempt загружает незавершённую попытку текущего учащегося.
func (c *Ctx) ownAttempt() (*Attempt, bool) {
	at, ok := c.Attempts.Get(c.R.PathValue("id"))
	if !ok || at.UserID != c.User.ID {
		c.Deny()
		return nil, false
	}
	return &at, true
}

func (c *Ctx) storeAnswers(at *Attempt) bool {
	// ответы принимаются до срока (с допуском 60 с на доставку запроса)
	if at.Done || (!at.Deadline.IsZero() && time.Now().After(at.Deadline.Add(time.Minute))) {
		return false
	}
	ans := c.readAnswers(at)
	return c.Attempts.Update(at.ID, func(x *Attempt) error {
		if x.Done {
			return errors.New("завершена")
		}
		x.Answers = ans
		return nil
	}) == nil
}

func (c *Ctx) attemptSave() {
	at, ok := c.ownAttempt()
	if !ok {
		return
	}
	if !c.storeAnswers(at) {
		c.W.WriteHeader(http.StatusConflict)
		return
	}
	c.W.WriteHeader(http.StatusNoContent)
}

func (c *Ctx) attemptSubmit() {
	at, ok := c.ownAttempt()
	if !ok {
		return
	}
	late := !c.storeAnswers(at) && !at.Done
	c.finishAttempt(at.ID)
	done, _ := c.Attempts.Get(at.ID)
	c.Log("завершение тестирования", at.ItemID, true, fmt.Sprintf("результат %s из %s", fmtNum(done.Score), fmtNum(done.MaxScore)))
	msg := "Тест завершён."
	if late {
		msg = "!Время истекло: засчитаны ответы, сохранённые до окончания срока."
	}
	c.Redirect("/attempts/"+at.ID, msg)
}

type reviewRow struct {
	Q       Question
	A       Answer
	Got     float64
	Correct bool
}

func (c *Ctx) attemptView() {
	at, ok := c.Attempts.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Попытка не найдена.")
		return
	}
	co, cok := c.Courses.Get(at.CourseID)
	it, iok := c.Items.Get(at.ItemID)
	if !cok || !iok {
		c.Fail(http.StatusNotFound, "Тест удалён.")
		return
	}
	manage := c.canManage(&co)
	if at.UserID != c.User.ID && !manage {
		c.Deny()
		return
	}
	if !at.Done && !at.Deadline.IsZero() && time.Now().After(at.Deadline) {
		c.finishAttempt(at.ID)
		at, _ = c.Attempts.Get(at.ID)
	}
	data := map[string]any{"At": at, "Item": it, "Course": co, "Manage": manage, "Student": c.userName(at.UserID)}
	if !at.Done {
		if manage {
			c.Fail(http.StatusConflict, "Попытка ещё не завершена.")
			return
		}
		if !at.Deadline.IsZero() {
			data["Remaining"] = int(time.Until(at.Deadline).Seconds())
		}
		c.Render("attempt", data)
		return
	}
	if manage || it.ShowAnswers {
		var rows []reviewRow
		for _, q := range at.Questions {
			got := scoreQuestion(&q, at.Answers[q.ID])
			rows = append(rows, reviewRow{q, at.Answers[q.ID], got, got >= q.Points})
		}
		data["Review"] = rows
	}
	data["Passed"] = at.MaxScore > 0 && at.Score/at.MaxScore*100 >= float64(it.PassPercent)
	c.Render("attempt_result", data)
}

func (c *Ctx) attemptAnnul() {
	at, ok := c.Attempts.Get(c.R.PathValue("id"))
	if !ok {
		c.Redirect("/", "")
		return
	}
	co, ok := c.course(at.CourseID, true)
	if !ok {
		return
	}
	c.Attempts.Delete(at.ID)
	c.Log("аннулирование попытки", co.Title, true, "учащийся: "+c.userName(at.UserID))
	c.notify(at.UserID, "Преподаватель аннулировал вашу попытку теста — можно пройти его заново.", "/items/"+at.ItemID)
	c.Redirect("/items/"+at.ItemID+"/results", "Попытка аннулирована.")
}

type resultRow struct {
	User     User
	Attempts []Attempt
	Best     float64
	HasBest  bool
}

type qStat struct {
	Q       Question
	Shown   int
	Correct int
}

func (c *Ctx) testResults() {
	it, co, ok := c.item(true)
	if !ok {
		return
	}
	byUser := map[string][]Attempt{}
	stats := map[string]*qStat{}
	var order []string
	for _, q := range it.Questions {
		stats[q.ID] = &qStat{Q: q}
		order = append(order, q.ID)
	}
	for _, a := range c.Attempts.Find(func(a *Attempt) bool { return a.ItemID == it.ID }) {
		byUser[a.UserID] = append(byUser[a.UserID], a)
		if !a.Done {
			continue
		}
		for _, q := range a.Questions {
			if s := stats[q.ID]; s != nil {
				s.Shown++
				if scoreQuestion(&q, a.Answers[q.ID]) >= q.Points {
					s.Correct++
				}
			}
		}
	}
	var rows []resultRow
	sum, n := 0.0, 0
	for _, u := range c.students(co, "") {
		r := resultRow{User: u, Attempts: sortBy(byUser[u.ID], func(x, y *Attempt) bool { return x.Started.Before(y.Started) })}
		for _, a := range r.Attempts {
			if a.Done && (!r.HasBest || a.Points > r.Best) {
				r.Best, r.HasBest = a.Points, true
			}
		}
		if r.HasBest {
			sum += r.Best
			n++
		}
		rows = append(rows, r)
	}
	var qs []qStat
	for _, id := range order {
		qs = append(qs, *stats[id])
	}
	avg := 0.0
	if n > 0 {
		avg = sum / float64(n)
	}
	c.Render("test_results", map[string]any{"Item": it, "Course": co, "Rows": rows, "Stats": qs, "Avg": avg, "Passed": n, "GroupNames": c.groupNames()})
}

// ---------- задания ----------

type taskRow struct {
	User User
	Sub  *Submission
	File *FileMeta
}

func (c *Ctx) taskView(it *Item, co *Course, data map[string]any) {
	if c.canManage(co) {
		subs := map[string]Submission{}
		for _, s := range c.Subs.Find(func(s *Submission) bool { return s.ItemID == it.ID }) {
			subs[s.UserID] = s
		}
		var rows []taskRow
		for _, u := range c.students(co, "") {
			r := taskRow{User: u}
			if s, ok := subs[u.ID]; ok {
				r.Sub = &s
				if m, ok := c.Files.Get(s.FileID); ok {
					r.File = &m
				}
			}
			rows = append(rows, r)
		}
		data["Rows"], data["GroupNames"] = rows, c.groupNames()
	} else if s, ok := c.Subs.Get(it.ID + ":" + c.User.ID); ok {
		data["Sub"] = s
		if m, ok := c.Files.Get(s.FileID); ok {
			data["SubFile"] = m
		}
	}
	data["Overdue"] = !it.CloseAt.IsZero() && time.Now().After(it.CloseAt)
	c.Render("task", data)
}

func (c *Ctx) taskSubmit() {
	it, co, ok := c.item(false)
	if !ok || it.Kind != KindTask {
		return
	}
	back := "/items/" + it.ID
	id := it.ID + ":" + c.User.ID
	old, exists := c.Subs.Get(id)
	if exists && old.Graded {
		c.Redirect(back, "!Работа уже оценена, повторная сдача невозможна.")
		return
	}
	s := Submission{ID: id, ItemID: it.ID, CourseID: co.ID, UserID: c.User.ID, Text: c.F("text"), Submitted: time.Now(), FileID: old.FileID}
	s.Late = !it.CloseAt.IsZero() && s.Submitted.After(it.CloseAt)
	if len(s.Text) > 50000 {
		c.Redirect(back, "!Текст ответа слишком длинный.")
		return
	}
	if c.R.MultipartForm != nil && len(c.R.MultipartForm.File["file"]) > 0 {
		m, err := c.saveUpload(c.R.MultipartForm.File["file"][0], co.ID, "sub")
		if err != nil {
			c.Redirect(back, "!Файл не принят: "+err.Error()+".")
			return
		}
		c.removeFile(old.FileID)
		s.FileID = m.ID
	}
	if s.Text == "" && s.FileID == "" {
		c.Redirect(back, "!Добавьте текст ответа или файл.")
		return
	}
	c.Subs.Put(id, s)
	c.Log("сдача работы", co.Title+" / "+it.Title, true, "")
	for _, t := range co.TeacherIDs {
		c.notify(t, c.User.FullName+" сдал(а) работу «"+it.Title+"».", back)
	}
	c.Redirect(back, "Работа отправлена на проверку.")
}

func (c *Ctx) taskGrade() {
	s, ok := c.Subs.Get(c.R.PathValue("id"))
	if !ok {
		c.Redirect("/", "!Работа не найдена.")
		return
	}
	co, ok := c.course(s.CourseID, true)
	if !ok {
		return
	}
	it, _ := c.Items.Get(s.ItemID)
	pts := atof(c.F("points"), -1, -1, it.MaxPoints)
	if pts < 0 {
		c.Redirect("/items/"+it.ID, "!Укажите баллы от 0 до "+fmtNum(it.MaxPoints)+".")
		return
	}
	c.Subs.Update(s.ID, func(x *Submission) error {
		x.Graded, x.Points, x.Comment, x.GradedBy, x.GradedAt = true, pts, c.F("comment"), c.User.ID, time.Now()
		return nil
	})
	c.Log("выставление оценки", co.Title+" / "+it.Title, true, fmt.Sprintf("%s: %s", c.userName(s.UserID), fmtNum(pts)))
	c.notify(s.UserID, fmt.Sprintf("Работа «%s» оценена: %s из %s.", it.Title, fmtNum(pts), fmtNum(it.MaxPoints)), "/items/"+it.ID)
	c.Redirect("/items/"+it.ID, "Оценка выставлена.")
}
