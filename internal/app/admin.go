package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"kafedra/internal/secure"
)

var reLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,39}$`)

// CreateUser создаёт учётную запись с временным паролем (смена при первом входе).
func (a *App) CreateUser(login, name, email, role, groupID, password string) (User, error) {
	if err := a.checkUserEmail(strings.TrimSpace(email), ""); err != nil {
		return User{}, err
	}
	return a.createUser(login, name, email, role, groupID, a.Keys.HashPassword(password))
}

func (a *App) checkUserEmail(email, excludeID string) error {
	if email == "" {
		return nil
	}
	if !validEmail(email) {
		return errors.New("некорректный адрес электронной почты")
	}
	if a.Users.Count(func(u *User) bool { return u.ID != excludeID && strings.EqualFold(u.Email, email) }) != 0 {
		return errors.New("адрес электронной почты уже используется")
	}
	return nil
}

// createUser создаёт учётную запись с заранее вычисленным хешем пароля.
func (a *App) createUser(login, name, email, role, groupID, passHash string) (User, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	name = strings.TrimSpace(name)
	if !reLogin.MatchString(login) {
		return User{}, errors.New("логин: 3–40 символов, латинские буквы, цифры, точка, дефис, подчёркивание")
	}
	if name == "" || roleName(role) == "" {
		return User{}, errors.New("укажите ФИО и роль")
	}
	if _, dup := a.Users.First(func(u *User) bool { return u.Login == login }); dup {
		return User{}, fmt.Errorf("логин %q уже занят", login)
	}
	if role != RoleStudent {
		groupID = ""
	} else if _, ok := a.Groups.Get(groupID); !ok {
		groupID = ""
	}
	u := User{ID: secure.NewID(), Login: login, FullName: name, Email: strings.TrimSpace(email), Role: role, GroupID: groupID,
		PassHash: passHash, PassChanged: time.Now(), MustChange: true, Created: time.Now()}
	return u, a.Users.Put(u.ID, u)
}

func (c *Ctx) adminUsers() {
	q := strings.ToLower(c.R.URL.Query().Get("q"))
	role, group := c.R.URL.Query().Get("role"), c.R.URL.Query().Get("group")
	list := c.Users.Find(func(u *User) bool {
		return (role == "" || u.Role == role) && (group == "" || u.GroupID == group) &&
			(q == "" || strings.Contains(strings.ToLower(u.FullName), q) || strings.Contains(u.Login, q))
	})
	sortBy(list, func(x, y *User) bool { return x.FullName < y.FullName })
	total := len(list)
	if len(list) > 500 {
		list = list[:500]
	}
	c.Render("admin_users", map[string]any{"Users": list, "Total": total, "Q": q, "Role": role, "Group": group,
		"Groups": sortBy(c.Groups.All(), func(x, y *Group) bool { return x.Name < y.Name }), "GroupNames": c.groupNames(), "Now": time.Now()})
}

func (c *Ctx) adminUserCreate() {
	pw := secure.GeneratePassword(max(c.S().PwMinLen, 14))
	u, err := c.CreateUser(c.F("login"), c.F("name"), c.F("email"), c.F("role"), c.F("group"), pw)
	if err != nil {
		c.Log("создание учётной записи", c.F("login"), false, err.Error())
		c.Redirect("/admin/users", "!"+err.Error())
		return
	}
	c.Log("создание учётной записи", u.Login, true, "роль: "+roleName(u.Role))
	c.userPage(&u, pw)
}

func (c *Ctx) userPage(u *User, tempPassword string) {
	c.Render("admin_user", map[string]any{"U": u, "TempPassword": tempPassword,
		"Groups":   sortBy(c.Groups.All(), func(x, y *Group) bool { return x.Name < y.Name }),
		"Locked":   time.Now().Before(u.LockedUntil),
		"Sessions": c.Sessions.Count(func(s *Session) bool { return s.UserID == u.ID })})
}

func (c *Ctx) adminUser() {
	u, ok := c.Users.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Пользователь не найден.")
		return
	}
	c.userPage(&u, "")
}

func (c *Ctx) lastAdmin(u *User) bool {
	return u.Role == RoleAdmin && !u.Disabled && c.Users.Count(func(x *User) bool { return x.Role == RoleAdmin && !x.Disabled }) <= 1
}

func (c *Ctx) adminUserSave() {
	u, ok := c.Users.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Пользователь не найден.")
		return
	}
	back := "/admin/users/" + u.ID
	role, disabled := c.F("role"), c.F("disabled") == "1"
	if roleName(role) == "" || c.F("name") == "" {
		c.Redirect(back, "!Укажите ФИО и роль.")
		return
	}
	if (role != u.Role || disabled) && (u.ID == c.User.ID || c.lastAdmin(&u)) {
		c.Redirect(back, "!Нельзя отключить или понизить собственную учётную запись либо последнего администратора.")
		return
	}
	var changes []string
	groupID := ""
	if _, ok := c.Groups.Get(c.F("group")); ok && role == RoleStudent {
		groupID = c.F("group")
	}
	email := c.F("email")
	if email != u.Email {
		if err := c.checkUserEmail(email, u.ID); err != nil {
			c.Redirect(back, "!"+err.Error())
			return
		}
	}
	resetMFA := c.F("reset_mfa") == "1"
	if disabled || role != u.Role || resetMFA {
		if err := c.revokeSessions(u.ID, ""); err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось завершить сеансы пользователя.")
			return
		}
	}
	if err := c.Users.Update(u.ID, func(x *User) error {
		if x.Role != role {
			changes = append(changes, "роль: "+roleName(x.Role)+" → "+roleName(role))
		}
		if x.Disabled != disabled {
			changes = append(changes, fmt.Sprintf("отключена: %v", disabled))
		}
		x.FullName, x.Email, x.Role, x.Disabled = c.F("name"), email, role, disabled
		x.GroupID = groupID
		if c.F("unlock") == "1" {
			x.LockedUntil, x.Failed = time.Time{}, 0
			changes = append(changes, "снята блокировка")
		}
		if resetMFA {
			x.TOTPEnabled, x.TOTPSecret, x.TOTPStep = false, "", 0
			changes = append(changes, "сброшен второй фактор")
		}
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось изменить учётную запись.")
		return
	}
	c.Log("изменение учётной записи", u.Login, true, strings.Join(changes, "; "))
	c.Redirect(back, "Учётная запись сохранена.")
}

func (c *Ctx) adminUserReset() {
	u, ok := c.Users.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Пользователь не найден.")
		return
	}
	pw := secure.GeneratePassword(max(c.S().PwMinLen, 14))
	if err := c.revokeSessions(u.ID, ""); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось завершить сеансы пользователя.")
		return
	}
	if err := c.setPassword(u.ID, pw, secure.Policy{}, c.S().PwHistory, true); err != nil {
		c.Redirect("/admin/users/"+u.ID, "!"+err.Error())
		return
	}
	c.Log("сброс пароля администратором", u.Login, true, "")
	u, _ = c.Users.Get(u.ID)
	c.userPage(&u, pw)
}

// DeleteUser удаляет учётную запись и связанные с ней персональные данные.
func (a *App) DeleteUser(id string) {
	for _, x := range a.Sessions.Find(func(x *Session) bool { return x.UserID == id }) {
		a.Sessions.Delete(x.ID)
	}
	for _, x := range a.Attempts.Find(func(x *Attempt) bool { return x.UserID == id }) {
		a.Attempts.Delete(x.ID)
	}
	// сообщения чатов, исключения и загруженные заявления — тоже персональные данные
	for _, x := range a.Posts.Find(func(x *Post) bool { return x.AuthorID == id }) {
		if a.Posts.Delete(x.ID) == nil {
			a.removeFile(x.FileID)
		}
	}
	for _, x := range a.ChatBans.Find(func(x *ChatBan) bool { return x.UserID == id }) {
		a.ChatBans.Delete(x.ID)
	}
	for _, x := range a.CertApps.Find(func(x *CertApplication) bool { return x.UserID == id }) {
		if a.CertApps.Delete(x.ID) == nil {
			a.removeFile(x.FileID)
		}
	}
	for _, x := range a.Certs.Find(func(x *Certificate) bool { return x.UserID == id }) {
		if err := a.Certs.Delete(x.ID); err == nil {
			a.removeFile(x.PDFFileID)
		}
	}
	for _, x := range a.Subs.Find(func(x *Submission) bool { return x.UserID == id }) {
		a.removeFile(x.FileID)
		a.Subs.Delete(x.ID)
	}
	for _, x := range a.Grades.Find(func(x *Grade) bool { return x.UserID == id }) {
		a.Grades.Delete(x.ID)
	}
	for _, x := range a.Attend.Find(func(x *Attendance) bool { return x.UserID == id }) {
		a.Attend.Delete(x.ID)
	}
	for _, x := range a.Progress.Find(func(x *Progress) bool { return x.UserID == id }) {
		a.Progress.Delete(x.ID)
	}
	for _, x := range a.Notices.Find(func(x *Notice) bool { return x.UserID == id }) {
		a.Notices.Delete(x.ID)
	}
	for _, x := range a.Portfolio.Find(func(x *PortfolioEntry) bool { return x.UserID == id }) {
		a.removeFile(x.FileID)
		a.Portfolio.Delete(x.ID)
	}
	for _, co := range a.Courses.Find(func(co *Course) bool { return contains(co.TeacherIDs, id) }) {
		a.Courses.Update(co.ID, func(x *Course) error {
			var keep []string
			for _, t := range x.TeacherIDs {
				if t != id {
					keep = append(keep, t)
				}
			}
			x.TeacherIDs = keep
			return nil
		})
	}
	a.Users.Delete(id)
}

func (c *Ctx) adminUserDelete() {
	u, ok := c.Users.Get(c.R.PathValue("id"))
	if !ok {
		c.Redirect("/admin/users", "")
		return
	}
	if u.ID == c.User.ID || c.lastAdmin(&u) {
		c.Redirect("/admin/users/"+u.ID, "!Нельзя удалить собственную учётную запись либо последнего администратора.")
		return
	}
	c.DeleteUser(u.ID)
	c.Log("удаление учётной записи", u.Login, true, "удалены связанные персональные данные")
	c.Redirect("/admin/users", "Учётная запись и связанные данные удалены.")
}

func (c *Ctx) adminUserImport() {
	rows, err := c.readUserImport("")
	if err != nil {
		c.Redirect("/admin/users", "!"+err.Error())
		return
	}
	if len(rows) == 0 || len(rows) > 5000 {
		c.Redirect("/admin/users", "!В файле должно быть от 1 до 5000 строк с данными.")
		return
	}
	markDuplicateEmails(rows)
	created, allowed := c.bulkCreate(rows, RoleAdmin, "")
	if !allowed {
		c.Deny()
		return
	}
	c.Log("импорт учётных записей", "", created == len(rows), fmt.Sprintf("строк: %d, создано: %d", len(rows), created))
	c.sendXLSX("Результат импорта пользователей.xlsx", importResultSheet(rows))
}

func (c *Ctx) adminGroups() {
	type row struct {
		Group Group
		Count int
	}
	var rows []row
	for _, g := range sortBy(c.Groups.All(), func(x, y *Group) bool { return x.Name < y.Name }) {
		rows = append(rows, row{g, c.Users.Count(func(u *User) bool { return u.GroupID == g.ID })})
	}
	c.Render("admin_groups", map[string]any{"Rows": rows})
}

func (c *Ctx) adminGroupSave() {
	name := c.F("name")
	if name == "" {
		c.Redirect("/admin/groups", "!Укажите название группы.")
		return
	}
	id := c.F("id")
	if dup, ok := c.Groups.First(func(g *Group) bool { return strings.EqualFold(g.Name, name) && g.ID != id }); ok {
		c.Redirect("/admin/groups", "!Группа «"+dup.Name+"» уже существует.")
		return
	}
	if _, ok := c.Groups.Get(id); !ok {
		id = secure.NewID()
	}
	c.Groups.Put(id, Group{ID: id, Name: name, Note: c.F("note")})
	c.Log("сохранение группы", name, true, "")
	c.Redirect("/admin/groups", "Группа сохранена.")
}

func (c *Ctx) adminGroupDelete() {
	id := c.R.PathValue("id")
	g, ok := c.Groups.Get(id)
	if !ok {
		c.Redirect("/admin/groups", "")
		return
	}
	if n := c.Users.Count(func(u *User) bool { return u.GroupID == id }); n > 0 {
		c.Redirect("/admin/groups", fmt.Sprintf("!В группе %d учащихся: сначала переведите их в другую группу.", n))
		return
	}
	for _, co := range c.Courses.Find(func(co *Course) bool { return contains(co.GroupIDs, id) }) {
		c.Courses.Update(co.ID, func(x *Course) error {
			var keep []string
			for _, gid := range x.GroupIDs {
				if gid != id {
					keep = append(keep, gid)
				}
			}
			x.GroupIDs = keep
			return nil
		})
	}
	c.dropChat(func(x *Post) bool { return x.GroupID == id }, "g-"+id)
	c.Groups.Delete(id)
	c.Log("удаление группы", g.Name, true, "")
	c.Redirect("/admin/groups", "Группа удалена.")
}

// ---------- настройки ----------

func (c *Ctx) adminSettings() { c.Render("admin_settings", map[string]any{"S": c.S()}) }

func (c *Ctx) adminSettingsSave() {
	old := c.S()
	s := old
	b := func(n string) bool { return c.F(n) == "1" }
	s.PwMinLen = atoi(c.F("pw_min_len"), 12, 8, 64)
	s.PwUpper, s.PwLower, s.PwDigit, s.PwSymbol = b("pw_upper"), b("pw_lower"), b("pw_digit"), b("pw_symbol")
	s.PwHistory = atoi(c.F("pw_history"), 5, 0, 24)
	s.PwMaxAgeDays = atoi(c.F("pw_max_age"), 90, 0, 730)
	s.LockAttempts = atoi(c.F("lock_attempts"), 5, 3, 20)
	s.LockMinutes = atoi(c.F("lock_minutes"), 15, 1, 1440)
	s.IPFailLimit = atoi(c.F("ip_fail_limit"), 100, 20, 100000)
	s.IdleMinutes = atoi(c.F("idle_minutes"), 30, 5, 480)
	s.SessionHours = atoi(c.F("session_hours"), 12, 1, 72)
	s.MaxSessions = atoi(c.F("max_sessions"), 3, 1, 20)
	if m := c.F("mfa_roles"); contains([]string{"none", "admin", "staff", "all"}, m) {
		s.MFARoles = m
	}
	s.Grade5 = atoi(c.F("grade5"), 85, 1, 100)
	s.Grade4 = atoi(c.F("grade4"), 70, 1, s.Grade5)
	s.Grade3 = atoi(c.F("grade3"), 50, 1, s.Grade4)
	s.RatingNames, s.TeacherCourses, s.TeacherImport = b("rating_names"), b("teacher_courses"), b("teacher_import")
	s.QuestRequired, s.CertNeedsApp = b("quest_required"), b("cert_needs_app")
	s.ChatMaxFileMB = atoi(c.F("chat_max_file"), 10, 1, 100)
	s.QuestEmail = ""
	if addr := c.F("quest_email"); addr != "" {
		if parsed, err := mail.ParseAddress(addr); err == nil && parsed.Address == addr && len(addr) <= 254 {
			s.QuestEmail = addr
		}
	}
	s.MaxUploadMB = atoi(c.F("max_upload"), 200, 1, 4096)
	var exts []string
	for _, e := range strings.Fields(strings.ToLower(c.F("allowed_ext"))) {
		e = strings.TrimPrefix(e, ".")
		if regexp.MustCompile(`^[a-z0-9]{1,8}$`).MatchString(e) && !contains([]string{"exe", "dll", "bat", "cmd", "sh", "msi", "com", "scr", "ps1", "vbs", "jar", "svg", "php"}, e) {
			exts = append(exts, e)
		}
	}
	s.AllowedExt = strings.Join(exts, " ")
	s.Banner = c.F("banner")
	s.TOTPSkew = atoi(c.F("totp_skew"), 1, 0, 10)
	saveErr := c.SetSettings(s)
	ob, _ := json.Marshal(old)
	nb, _ := json.Marshal(s)
	c.Log("изменение настроек системы", "", true, fmt.Sprintf("было: %s; стало: %s", ob, nb))
	if saveErr != nil {
		c.Redirect("/admin/settings", "!Настройки применены, но не записаны в файл конфигурации ("+saveErr.Error()+"). После перезапуска вернутся значения из файла — внесите их в config.json вручную.")
		return
	}
	c.Redirect("/admin/settings", "Настройки сохранены в файле конфигурации.")
}

// ---------- журнал событий ----------

func (c *Ctx) adminAudit() {
	dir := c.Cfg.AuditPath()
	files := secure.AuditFiles(dir)
	var months []string
	for _, f := range files {
		months = append(months, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "audit-"), ".log"))
	}
	sel := c.R.URL.Query().Get("m")
	if !contains(months, sel) && len(months) > 0 {
		sel = months[len(months)-1]
	}
	q := strings.ToLower(c.R.URL.Query().Get("q"))
	onlyFail := c.R.URL.Query().Get("fail") == "1"
	var rows []secure.AuditRecord
	total := 0
	if sel != "" {
		recs, err := secure.ReadAuditFile(filepath.Join(dir, "audit-"+sel+".log"))
		if err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось прочитать журнал аудита.")
			return
		}
		for i := len(recs) - 1; i >= 0; i-- {
			r := recs[i]
			if onlyFail && r.OK {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(r.Login+" "+r.Event+" "+r.Object+" "+r.Detail+" "+r.IP), q) {
				continue
			}
			total++
			if len(rows) < 500 {
				rows = append(rows, r)
			}
		}
	}
	c.Render("admin_audit", map[string]any{"Rows": rows, "Total": total, "Months": months, "Sel": sel, "Q": q, "Fail": onlyFail})
}

func (c *Ctx) adminAuditVerify() {
	n, err := c.Audit.Verify()
	if err != nil {
		c.Log("проверка целостности журнала событий", "", false, err.Error())
		c.Redirect("/admin/audit", "!"+err.Error())
		return
	}
	c.Log("проверка целостности журнала событий", "", true, fmt.Sprintf("проверено записей: %d", n))
	c.Redirect("/admin/audit", fmt.Sprintf("Целостность подтверждена, проверено записей: %d.", n))
}

// ---------- система ----------

func (c *Ctx) adminSystem() {
	type sv struct {
		S    Session
		User string
		Role string
	}
	var sess []sv
	for _, s := range sortBy(c.Sessions.All(), func(x, y *Session) bool { return x.Seen.After(y.Seen) }) {
		u, _ := c.Users.Get(s.UserID)
		sess = append(sess, sv{s, u.FullName, u.Role})
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	c.Render("admin_system", map[string]any{"Sessions": sess, "SessID": c.Sess.ID, "Go": runtime.Version(), "OS": runtime.GOOS + "/" + runtime.GOARCH,
		"Uptime": time.Since(c.started).Round(time.Second).String(), "BinSum": c.BinSum, "Mem": int64(ms.Sys),
		"Counts": map[string]int{"Пользователи": c.Users.Count(nil), "Курсы": c.Courses.Count(nil), "Материалы": c.Items.Count(nil),
			"Попытки тестов": c.Attempts.Count(nil), "Сданные работы": c.Subs.Count(nil), "Файлы": c.Files.Count(nil)}})
}

func (c *Ctx) adminSessionRevoke() {
	id := c.F("id")
	if s, ok := c.Sessions.Get(id); ok && id != c.Sess.ID {
		c.Sessions.Delete(id)
		c.Log("принудительное завершение сеанса", c.userName(s.UserID), true, "")
	}
	c.Redirect("/admin/system", "Сеанс завершён.")
}
