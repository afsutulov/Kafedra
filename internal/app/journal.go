package app

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"kafedra/internal/secure"
)

// ---------- журнал ----------

type jCol struct {
	ID     string
	Title  string
	Kind   string // test, task, manual
	Max    float64
	Manual bool
	Date   time.Time
}

type attStat struct{ Present, Total int }

type gradebook struct {
	Cols   []jCol
	Max    float64
	Points map[string]map[string]float64 // учащийся → столбец → баллы
	Att    map[string]attStat
}

// gradebook собирает журнал курса: тесты (лучшая попытка), задания и ручные столбцы.
func (a *App) gradebook(courseID string) *gradebook {
	g := &gradebook{Points: map[string]map[string]float64{}, Att: map[string]attStat{}}
	set := func(u, col string, v float64, best bool) {
		m := g.Points[u]
		if m == nil {
			m = map[string]float64{}
			g.Points[u] = m
		}
		if old, ok := m[col]; !ok || !best || v > old {
			m[col] = v
		}
	}
	for _, it := range a.courseItems(courseID, true) {
		if it.Kind == KindTest || it.Kind == KindTask {
			g.Cols = append(g.Cols, jCol{ID: it.ID, Title: it.Title, Kind: it.Kind, Max: it.MaxPoints, Date: it.CloseAt})
		}
	}
	manual := sortBy(a.Columns.Find(func(c *Column) bool { return c.CourseID == courseID }), func(x, y *Column) bool {
		if x.Date.Equal(y.Date) {
			return x.Title < y.Title
		}
		return x.Date.Before(y.Date)
	})
	for _, c := range manual {
		g.Cols = append(g.Cols, jCol{ID: c.ID, Title: c.Title, Kind: "manual", Max: c.MaxPoints, Manual: true, Date: c.Date})
	}
	valid := map[string]bool{}
	for _, c := range g.Cols {
		g.Max += c.Max
		valid[c.ID] = true
	}
	for _, at := range a.Attempts.Find(func(x *Attempt) bool { return x.CourseID == courseID && x.Done }) {
		if valid[at.ItemID] {
			set(at.UserID, at.ItemID, at.Points, true)
		}
	}
	for _, s := range a.Subs.Find(func(x *Submission) bool { return x.CourseID == courseID && x.Graded }) {
		if valid[s.ItemID] {
			set(s.UserID, s.ItemID, s.Points, false)
		}
	}
	for _, gr := range a.Grades.Find(func(x *Grade) bool { return x.CourseID == courseID }) {
		if valid[gr.ColumnID] {
			set(gr.UserID, gr.ColumnID, gr.Points, false)
		}
	}
	for _, at := range a.Attend.Find(func(x *Attendance) bool { return x.CourseID == courseID }) {
		s := g.Att[at.UserID]
		s.Total++
		if at.Status == "p" || at.Status == "l" {
			s.Present++
		}
		g.Att[at.UserID] = s
	}
	return g
}

func (g *gradebook) total(userID string) float64 {
	t := 0.0
	for _, v := range g.Points[userID] {
		t += v
	}
	return t
}

func (a *App) pointsOf(courseID, userID string) (map[string]float64, float64) {
	g := a.gradebook(courseID)
	return g.Points[userID], g.total(userID)
}

func percent(v, max float64) float64 {
	if max <= 0 {
		return 0
	}
	return float64(int(v/max*1000+0.5)) / 10
}

func (a *App) mark(p float64) string {
	s := a.S()
	switch {
	case p >= float64(s.Grade5):
		return "5"
	case p >= float64(s.Grade4):
		return "4"
	case p >= float64(s.Grade3):
		return "3"
	}
	return "2"
}

type jRow struct {
	User    User
	Points  map[string]float64
	Total   float64
	Percent float64
	Mark    string
	Att     attStat
}

