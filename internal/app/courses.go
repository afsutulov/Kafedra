package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"kafedra/internal/content"
	"kafedra/internal/secure"
)

// ---------- права доступа к курсам ----------

func (c *Ctx) canManage(co *Course) bool {
	return c.User.Role == RoleAdmin || (c.User.Role == RoleTeacher && contains(co.TeacherIDs, c.User.ID))
}

func (c *Ctx) canView(co *Course) bool {
	return c.canManage(co) || (c.User.Role == RoleStudent && c.User.GroupID != "" && contains(co.GroupIDs, c.User.GroupID) && !co.Archived)
}

// course загружает курс и проверяет права; при отказе сам формирует ответ.
func (c *Ctx) course(id string, manage bool) (*Course, bool) {
	co, ok := c.Courses.Get(id)
	if !ok {
		c.Fail(http.StatusNotFound, "Курс не найден.")
		return nil, false
	}
	if (manage && !c.canManage(&co)) || !c.canView(&co) {
		c.Deny()
		return nil, false
	}
	return &co, true
}

// item загружает элемент курса из параметра пути {id} с проверкой прав.
func (c *Ctx) item(manage bool) (*Item, *Course, bool) {
	it, ok := c.Items.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Материал не найден.")
		return nil, nil, false
	}
	co, ok := c.course(it.CourseID, manage)
	if !ok {
		return nil, nil, false
	}
	if !it.Visible && !c.canManage(co) {
		c.Fail(http.StatusNotFound, "Материал не найден.")
		return nil, nil, false
	}
	return &it, co, true
}

func (a *App) coursesOf(u *User) []Course {
	list := a.Courses.Find(func(co *Course) bool {
		switch u.Role {
		case RoleAdmin:
			return true
		case RoleTeacher:
			return contains(co.TeacherIDs, u.ID)
		}
		return u.GroupID != "" && contains(co.GroupIDs, u.GroupID) && !co.Archived
	})
	return sortBy(list, func(x, y *Course) bool { return x.Title < y.Title })
}

// students возвращает учащихся курса (или одной его группы), упорядоченных по ФИО.
func (a *App) students(co *Course, groupID string) []User {
	list := a.Users.Find(func(u *User) bool {
		return u.Role == RoleStudent && !u.Disabled && contains(co.GroupIDs, u.GroupID) && (groupID == "" || u.GroupID == groupID)
	})
	return sortBy(list, func(x, y *User) bool { return x.FullName < y.FullName })
}

func (a *App) courseItems(courseID string, onlyVisible bool) []Item {
	list := a.Items.Find(func(it *Item) bool { return it.CourseID == courseID && (it.Visible || !onlyVisible) })
	return sortBy(list, func(x, y *Item) bool { return x.Order < y.Order })
}

// ---------- курсы ----------

type courseCard struct {
	Course   Course
	Teachers string
	Done     int
	Total    int
}

func (c *Ctx) cards() []courseCard {
	var out []courseCard
	for _, co := range c.coursesOf(c.User) {
		card := courseCard{Course: co}
		var names []string
		for _, t := range co.TeacherIDs {
			names = append(names, c.userName(t))
		}
		card.Teachers = strings.Join(names, ", ")
		if c.User.Role == RoleStudent {
			card.Done, card.Total = c.progressOf(c.User.ID, co.ID)
		}
		out = append(out, card)
	}
	return out
}

// progressOf возвращает число выполненных и общее число опубликованных элементов курса.
func (a *App) progressOf(userID, courseID string) (done, total int) {
	seen := map[string]bool{}
	for _, p := range a.Progress.Find(func(p *Progress) bool { return p.UserID == userID && p.CourseID == courseID }) {
		seen[p.ItemID] = true
	}
	for _, at := range a.Attempts.Find(func(x *Attempt) bool { return x.UserID == userID && x.CourseID == courseID && x.Done }) {
		seen[at.ItemID] = true
	}
	for _, s := range a.Subs.Find(func(x *Submission) bool { return x.UserID == userID && x.CourseID == courseID }) {
		seen[s.ItemID] = true
	}
	for _, it := range a.courseItems(courseID, true) {
		total++
		if seen[it.ID] {
			done++
		}
	}
	return
}

func (c *Ctx) courseList() {
	c.Render("courses", map[string]any{"Cards": c.cards(),
		"CanCreate": c.User.Role == RoleAdmin || (c.User.Role == RoleTeacher && c.S().TeacherCourses)})
}

