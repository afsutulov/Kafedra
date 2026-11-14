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
	"kafedra/internal/store"
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
	err := c.Items.Update(it.ID, func(x *Item) error {
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
	if err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить вопрос.")
		return
	}
	c.Redirect(back, "Вопрос сохранён.")
}

func (c *Ctx) questionDelete() {
	it, _, ok := c.item(true)
	if !ok {
		return
	}
	qid := c.F("qid")
	err := c.Items.Update(it.ID, func(x *Item) error {
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
	if err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось удалить вопрос.")
		return
	}
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
	if err := c.Items.Update(it.ID, func(x *Item) error { x.Questions = append(x.Questions, qs...); return nil }); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить вопросы.")
		return
	}
	c.Redirect(back, fmt.Sprintf("Добавлено вопросов: %d.", len(qs)))
}

// ---------- прохождение теста ----------

func norm(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(s), "ё", "е")), " ")
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

func (a *App) preserveAttemptGrading(it *Item, nextMax float64) error {
	attempts := a.Attempts.Find(func(at *Attempt) bool { return at.ItemID == it.ID })
	if len(attempts) > 0 && nextMax != it.MaxPoints {
		return errors.New("максимум баллов нельзя изменить после начала тестирования; создайте новый тест")
	}
	for _, at := range attempts {
		if at.Grading == nil {
			if err := a.Attempts.Update(at.ID, func(x *Attempt) error {
				g := x.grading(it)
				x.Grading = &g
				return nil
			}); err != nil {
				return fmt.Errorf("не удалось сохранить настройки прежней попытки: %w", err)
			}
		}
	}
	return nil
}

func (at *Attempt) grading(it *Item) AttemptGrading {
	if at.Grading != nil {
		return *at.Grading
	}
	return AttemptGrading{MaxPoints: it.MaxPoints, PassPercent: it.PassPercent}
}

func (at *Attempt) passed(it *Item) bool {
	return at.MaxScore > 0 && at.Score/at.MaxScore*100 >= float64(at.grading(it).PassPercent)
}

func completeAttempt(at *Attempt, it *Item, now time.Time) {
	if at.Grading == nil {
		g := at.grading(it)
		at.Grading = &g
	}
	at.Score, at.MaxScore = 0, 0
	for i := range at.Questions {
		q := &at.Questions[i]
		at.MaxScore += q.Points
		at.Score += scoreQuestion(q, at.Answers[q.ID])
	}
	if at.MaxScore > 0 {
		at.Points = float64(int(at.Score/at.MaxScore*at.Grading.MaxPoints*100+0.5)) / 100
	}
	at.Done, at.Finished = true, now
	if !at.Deadline.IsZero() && at.Finished.After(at.Deadline) {
		at.Finished = at.Deadline
	}
}

// Таймер браузера отправляет форму по истечении времени; допуск учитывает доставку запроса
// по медленной сети. Момент прихода фиксируется при поступлении запроса (Ctx.Arrived),
// поэтому ожидание в очереди на запись в допуск не входит.
const attemptSubmitGrace = 30 * time.Second