func (c *Ctx) journalRows(co *Course, groupID string) (*gradebook, []jRow) {
	g := c.gradebook(co.ID)
	var rows []jRow
	for _, u := range c.students(co, groupID) {
		t := g.total(u.ID)
		p := percent(t, g.Max)
		rows = append(rows, jRow{u, g.Points[u.ID], t, p, c.mark(p), g.Att[u.ID]})
	}
	return g, rows
}

func (c *Ctx) journal() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	group := c.R.URL.Query().Get("group")
	if !contains(co.GroupIDs, group) {
		group = ""
		if len(co.GroupIDs) > 0 {
			group = co.GroupIDs[0]
		}
	}
	g, rows := c.journalRows(co, group)
	c.Render("journal", map[string]any{"Course": co, "G": g, "Rows": rows, "Group": group, "GroupNames": c.groupNames(), "Tab": "journal", "Manage": true, "S": c.S()})
}

func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		s = "'" + s // защита от внедрения формул при открытии в табличном редакторе
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (c *Ctx) journalCSV() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	g, rows := c.journalRows(co, c.R.URL.Query().Get("group"))
	names := c.groupNames()
	var b strings.Builder
	b.WriteString("\xef\xbb\xbf")
	head := []string{csvCell("ФИО"), csvCell("Группа")}
	for _, col := range g.Cols {
		head = append(head, csvCell(fmt.Sprintf("%s (макс. %s)", col.Title, fmtNum(col.Max))))
	}
	head = append(head, csvCell("Итого"), csvCell("%"), csvCell("Оценка"), csvCell("Посещено"), csvCell("Занятий"))
	b.WriteString(strings.Join(head, ";") + "\r\n")
	for _, r := range rows {
		line := []string{csvCell(r.User.FullName), csvCell(names[r.User.GroupID])}
		for _, col := range g.Cols {
			if v, ok := r.Points[col.ID]; ok {
				line = append(line, fmtNum(v))
			} else {
				line = append(line, "")
			}
		}
		line = append(line, fmtNum(r.Total), fmtNum(r.Percent), r.Mark, fmt.Sprint(r.Att.Present), fmt.Sprint(r.Att.Total))
		b.WriteString(strings.Join(line, ";") + "\r\n")
	}
	c.Log("выгрузка журнала", co.Title, true, "")
	c.W.Header().Set("Content-Type", "text/csv; charset=utf-8")
	c.W.Header().Set("Content-Disposition", `attachment; filename="journal.csv"`)
	c.W.Write([]byte(b.String()))
}

func (c *Ctx) columnCreate() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	title := c.F("title")
	if title == "" {
		c.Redirect(c.Back("/courses/"+co.ID+"/journal"), "!Укажите название столбца.")
		return
	}
	d := c.parseLocal(c.F("date"))
	if d.IsZero() {
		d = time.Now()
	}
	id := secure.NewID()
	c.Columns.Put(id, Column{ID: id, CourseID: co.ID, Title: title, MaxPoints: atof(c.F("max_points"), 5, 0, 1000), Date: d})
	c.Redirect(c.Back("/courses/"+co.ID+"/journal"), "Столбец добавлен.")
}

func (c *Ctx) columnDelete() {
	col, ok := c.Columns.Get(c.R.PathValue("id"))
	if !ok {
		c.Redirect("/", "")
		return
	}
	co, ok := c.course(col.CourseID, true)
	if !ok {
		return
	}
	for _, g := range c.Grades.Find(func(g *Grade) bool { return g.ColumnID == col.ID }) {
		c.Grades.Delete(g.ID)
	}
	c.Columns.Delete(col.ID)
	c.Log("удаление столбца журнала", co.Title+" / "+col.Title, true, "")
	c.Redirect(c.Back("/courses/"+co.ID+"/journal"), "Столбец удалён.")
}