func (c *Ctx) courseCreate() {
	if c.User.Role == RoleTeacher && !c.S().TeacherCourses {
		c.Deny()
		return
	}
	title := c.F("title")
	if title == "" || len(title) > 300 {
		c.Redirect("/courses", "!Укажите название курса.")
		return
	}
	co := Course{ID: secure.NewID(), Title: title, Created: time.Now(), Sections: []Section{{ID: secure.NewID(), Title: "Раздел 1"}}}
	if c.User.Role == RoleTeacher {
		co.TeacherIDs = []string{c.User.ID}
	}
	c.Courses.Put(co.ID, co)
	c.Log("создание курса", co.Title, true, "")
	c.Redirect("/courses/"+co.ID+"/edit", "Курс создан. Назначьте группы и добавьте материалы.")
}

type itemRow struct {
	Item   Item
	Status string // отметка для учащегося
	Done   bool
}

type sectionView struct {
	Section Section
	Items   []itemRow
}

func (c *Ctx) courseView() {
	co, ok := c.course(c.R.PathValue("id"), false)
	if !ok {
		return
	}
	manage := c.canManage(co)
	tab := c.R.URL.Query().Get("tab")
	data := map[string]any{"Course": co, "Manage": manage, "Tab": tab}
	switch tab {
	case "news":
		data["News"] = c.newsView(func(n *News) bool { return n.CourseID == co.ID }, 100)
	case "talk":
		posts := sortBy(c.Posts.Find(func(p *Post) bool { return p.CourseID == co.ID }), func(x, y *Post) bool { return x.Created.After(y.Created) })
		if len(posts) > 200 {
			posts = posts[:200]
		}
		type pv struct {
			Post   Post
			Author string
			Role   string
			Mine   bool
		}
		var out []pv
		for _, p := range posts {
			u, _ := c.Users.Get(p.AuthorID)
			out = append(out, pv{p, c.userName(p.AuthorID), u.Role, p.AuthorID == c.User.ID})
		}
		data["Posts"] = out
	case "people":
		var teachers []User
		for _, id := range co.TeacherIDs {
			if u, ok := c.Users.Get(id); ok {
				teachers = append(teachers, u)
			}
		}
		data["Teachers"] = teachers
		if manage {
			data["Students"] = c.students(co, "")
			data["GroupNames"] = c.groupNames()
		}
	default:
		data["Tab"] = "materials"
		done := map[string]string{}
		if !manage {
			for _, p := range c.Progress.Find(func(p *Progress) bool { return p.UserID == c.User.ID && p.CourseID == co.ID }) {
				done[p.ItemID] = "изучено"
			}
			pts, _ := c.pointsOf(co.ID, c.User.ID)
			for id, v := range pts {
				done[id] = fmtNum(v) + " б."
			}
			for _, s := range c.Subs.Find(func(s *Submission) bool { return s.UserID == c.User.ID && s.CourseID == co.ID && !s.Graded }) {
				done[s.ItemID] = "на проверке"
			}
		}
		bySec := map[string][]itemRow{}
		for _, it := range c.courseItems(co.ID, !manage) {
			sid := it.SectionID
			found := false
			for _, s := range co.Sections {
				found = found || s.ID == sid
			}
			if !found {
				sid = ""
			}
			bySec[sid] = append(bySec[sid], itemRow{Item: it, Status: done[it.ID], Done: done[it.ID] != ""})
		}
		var views []sectionView
		for _, s := range co.Sections {
			views = append(views, sectionView{s, bySec[s.ID]})
		}
		if len(bySec[""]) > 0 {
			views = append(views, sectionView{Section{Title: "Без раздела"}, bySec[""]})
		}
		data["Sections"] = views
		data["Desc"] = template.HTML(content.Render(co.Description))
		if !manage {
			d, t := c.progressOf(c.User.ID, co.ID)
			data["Done"], data["Total"] = d, t
		}
	}
	c.Render("course", data)
}

func (a *App) groupNames() map[string]string {
	m := map[string]string{}
	for _, g := range a.Groups.All() {
		m[g.ID] = g.Name
	}
	return m
}

func (c *Ctx) courseEdit() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	c.Render("course_edit", map[string]any{"Course": co,
		"Groups":   sortBy(c.Groups.All(), func(x, y *Group) bool { return x.Name < y.Name }),
		"Teachers": sortBy(c.Users.Find(func(u *User) bool { return u.Role == RoleTeacher && !u.Disabled }), func(x, y *User) bool { return x.FullName < y.FullName }),
	})
}