func (a *App) finishExpiredAttempt(id string, now time.Time) error {
	a.attemptQueueMu.Lock()
	defer a.attemptQueueMu.Unlock()
	if a.attemptQueue[id] > 0 {
		return nil
	}
	cur, ok := a.Attempts.Get(id)
	if !ok || cur.Done || cur.Deadline.IsZero() || now.Before(cur.Deadline.Add(attemptSubmitGrace)) {
		return nil
	}
	it, ok := a.Items.Get(cur.ItemID)
	if !ok {
		return store.ErrNotFound
	}
	return a.Attempts.Update(id, func(at *Attempt) error {
		if !at.Done {
			completeAttempt(at, &it, now)
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
		c.Render("quiz", data)
		return
	}
	now := time.Now()
	atts := c.attemptsOf(it.ID, c.User.ID)
	best := -1.0
	bestMax := it.MaxPoints
	for _, a := range atts {
		if !a.Done {
			if !a.Deadline.IsZero() && !now.Before(a.Deadline.Add(attemptSubmitGrace)) {
				if err := c.finishExpiredAttempt(a.ID, now); err != nil {
					c.Fail(http.StatusServiceUnavailable, "Не удалось завершить попытку.")
					return
				}
				if updated, ok := c.Attempts.Get(a.ID); ok && updated.Done {
					c.Redirect("/items/"+it.ID, "")
					return
				}
			}
			data["Active"] = a
		} else if a.Points > best {
			best, bestMax = a.Points, a.grading(it).MaxPoints
		}
	}
	open, why := testOpen(it, now)
	left := it.MaxAttempts - len(atts)
	if it.MaxAttempts > 0 && left <= 0 {
		open, why = false, "Все попытки использованы."
	}
	data["Attempts"], data["Open"], data["Why"], data["Left"] = atts, open, why, left
	data["Best"], data["BestMax"], data["HasBest"] = best, bestMax, best >= 0
	n := len(it.Questions)
	if it.PoolSize > 0 && it.PoolSize < n {
		n = it.PoolSize
	}
	data["QCount"] = n
	c.Render("quiz", data)
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
		Answers: map[string]Answer{}, IP: c.IP, Grading: &AttemptGrading{MaxPoints: it.MaxPoints, PassPercent: it.PassPercent}}
	if it.TimeLimit > 0 {
		at.Deadline = now.Add(time.Duration(it.TimeLimit) * time.Minute)
	}
	if !it.CloseAt.IsZero() && (at.Deadline.IsZero() || it.CloseAt.Before(at.Deadline)) {
		at.Deadline = it.CloseAt
	}
	if err := c.Attempts.Put(at.ID, at); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось начать тестирование.")
		return
	}
	c.Log("начало тестирования", co.Title+" / "+it.Title, true, fmt.Sprintf("попытка %d", len(atts)+1))
	c.Redirect("/attempts/"+at.ID, "")
}