func (c *Ctx) gradesSave() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	cols := c.Columns.Find(func(x *Column) bool { return x.CourseID == co.ID })
	changed := 0
	var bad []string
	for _, u := range c.students(co, c.F("group")) {
		for _, col := range cols {
			key := "g_" + col.ID + "_" + u.ID
			if _, sent := c.R.Form[key]; !sent {
				continue
			}
			id := col.ID + ":" + u.ID
			old, had := c.Grades.Get(id)
			raw := c.F(key)
			if raw == "" {
				if had {
					c.Grades.Delete(id)
					changed++
				}
				continue
			}
			v := atof(raw, -1, -1, 1e9)
			if v < 0 || v > col.MaxPoints {
				bad = append(bad, u.FullName+" / "+col.Title)
				continue
			}
			if !had || old.Points != v {
				c.Grades.Put(id, Grade{ID: id, ColumnID: col.ID, CourseID: co.ID, UserID: u.ID, Points: v, By: c.User.ID, At: time.Now()})
				changed++
				c.notify(u.ID, fmt.Sprintf("%s: «%s» — %s из %s.", co.Title, col.Title, fmtNum(v), fmtNum(col.MaxPoints)), "/grades")
			}
		}
	}
	c.Log("изменение оценок в журнале", co.Title, true, fmt.Sprintf("изменено значений: %d", changed))
	msg := fmt.Sprintf("Журнал сохранён, изменено значений: %d.", changed)
	if len(bad) > 0 {
		msg = "!Часть значений не принята (вне диапазона): " + strings.Join(bad, "; ")
	}
	c.Redirect(c.Back("/courses/"+co.ID+"/journal"), msg)
}

type myCourseGrades struct {
	Course  Course
	Cols    []jCol
	Points  map[string]float64
	Total   float64
	Max     float64
	Percent float64
	Mark    string
	Att     attStat
}

func (c *Ctx) myGrades() {
	var out []myCourseGrades
	for _, co := range c.coursesOf(c.User) {
		g := c.gradebook(co.ID)
		t := g.total(c.User.ID)
		p := percent(t, g.Max)
		out = append(out, myCourseGrades{co, g.Cols, g.Points[c.User.ID], t, g.Max, p, c.mark(p), g.Att[c.User.ID]})
	}
	c.Render("grades", map[string]any{"Courses": out})
}

// ---------- рейтинг ----------

type ratingRow struct {
	Place   int
	Name    string
	Group   string
	Points  float64
	Max     float64
	Percent float64
	Me      bool
}

func (c *Ctx) rating() {
	q := c.R.URL.Query()
	courses := c.coursesOf(c.User)
	selCourse, selGroup := q.Get("course"), q.Get("group")
	if c.User.Role == RoleStudent {
		selGroup = c.User.GroupID
	}
	type acc struct{ pts, max float64 }
	sums := map[string]*acc{}
	users := map[string]User{}
	for _, co := range courses {
		if selCourse != "" && co.ID != selCourse {
			continue
		}
		g := c.gradebook(co.ID)
		if g.Max == 0 {
			continue
		}
		for _, u := range c.students(&co, selGroup) {
			a := sums[u.ID]
			if a == nil {
				a = &acc{}
				sums[u.ID] = a
				users[u.ID] = u
			}
			a.pts += g.total(u.ID)
			a.max += g.Max
		}
	}
	names := c.groupNames()
	showNames := c.User.Role != RoleStudent || c.S().RatingNames
	var rows []ratingRow
	for id, a := range sums {
		r := ratingRow{Name: users[id].FullName, Group: names[users[id].GroupID], Points: a.pts, Max: a.max, Percent: percent(a.pts, a.max), Me: id == c.User.ID}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Percent != rows[j].Percent {
			return rows[i].Percent > rows[j].Percent
		}
		return rows[i].Name < rows[j].Name
	})
	for i := range rows {
		rows[i].Place = i + 1
		if i > 0 && rows[i].Percent == rows[i-1].Percent {
			rows[i].Place = rows[i-1].Place
		}
		if !showNames && !rows[i].Me {
			rows[i].Name = "Участник"
		}
	}
	groupSet := map[string]bool{}
	for _, co := range courses {
		for _, g := range co.GroupIDs {
			groupSet[g] = true
		}
	}
	var groups []Group
	for id := range groupSet {
		groups = append(groups, Group{ID: id, Name: names[id]})
	}
	sortBy(groups, func(x, y *Group) bool { return x.Name < y.Name })
	c.Render("rating", map[string]any{"Rows": rows, "Courses": courses, "Groups": groups, "SelCourse": selCourse, "SelGroup": selGroup})
}