func (c *Ctx) courseSave() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	title := c.F("title")
	if title == "" {
		c.Redirect("/courses/"+co.ID+"/edit", "!Название не может быть пустым.")
		return
	}
	groups := c.validIDs(c.R.Form["groups"], func(id string) bool { _, ok := c.Groups.Get(id); return ok })
	teachers := c.validIDs(c.R.Form["teachers"], func(id string) bool { u, ok := c.Users.Get(id); return ok && u.Role == RoleTeacher })
	admin := c.User.Role == RoleAdmin
	c.Courses.Update(co.ID, func(x *Course) error {
		x.Title, x.Description, x.GroupIDs = title, c.R.FormValue("description"), groups
		if admin { // состав преподавателей и архивирование — только администратор
			x.TeacherIDs, x.Archived = teachers, c.F("archived") == "1"
		}
		return nil
	})
	c.Log("изменение курса", title, true, fmt.Sprintf("групп: %d", len(groups)))
	c.Redirect("/courses/"+co.ID, "Курс сохранён.")
}

func (c *Ctx) validIDs(in []string, ok func(string) bool) []string {
	out := []string{}
	for _, id := range in {
		if ok(id) && !contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func (c *Ctx) courseDelete() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	for _, it := range c.courseItems(co.ID, false) {
		c.deleteItem(&it)
	}
	for _, x := range c.Columns.Find(func(x *Column) bool { return x.CourseID == co.ID }) {
		c.Columns.Delete(x.ID)
	}
	for _, x := range c.Grades.Find(func(x *Grade) bool { return x.CourseID == co.ID }) {
		c.Grades.Delete(x.ID)
	}
	for _, x := range c.Events.Find(func(x *Event) bool { return x.CourseID == co.ID }) {
		c.Events.Delete(x.ID)
	}
	for _, x := range c.Attend.Find(func(x *Attendance) bool { return x.CourseID == co.ID }) {
		c.Attend.Delete(x.ID)
	}
	for _, x := range c.NewsC.Find(func(x *News) bool { return x.CourseID == co.ID }) {
		c.NewsC.Delete(x.ID)
	}
	for _, x := range c.Posts.Find(func(x *Post) bool { return x.CourseID == co.ID }) {
		c.Posts.Delete(x.ID)
	}
	c.Courses.Delete(co.ID)
	c.Log("удаление курса", co.Title, true, "")
	c.Redirect("/courses", "Курс удалён.")
}

func (c *Ctx) sectionSave() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	sid, title, del := c.F("sid"), c.F("title"), c.F("delete") == "1"
	c.Courses.Update(co.ID, func(x *Course) error {
		for i := range x.Sections {
			if x.Sections[i].ID == sid {
				if del {
					x.Sections = append(x.Sections[:i], x.Sections[i+1:]...)
				} else if title != "" {
					x.Sections[i].Title = title
				}
				return nil
			}
		}
		if sid == "" && title != "" {
			x.Sections = append(x.Sections, Section{ID: secure.NewID(), Title: title})
		}
		return nil
	})
	c.Redirect(c.Back("/courses/"+co.ID), "")
}

// ---------- элементы курса ----------

func (c *Ctx) itemCreate() {
	co, ok := c.course(c.R.PathValue("id"), true)
	if !ok {
		return
	}
	kind, title := c.F("kind"), c.F("title")
	if kindName(kind) == "" || title == "" {
		c.Redirect("/courses/"+co.ID, "!Укажите название.")
		return
	}
	order := 0
	for _, it := range c.courseItems(co.ID, false) {
		order = max(order, it.Order)
	}
	it := Item{ID: secure.NewID(), CourseID: co.ID, SectionID: c.F("section"), Kind: kind, Title: title, Order: order + 1,
		Created: time.Now(), Updated: time.Now()}
	switch kind {
	case KindTest:
		it.MaxPoints, it.MaxAttempts, it.PassPercent, it.Shuffle, it.ShowAnswers = 10, 1, 50, true, true
	case KindTask:
		it.MaxPoints = 10
	}
	c.Items.Put(it.ID, it)
	c.Redirect("/items/"+it.ID+"/edit", "")
}

type fileView struct {
	Meta FileMeta
	Type string // image, pdf, video, audio, html, other
}