func (c *Ctx) readAnswers(at *Attempt) map[string]Answer {
	out := map[string]Answer{}
	for _, q := range at.Questions {
		var a Answer
		if q.Type == "text" {
			a.Text = c.F("q_" + q.ID)
			if runes := []rune(a.Text); len(runes) > 500 {
				a.Text = string(runes[:500])
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

func (c *Ctx) ownAttempt() (*Attempt, *Item, bool) {
	at, ok := c.Attempts.Get(c.R.PathValue("id"))
	if !ok || at.UserID != c.User.ID {
		c.Deny()
		return nil, nil, false
	}
	co, ok := c.course(at.CourseID, false)
	if !ok {
		return nil, nil, false
	}
	it, ok := c.Items.Get(at.ItemID)
	if !ok || it.CourseID != co.ID || !it.Visible || it.Kind != KindTest {
		c.Deny()
		return nil, nil, false
	}
	return &at, &it, true
}

func (c *Ctx) attemptSave() {
	at, _, ok := c.ownAttempt()
	if !ok {
		return
	}
	if at.Done || (!at.Deadline.IsZero() && !c.Arrived.Before(at.Deadline.Add(attemptSubmitGrace))) {
		c.W.WriteHeader(http.StatusConflict)
		return
	}
	ans := c.readAnswers(at)
	if err := c.Attempts.Update(at.ID, func(x *Attempt) error {
		if x.Done {
			return errAttemptDone
		}
		x.Answers = ans
		return nil
	}); err != nil {
		if errors.Is(err, errAttemptDone) {
			c.W.WriteHeader(http.StatusConflict)
		} else {
			c.W.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	c.W.WriteHeader(http.StatusNoContent)
}

var errAttemptDone = errors.New("попытка завершена")

func (c *Ctx) attemptSubmit() {
	at, it, ok := c.ownAttempt()
	if !ok {
		return
	}
	if at.Done {
		c.Redirect("/attempts/"+at.ID, "")
		return
	}
	ans := c.readAnswers(at)
	received := c.Arrived
	late := false
	err := c.Attempts.Update(at.ID, func(x *Attempt) error {
		if x.Done {
			return errAttemptDone
		}
		late = !x.Deadline.IsZero() && !received.Before(x.Deadline.Add(attemptSubmitGrace))
		if !late {
			x.Answers = ans
		}
		completeAttempt(x, it, received)
		*at = *x
		return nil
	})
	if errors.Is(err, errAttemptDone) {
		c.Redirect("/attempts/"+at.ID, "")
		return
	}
	if err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить результат. Повторите отправку.")
		return
	}
	c.Log("завершение тестирования", at.ItemID, true, fmt.Sprintf("результат %s из %s", fmtNum(at.Score), fmtNum(at.MaxScore)))
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
	if !manage && (at.UserID != c.User.ID || !c.canView(&co) || !it.Visible) {
		c.Deny()
		return
	}
	if !at.Done && !at.Deadline.IsZero() && !time.Now().Before(at.Deadline.Add(attemptSubmitGrace)) {
		if err := c.finishExpiredAttempt(at.ID, time.Now()); err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось завершить попытку.")
			return
		}
		at, ok = c.Attempts.Get(at.ID)
		if !ok {
			c.Fail(http.StatusNotFound, "Попытка не найдена.")
			return
		}
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
	data["Passed"], data["Grading"] = at.passed(&it), at.grading(&it)
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
	if err := c.Attempts.Delete(at.ID); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось аннулировать попытку.")
		return
	}
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
	sum, n, passed := 0.0, 0, 0
	for _, u := range c.students(co, "") {
		r := resultRow{User: u, Attempts: sortBy(byUser[u.ID], func(x, y *Attempt) bool { return x.Started.Before(y.Started) })}
		hasPass := false
		for _, a := range r.Attempts {
			hasPass = hasPass || (a.Done && a.passed(it))
			if a.Done && (!r.HasBest || a.Points > r.Best) {
				r.Best, r.HasBest = a.Points, true
			}
		}
		if hasPass {
			passed++
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
	c.Render("quiz_results", map[string]any{"Item": it, "Course": co, "Rows": rows, "Stats": qs, "Avg": avg, "Passed": passed, "GroupNames": c.groupNames()})
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
		m, err := c.saveUpload(c.R.MultipartForm.File["file"][0], co.ID, "sub", it.ID)
		if err != nil {
			c.Redirect(back, "!Файл не принят: "+err.Error()+".")
			return
		}
		s.FileID = m.ID
	}
	if s.Text == "" && s.FileID == "" {
		c.Redirect(back, "!Добавьте текст ответа или файл.")
		return
	}
	if err := c.Subs.Put(id, s); err != nil {
		if s.FileID != old.FileID {
			c.removeFile(s.FileID)
		}
		c.Log("сдача работы", co.Title+" / "+it.Title, false, err.Error())
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить работу. Повторите отправку.")
		return
	}
	if s.FileID != old.FileID {
		c.removeFile(old.FileID)
	}
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
	pts := atof(c.F("points"), -1, -1, 1e9)
	if pts < 0 || pts > it.MaxPoints {
		c.Redirect("/items/"+it.ID, "!Укажите баллы от 0 до "+fmtNum(it.MaxPoints)+".")
		return
	}
	if err := c.Subs.Update(s.ID, func(x *Submission) error {
		x.Graded, x.Points, x.Comment, x.GradedBy, x.GradedAt = true, pts, c.F("comment"), c.User.ID, time.Now()
		return nil
	}); err != nil {
		c.Log("выставление оценки", co.Title+" / "+it.Title, false, err.Error())
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить оценку.")
		return
	}
	c.Log("выставление оценки", co.Title+" / "+it.Title, true, fmt.Sprintf("%s: %s", c.userName(s.UserID), fmtNum(pts)))
	c.notify(s.UserID, fmt.Sprintf("Работа «%s» оценена: %s из %s.", it.Title, fmtNum(pts), fmtNum(it.MaxPoints)), "/items/"+it.ID)
	c.Redirect("/items/"+it.ID, "Оценка выставлена.")
}