// ---------- расписание ----------

type eventView struct {
	Event   Event
	Course  string
	Group   string
	Teacher string
	Mine    bool
}

func (c *Ctx) eventsFor(from, to time.Time) []eventView {
	mine := map[string]bool{}
	for _, co := range c.coursesOf(c.User) {
		mine[co.ID] = true
	}
	list := c.Events.Find(func(e *Event) bool {
		if e.Start.Before(from) || !e.Start.Before(to) {
			return false
		}
		switch c.User.Role {
		case RoleAdmin:
			return true
		case RoleTeacher:
			return e.TeacherID == c.User.ID || mine[e.CourseID]
		}
		return e.GroupID == c.User.GroupID && mine[e.CourseID]
	})
	sortBy(list, func(x, y *Event) bool { return x.Start.Before(y.Start) })
	names := c.groupNames()
	var out []eventView
	for _, e := range list {
		v := eventView{Event: e, Group: names[e.GroupID], Teacher: c.userName(e.TeacherID)}
		if co, ok := c.Courses.Get(e.CourseID); ok {
			v.Course = co.Title
			v.Mine = c.canManage(&co)
		}
		out = append(out, v)
	}
	return out
}

func (a *App) weekStart(t time.Time) time.Time {
	t = t.In(a.Loc)
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, a.Loc)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

var weekdays = []string{"Понедельник", "Вторник", "Среда", "Четверг", "Пятница", "Суббота", "Воскресенье"}

type dayView struct {
	Name   string
	Date   time.Time
	Today  bool
	Events []eventView
}

func (c *Ctx) schedule() {
	start := c.weekStart(time.Now())
	if w := c.parseLocal(c.R.URL.Query().Get("w")); !w.IsZero() {
		start = c.weekStart(w)
	}
	evs := c.eventsFor(start, start.AddDate(0, 0, 7))
	today := time.Now().In(c.Loc).Format("2006-01-02")
	var days []dayView
	for i := 0; i < 7; i++ {
		d := start.AddDate(0, 0, i)
		dv := dayView{Name: weekdays[i], Date: d, Today: d.Format("2006-01-02") == today}
		for _, e := range evs {
			if e.Event.Start.In(c.Loc).Format("2006-01-02") == d.Format("2006-01-02") {
				dv.Events = append(dv.Events, e)
			}
		}
		days = append(days, dv)
	}
	data := map[string]any{"Days": days, "Prev": start.AddDate(0, 0, -7).Format("2006-01-02"), "Next": start.AddDate(0, 0, 7).Format("2006-01-02"),
		"From": start, "To": start.AddDate(0, 0, 6), "Kinds": eventKinds}
	if c.User.Role != RoleStudent {
		courses := c.coursesOf(c.User)
		data["Courses"] = courses
		data["GroupNames"] = c.groupNames()
	}
	c.Render("schedule", data)
}