func fileType(ext string) string {
	switch ext {
	case "png", "jpg", "jpeg", "gif", "webp":
		return "image"
	case "pdf":
		return "pdf"
	case "mp4", "webm":
		return "video"
	case "mp3", "ogg":
		return "audio"
	case "html":
		return "html"
	}
	return "other"
}

func (a *App) fileViews(ids []string) []fileView {
	var out []fileView
	for _, id := range ids {
		if m, ok := a.Files.Get(id); ok {
			out = append(out, fileView{m, fileType(m.Ext)})
		}
	}
	return out
}

func (c *Ctx) itemView() {
	it, co, ok := c.item(false)
	if !ok {
		return
	}
	manage := c.canManage(co)
	data := map[string]any{"Item": it, "Course": co, "Manage": manage, "Files": c.fileViews(it.FileIDs),
		"Body": template.HTML(content.Render(it.Body))}
	// соседние элементы для перехода «назад / далее»
	items := c.courseItems(co.ID, !manage)
	for i := range items {
		if items[i].ID == it.ID {
			if i > 0 {
				data["Prev"] = items[i-1]
			}
			if i+1 < len(items) {
				data["Next"] = items[i+1]
			}
		}
	}
	switch it.Kind {
	case KindLesson, KindFile:
		if c.User.Role == RoleStudent {
			id := c.User.ID + ":" + it.ID
			if _, seen := c.Progress.Get(id); !seen {
				c.Progress.Put(id, Progress{ID: id, UserID: c.User.ID, ItemID: it.ID, CourseID: co.ID, At: time.Now()})
			}
		}
		c.Render("lesson", data)
	case KindTest:
		c.testView(it, co, data)
	case KindTask:
		c.taskView(it, co, data)
	}
}

func (c *Ctx) itemEdit() {
	it, co, ok := c.item(true)
	if !ok {
		return
	}
	data := map[string]any{"Item": it, "Course": co, "Files": c.fileViews(it.FileIDs)}
	if it.Kind == KindTest {
		if qid := c.R.URL.Query().Get("q"); qid != "" {
			for _, q := range it.Questions {
				if q.ID == qid {
					data["EditQ"] = q
				}
			}
		}
		total := 0.0
		for _, q := range it.Questions {
			total += q.Points
		}
		data["QTotal"] = total
		data["Attempts"] = c.Attempts.Count(func(a *Attempt) bool { return a.ItemID == it.ID })
	}
	c.Render("item_edit", data)
}

func atoi(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

func atof(s string, def, lo, hi float64) float64 {
	f, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(s), ",", ".", 1), 64)
	if err != nil || f != f {
		return def
	}
	return min(max(f, lo), hi)
}