func (c *Ctx) eventCreate() {
	parts := strings.SplitN(c.F("target"), ":", 2) // курс:группа
	if len(parts) != 2 {
		c.Redirect("/schedule", "!Выберите курс и группу.")
		return
	}
	co, ok := c.course(parts[0], true)
	if !ok {
		return
	}
	if !contains(co.GroupIDs, parts[1]) || eventKinds[c.F("kind")] == "" {
		c.Redirect("/schedule", "!Некорректные параметры занятия.")
		return
	}
	start := c.parseLocal(c.F("date") + "T" + c.F("from"))
	end := c.parseLocal(c.F("date") + "T" + c.F("to"))
	if start.IsZero() || !end.After(start) {
		c.Redirect("/schedule", "!Проверьте дату и время: окончание должно быть позже начала.")
		return
	}
	teacher := c.User.ID
	if c.User.Role == RoleAdmin && len(co.TeacherIDs) > 0 {
		teacher = co.TeacherIDs[0]
	}
	weeks := atoi(c.F("weeks"), 1, 1, 30)
	room := c.F("room")
	created, conflicts := 0, 0
	for w := 0; w < weeks; w++ {
		s, e := start.AddDate(0, 0, 7*w), end.AddDate(0, 0, 7*w)
		clash := c.Events.Count(func(x *Event) bool {
			return x.Start.Before(e) && s.Before(x.End) && (x.GroupID == parts[1] || x.TeacherID == teacher || (room != "" && x.Room == room))
		})
		if clash > 0 {
			conflicts++
			continue
		}
		id := secure.NewID()
		c.Events.Put(id, Event{ID: id, CourseID: co.ID, GroupID: parts[1], TeacherID: teacher, Kind: c.F("kind"), Start: s, End: e, Room: room, Note: c.F("note")})
		created++
	}
	msg := fmt.Sprintf("Добавлено занятий: %d.", created)
	if conflicts > 0 {
		msg = fmt.Sprintf("!Добавлено занятий: %d. Пропущено из-за пересечений (группа, преподаватель или аудитория заняты): %d.", created, conflicts)
	}
	c.Redirect("/schedule?w="+start.Format("2006-01-02"), msg)
}

func (c *Ctx) event() (*Event, *Course, bool) {
	e, ok := c.Events.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Занятие не найдено.")
		return nil, nil, false
	}
	co, ok := c.course(e.CourseID, true)
	return &e, co, ok
}

func (c *Ctx) eventView() {
	e, co, ok := c.event()
	if !ok {
		return
	}
	marks := map[string]string{}
	for _, a := range c.Attend.Find(func(a *Attendance) bool { return a.EventID == e.ID }) {
		marks[a.UserID] = a.Status
	}
	c.Render("event", map[string]any{"Event": e, "Course": co, "Students": c.students(co, e.GroupID), "Marks": marks, "Group": c.groupNames()[e.GroupID]})
}

func (c *Ctx) attendanceSave() {
	e, co, ok := c.event()
	if !ok {
		return
	}
	for _, u := range c.students(co, e.GroupID) {
		st := c.F("a_" + u.ID)
		id := e.ID + ":" + u.ID
		if strings.Contains("pael", st) && len(st) == 1 {
			c.Attend.Put(id, Attendance{ID: id, EventID: e.ID, CourseID: co.ID, UserID: u.ID, Status: st})
		} else {
			c.Attend.Delete(id)
		}
	}
	c.Log("отметка посещаемости", co.Title, true, fmtTime(e.Start, c.Loc, "02.01.2006 15:04"))
	c.Redirect("/events/"+e.ID, "Посещаемость сохранена.")
}

func (c *Ctx) eventDelete() {
	e, _, ok := c.event()
	if !ok {
		return
	}
	for _, a := range c.Attend.Find(func(a *Attendance) bool { return a.EventID == e.ID }) {
		c.Attend.Delete(a.ID)
	}
	c.Events.Delete(e.ID)
	c.Redirect("/schedule?w="+e.Start.In(c.Loc).Format("2006-01-02"), "Занятие удалено.")
}

func icsText(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r", "", "\n", `\n`).Replace(s)
}

func (c *Ctx) scheduleICS() {
	now := time.Now()
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Kafedra//LMS//RU\r\nCALSCALE:GREGORIAN\r\n")
	for _, e := range c.eventsFor(now.AddDate(0, -1, 0), now.AddDate(0, 6, 0)) {
		const f = "20060102T150405Z"
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:%s@kafedra\r\nDTSTAMP:%s\r\nDTSTART:%s\r\nDTEND:%s\r\nSUMMARY:%s\r\nLOCATION:%s\r\nDESCRIPTION:%s\r\nEND:VEVENT\r\n",
			e.Event.ID, now.UTC().Format(f), e.Event.Start.UTC().Format(f), e.Event.End.UTC().Format(f),
			icsText(eventKinds[e.Event.Kind]+": "+e.Course), icsText(e.Event.Room), icsText(e.Group+". "+e.Teacher+". "+e.Event.Note))
	}
	b.WriteString("END:VCALENDAR\r\n")
	c.W.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	c.W.Header().Set("Content-Disposition", `attachment; filename="schedule.ics"`)
	c.W.Write([]byte(b.String()))
}

// ---------- портфолио ----------

type pfWork struct {
	Course string
	Title  string
	Points float64
	Max    float64
	At     time.Time
}

func (c *Ctx) portfolio() {
	owner := *c.User
	if c.User.Role != RoleStudent {
		u, ok := c.Users.Get(c.R.URL.Query().Get("user"))
		if !ok || u.Role != RoleStudent {
			c.Redirect("/", "!Портфолио открывается из списка участников курса.")
			return
		}
		if c.User.Role == RoleTeacher { // преподаватель видит портфолио только своих учащихся
			teaches := false
			for _, co := range c.coursesOf(c.User) {
				teaches = teaches || contains(co.GroupIDs, u.GroupID)
			}
			if !teaches {
				c.Deny()
				return
			}
		}
		owner = u
		c.Log("просмотр портфолио", u.Login, true, "")
	}
	entries := sortBy(c.Portfolio.Find(func(p *PortfolioEntry) bool { return p.UserID == owner.ID }), func(x, y *PortfolioEntry) bool { return x.Created.After(y.Created) })
	files := map[string]FileMeta{}
	for _, e := range entries {
		if m, ok := c.Files.Get(e.FileID); ok {
			files[e.ID] = m
		}
	}
	var works []pfWork
	for _, s := range c.Subs.Find(func(s *Submission) bool { return s.UserID == owner.ID && s.Graded }) {
		it, ok1 := c.Items.Get(s.ItemID)
		co, ok2 := c.Courses.Get(s.CourseID)
		if ok1 && ok2 {
			works = append(works, pfWork{co.Title, it.Title, s.Points, it.MaxPoints, s.GradedAt})
		}
	}
	sortBy(works, func(x, y *pfWork) bool { return x.At.After(y.At) })
	c.Render("portfolio", map[string]any{"Owner": owner, "Entries": entries, "Files": files, "Works": works, "Own": owner.ID == c.User.ID,
		"Group": c.groupNames()[owner.GroupID]})
}

func (c *Ctx) portfolioAdd() {
	title := c.F("title")
	if title == "" {
		c.Redirect("/portfolio", "!Укажите название.")
		return
	}
	e := PortfolioEntry{ID: secure.NewID(), UserID: c.User.ID, Title: title, Description: c.F("description"), Created: time.Now()}
	if c.R.MultipartForm != nil && len(c.R.MultipartForm.File["file"]) > 0 {
		m, err := c.saveUpload(c.R.MultipartForm.File["file"][0], "", "pf")
		if err != nil {
			c.Redirect("/portfolio", "!Файл не принят: "+err.Error()+".")
			return
		}
		e.FileID = m.ID
	}
	c.Portfolio.Put(e.ID, e)
	c.Redirect("/portfolio", "Запись добавлена.")
}

func (c *Ctx) portfolioDelete() {
	e, ok := c.Portfolio.Get(c.R.PathValue("id"))
	if !ok || e.UserID != c.User.ID {
		c.Deny()
		return
	}
	c.removeFile(e.FileID)
	c.Portfolio.Delete(e.ID)
	c.Redirect("/portfolio", "Запись удалена.")
}