func (c *Ctx) itemSave() {
	it, co, ok := c.item(true)
	if !ok {
		return
	}
	back := "/items/" + it.ID + "/edit"
	title := c.F("title")
	if title == "" {
		c.Redirect(back, "!Название не может быть пустым.")
		return
	}
	var newFiles []string
	var notes []string
	body := c.R.FormValue("body")
	if c.R.MultipartForm != nil {
		for _, fh := range c.R.MultipartForm.File["attach"] {
			m, err := c.saveUpload(fh, co.ID, "course")
			if err != nil {
				notes = append(notes, fh.Filename+": "+err.Error())
				continue
			}
			newFiles = append(newFiles, m.ID)
		}
		if fhs := c.R.MultipartForm.File["import"]; len(fhs) > 0 && it.Kind != KindFile && it.Kind != KindTest {
			text, n, err := c.importDocument(fhs[0], co.ID)
			if err != nil {
				notes = append(notes, "импорт «"+fhs[0].Filename+"»: "+err.Error())
			} else {
				notes = append(notes, n...)
				if c.F("import_mode") == "append" && strings.TrimSpace(body) != "" {
					body = strings.TrimRight(body, "\n") + "\n\n" + text
				} else {
					body = text
				}
			}
		}
	}
	if len(body) > 2<<20 {
		c.Redirect(back, "!Текст слишком велик.")
		return
	}
	var dropped []string
	err := c.Items.Update(it.ID, func(x *Item) error {
		x.Title, x.SectionID, x.Visible, x.Updated = title, c.F("section"), c.F("visible") == "1", time.Now()
		switch x.Kind {
		case KindLesson:
			x.Body = body
			x.FileIDs = append(x.FileIDs, newFiles...)
		case KindFile:
			if len(newFiles) > 0 {
				dropped, x.FileIDs = x.FileIDs, newFiles[:1]
			}
			x.Body = body
		case KindTask:
			x.Body = body
			x.FileIDs = append(x.FileIDs, newFiles...)
			x.MaxPoints = atof(c.F("max_points"), 10, 0, 1000)
			x.CloseAt = c.parseLocal(c.F("close_at"))
		case KindTest:
			x.MaxPoints = atof(c.F("max_points"), 10, 0, 1000)
			x.OpenAt, x.CloseAt = c.parseLocal(c.F("open_at")), c.parseLocal(c.F("close_at"))
			x.TimeLimit = atoi(c.F("time_limit"), 0, 0, 600)
			x.MaxAttempts = atoi(c.F("max_attempts"), 1, 0, 100)
			x.PoolSize = atoi(c.F("pool_size"), 0, 0, 1000)
			x.PassPercent = atoi(c.F("pass_percent"), 50, 0, 100)
			x.Shuffle, x.ShowAnswers = c.F("shuffle") == "1", c.F("show_answers") == "1"
			if x.Visible && len(x.Questions) == 0 {
				x.Visible = false
				notes = append(notes, "тест без вопросов не может быть опубликован")
			}
		}
		return nil
	})
	if err != nil {
		c.Redirect(back, "!Не удалось сохранить: "+err.Error())
		return
	}
	for _, id := range dropped {
		c.removeFile(id)
	}
	msg := "Сохранено."
	if len(notes) > 0 {
		msg = "!Сохранено с замечаниями: " + strings.Join(notes, "; ")
	}
	if c.F("then") == "view" && len(notes) == 0 {
		back = "/items/" + it.ID
	}
	c.Redirect(back, msg)
}

func (c *Ctx) deleteItem(it *Item) {
	for _, id := range it.FileIDs {
		c.removeFile(id)
	}
	for _, x := range c.Attempts.Find(func(x *Attempt) bool { return x.ItemID == it.ID }) {
		c.Attempts.Delete(x.ID)
	}
	for _, x := range c.Subs.Find(func(x *Submission) bool { return x.ItemID == it.ID }) {
		c.removeFile(x.FileID)
		c.Subs.Delete(x.ID)
	}
	for _, x := range c.Progress.Find(func(x *Progress) bool { return x.ItemID == it.ID }) {
		c.Progress.Delete(x.ID)
	}
	c.Items.Delete(it.ID)
}

func (c *Ctx) itemDelete() {
	it, co, ok := c.item(true)
	if !ok {
		return
	}
	c.deleteItem(it)
	c.Log("удаление материала", co.Title+" / "+it.Title, true, kindName(it.Kind))
	c.Redirect("/courses/"+co.ID, "Удалено.")
}

func (c *Ctx) itemMove() {
	it, co, ok := c.item(true)
	if !ok {
		return
	}
	var same []Item
	for _, x := range c.courseItems(co.ID, false) {
		if x.SectionID == it.SectionID {
			same = append(same, x)
		}
	}
	for i := range same {
		if same[i].ID != it.ID {
			continue
		}
		j := i + 1
		if c.F("dir") == "up" {
			j = i - 1
		}
		if j >= 0 && j < len(same) {
			a, b := same[i].Order, same[j].Order
			c.Items.Update(same[i].ID, func(x *Item) error { x.Order = b; return nil })
			c.Items.Update(same[j].ID, func(x *Item) error { x.Order = a; return nil })
		}
	}
	c.Redirect("/courses/"+co.ID, "")
}

func (c *Ctx) itemDetach() {
	it, _, ok := c.item(true)
	if !ok {
		return
	}
	fid := c.F("file")
	found := false
	c.Items.Update(it.ID, func(x *Item) error {
		for i, id := range x.FileIDs {
			if id == fid {
				x.FileIDs = append(x.FileIDs[:i], x.FileIDs[i+1:]...)
				found = true
				break
			}
		}
		return nil
	})
	if found {
		c.removeFile(fid)
	}
	c.Redirect("/items/"+it.ID+"/edit", "Файл удалён.")
}

// ---------- файлы ----------

func cleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	if r := []rune(name); len(r) > 150 {
		name = string(r[len(r)-150:])
	}
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// checkContent сверяет начало файла с заявленным расширением и отсекает исполняемые файлы.
func checkContent(ext string, head []byte) error {
	has := func(p string) bool { return bytes.HasPrefix(head, []byte(p)) }
	if has("\x7fELF") || has("MZ") || has("#!") || has("\xca\xfe\xba\xbe") || has("\xcf\xfa\xed\xfe") {
		return errors.New("исполняемые файлы запрещены")
	}
	okSig := true
	switch ext {
	case "pdf":
		okSig = has("%PDF-")
	case "png":
		okSig = has("\x89PNG")
	case "jpg", "jpeg":
		okSig = has("\xff\xd8\xff")
	case "gif":
		okSig = has("GIF8")
	case "webp":
		okSig = has("RIFF") && len(head) >= 12 && string(head[8:12]) == "WEBP"
	case "docx", "pptx", "xlsx", "odt", "odp", "ods", "zip":
		okSig = has("PK")
	case "7z":
		okSig = has("7z\xbc\xaf")
	}
	if !okSig {
		return errors.New("содержимое файла не соответствует расширению ." + ext)
	}
	return nil
}

func (a *App) filePath(id string) string {
	return filepath.Join(a.Cfg.DataDir, "files", id[:2], id)
}

// storeFile проверяет и сохраняет файл в файловом хранилище.
func (a *App) storeFile(r io.Reader, name, owner, courseID, scope string) (FileMeta, error) {
	name = cleanName(name)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "htm" {
		ext = "html"
	}
	if !contains(strings.Fields(strings.ToLower(a.S().AllowedExt)), ext) {
		return FileMeta{}, fmt.Errorf("тип файла .%s не разрешён", ext)
	}
	head := make([]byte, 16)
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	if n == 0 {
		return FileMeta{}, errors.New("файл пуст")
	}
	if err := checkContent(ext, head); err != nil {
		return FileMeta{}, err
	}
	m := FileMeta{ID: secure.NewID(), Name: name, Ext: ext, OwnerID: owner, CourseID: courseID, Scope: scope, Created: time.Now()}
	p := a.filePath(m.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return m, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return m, err
	}
	h := sha256.New()
	m.Size, err = io.Copy(io.MultiWriter(f, h), io.MultiReader(bytes.NewReader(head), r))
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		os.Remove(p)
		return m, err
	}
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	return m, a.Files.Put(m.ID, m)
}

func (c *Ctx) saveUpload(fh *multipart.FileHeader, courseID, scope string) (FileMeta, error) {
	f, err := fh.Open()
	if err != nil {
		return FileMeta{}, err
	}
	defer f.Close()
	m, err := c.storeFile(f, fh.Filename, c.User.ID, courseID, scope)
	if err != nil {
		c.Log("загрузка файла", fh.Filename, false, err.Error())
		return m, err
	}
	c.Log("загрузка файла", m.Name, true, fmt.Sprintf("%s, sha256=%s", fmtSize(m.Size), m.SHA256[:16]))
	return m, nil
}

func (a *App) removeFile(id string) {
	if len(id) < 2 {
		return
	}
	if _, ok := a.Files.Get(id); ok {
		os.Remove(a.filePath(id))
		a.Files.Delete(id)
	}
}

// importDocument преобразует загруженный документ (DOCX, Markdown, текст) в текст лекции.
func (c *Ctx) importDocument(fh *multipart.FileHeader, courseID string) (string, []string, error) {
	f, err := fh.Open()
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	switch strings.ToLower(filepath.Ext(fh.Filename)) {
	case ".docx":
		return content.ImportDOCX(f, fh.Size, func(name string, data []byte) (string, error) {
			m, err := c.storeFile(bytes.NewReader(data), name, c.User.ID, courseID, "course")
			if err != nil {
				return "", err
			}
			return "/files/" + m.ID, nil
		})
	case ".md", ".txt":
		b, err := io.ReadAll(io.LimitReader(f, 2<<20))
		if err != nil {
			return "", nil, err
		}
		return strings.ToValidUTF8(string(b), "?"), nil, nil
	}
	return "", nil, errors.New("для импорта в текст поддерживаются DOCX, MD и TXT; PDF и презентации прикрепляйте как файлы")
}

var mimeTypes = map[string]string{
	"pdf": "application/pdf", "png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp",
	"mp4": "video/mp4", "webm": "video/webm", "mp3": "audio/mpeg", "ogg": "audio/ogg", "html": "text/html; charset=utf-8",
}