// ---------- главная страница и уведомления ----------

type deadline struct {
	Item   Item
	Course string
}

type pending struct {
	Item   Item
	Course string
	Count  int
}

func (c *Ctx) dashboard() {
	now := time.Now()
	courses := c.coursesOf(c.User)
	ids := map[string]string{}
	for _, co := range courses {
		ids[co.ID] = co.Title
	}
	data := map[string]any{"Events": c.eventsFor(now.Add(-time.Hour), now.AddDate(0, 0, 7))}
	data["News"] = c.newsView(func(n *News) bool { _, mine := ids[n.CourseID]; return n.CourseID == "" || mine }, 6)
	switch c.User.Role {
	case RoleStudent:
		data["Cards"] = c.cards()
		var dl []deadline
		for cid, title := range ids {
			done := map[string]bool{}
			for _, a := range c.Attempts.Find(func(a *Attempt) bool { return a.UserID == c.User.ID && a.CourseID == cid && a.Done }) {
				done[a.ItemID] = true
			}
			for _, s := range c.Subs.Find(func(s *Submission) bool { return s.UserID == c.User.ID && s.CourseID == cid }) {
				done[s.ItemID] = true
			}
			for _, it := range c.courseItems(cid, true) {
				if (it.Kind == KindTest || it.Kind == KindTask) && !done[it.ID] && it.CloseAt.After(now) {
					dl = append(dl, deadline{it, title})
				}
			}
		}
		sortBy(dl, func(x, y *deadline) bool { return x.Item.CloseAt.Before(y.Item.CloseAt) })
		if len(dl) > 8 {
			dl = dl[:8]
		}
		data["Deadlines"] = dl
	case RoleTeacher:
		data["Cards"] = c.cards()
		cnt := map[string]int{}
		for _, s := range c.Subs.Find(func(s *Submission) bool { _, mine := ids[s.CourseID]; return mine && !s.Graded }) {
			cnt[s.ItemID]++
		}
		var pend []pending
		for id, n := range cnt {
			if it, ok := c.Items.Get(id); ok {
				pend = append(pend, pending{it, ids[it.CourseID], n})
			}
		}
		sortBy(pend, func(x, y *pending) bool { return x.Count > y.Count })
		data["Pending"] = pend
	case RoleAdmin:
		var size int64
		for _, f := range c.Files.All() {
			size += f.Size
		}
		failed := 0
		if files := secure.AuditFiles(c.Cfg.DataDir + "/audit"); len(files) > 0 {
			recs, _ := secure.ReadAuditFile(files[len(files)-1])
			for _, r := range recs {
				if !r.OK && now.Sub(r.Time) < 24*time.Hour {
					failed++
				}
			}
		}
		data["Stats"] = map[string]any{
			"Students": c.Users.Count(func(u *User) bool { return u.Role == RoleStudent }),
			"Teachers": c.Users.Count(func(u *User) bool { return u.Role == RoleTeacher }),
			"Courses":  len(courses), "Groups": c.Groups.Count(nil), "Files": c.Files.Count(nil), "Size": size,
			"Sessions": c.Sessions.Count(nil), "Failed": failed,
		}
	}
	c.Render("dashboard", data)
}

func (c *Ctx) notices() {
	list := sortBy(c.Notices.Find(func(n *Notice) bool { return n.UserID == c.User.ID }), func(x, y *Notice) bool { return x.Created.After(y.Created) })
	for i, n := range list {
		if i >= 100 { // хранится не более 100 последних уведомлений
			c.Notices.Delete(n.ID)
		} else if !n.Read {
			c.Notices.Update(n.ID, func(x *Notice) error { x.Read = true; return nil })
		}
	}
	if len(list) > 100 {
		list = list[:100]
	}
	c.Render("notices", map[string]any{"List": list})
}