func (c *Ctx) fileServe() {
	m, ok := c.Files.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Файл не найден.")
		return
	}
	allowed := false
	switch m.Scope {
	case "course", "sub":
		if co, ok := c.Courses.Get(m.CourseID); ok {
			allowed = c.canManage(&co) || (m.Scope == "course" && c.canView(&co))
		}
		allowed = allowed || m.OwnerID == c.User.ID
	case "pf":
		allowed = m.OwnerID == c.User.ID || c.User.Role != RoleStudent
	}
	if !allowed {
		c.Deny()
		return
	}
	f, err := os.Open(c.filePath(m.ID))
	if err != nil {
		c.Fail(http.StatusNotFound, "Файл не найден.")
		return
	}
	defer f.Close()
	h := c.W.Header()
	ct, inline := mimeTypes[m.Ext]
	if !inline || c.R.URL.Query().Get("dl") == "1" {
		ct, inline = "application/octet-stream", false
	}
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", disp+"; filename*=UTF-8''"+url.PathEscape(m.Name))
	h.Set("Cache-Control", "private, max-age=300")
	if m.Ext == "html" {
		// загруженный HTML изолируется: без сценариев, без доступа к сеансу и к сети
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'")
	} else if m.Ext == "pdf" {
		h.Del("Content-Security-Policy") // встроенному просмотрщику PDF политика страницы не нужна и может мешать
	} else {
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}
	http.ServeContent(c.W, c.R, "", m.Created, f)
}

// ---------- объявления и обсуждения ----------

type newsView struct {
	News   News
	Author string
	Course string
	Body   template.HTML
}

func (a *App) newsView(pred func(*News) bool, limit int) []newsView {
	list := sortBy(a.NewsC.Find(pred), func(x, y *News) bool { return x.Created.After(y.Created) })
	if len(list) > limit {
		list = list[:limit]
	}
	var out []newsView
	for _, n := range list {
		v := newsView{News: n, Author: a.userName(n.AuthorID), Body: template.HTML(content.Render(n.Body))}
		if co, ok := a.Courses.Get(n.CourseID); ok {
			v.Course = co.Title
		}
		out = append(out, v)
	}
	return out
}

func (c *Ctx) newsCreate() {
	courseID := c.R.PathValue("id")
	back := "/"
	if courseID != "" {
		if _, ok := c.course(courseID, true); !ok {
			return
		}
		back = "/courses/" + courseID + "?tab=news"
	}
	title, body := c.F("title"), c.F("body")
	if title == "" || len(body) > 20000 {
		c.Redirect(back, "!Укажите заголовок объявления.")
		return
	}
	nid := secure.NewID()
	c.NewsC.Put(nid, News{ID: nid, CourseID: courseID, AuthorID: c.User.ID, Title: title, Body: body, Created: time.Now()})
	c.Redirect(back, "Объявление опубликовано.")
}

func (c *Ctx) newsDelete() {
	id := c.R.PathValue("id")
	n, ok := c.NewsC.Get(id)
	if !ok {
		c.Redirect("/", "")
		return
	}
	if n.CourseID == "" {
		if c.User.Role != RoleAdmin {
			c.Deny()
			return
		}
	} else if _, ok := c.course(n.CourseID, true); !ok {
		return
	}
	c.NewsC.Delete(id)
	c.Redirect(c.Back("/"), "Объявление удалено.")
}

func (c *Ctx) postCreate() {
	co, ok := c.course(c.R.PathValue("id"), false)
	if !ok {
		return
	}
	body := c.F("body")
	if body == "" || len(body) > 5000 {
		c.Redirect("/courses/"+co.ID+"?tab=talk", "!Сообщение пустое или слишком длинное.")
		return
	}
	pid := secure.NewID()
	c.Posts.Put(pid, Post{ID: pid, CourseID: co.ID, AuthorID: c.User.ID, Body: body, Created: time.Now()})
	c.Redirect("/courses/"+co.ID+"?tab=talk", "")
}

func (c *Ctx) postDelete() {
	id := c.R.PathValue("id")
	p, ok := c.Posts.Get(id)
	if !ok {
		c.Redirect("/", "")
		return
	}
	co, ok := c.course(p.CourseID, false)
	if !ok {
		return
	}
	if p.AuthorID != c.User.ID && !c.canManage(co) {
		c.Deny()
		return
	}
	c.Posts.Delete(id)
	c.Redirect("/courses/"+co.ID+"?tab=talk", "Сообщение удалено.")
}
