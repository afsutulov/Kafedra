package app

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"kafedra/internal/secure"
	"kafedra/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// App — экземпляр приложения.
type App struct {
	Cfg     Config
	Version string
	DB      *store.DB
	Keys    *secure.Keys
	Audit   *secure.Audit
	Loc     *time.Location

	Users     *store.Coll[User]
	Groups    *store.Coll[Group]
	Courses   *store.Coll[Course]
	Items     *store.Coll[Item]
	Attempts  *store.Coll[Attempt]
	Subs      *store.Coll[Submission]
	Columns   *store.Coll[Column]
	Grades    *store.Coll[Grade]
	Events    *store.Coll[Event]
	Attend    *store.Coll[Attendance]
	NewsC     *store.Coll[News]
	Posts     *store.Coll[Post]
	Portfolio *store.Coll[PortfolioEntry]
	Progress  *store.Coll[Progress]
	Notices   *store.Coll[Notice]
	Sessions  *store.Coll[Session]
	Files     *store.Coll[FileMeta]
	setMu     sync.RWMutex

	tpl     map[string]*template.Template
	started time.Time
	BinSum  string

	limMu sync.Mutex
	lim   map[string][]time.Time
}

func must[T any](c *store.Coll[T], err error) *store.Coll[T] {
	if err != nil {
		panic(err)
	}
	return c
}

// New открывает хранилище и подготавливает приложение.
func New(cfg Config, version string) (a *App, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("инициализация: %v", r)
		}
	}()
	if cfg.Timezone == "" {
		cfg.Timezone = "Europe/Moscow"
	}
	if cfg.PortalName == "" {
		cfg.PortalName = "Учебный портал"
	}
	if cfg.Settings.PwMinLen == 0 { // конфигурация создана в коде без раздела settings
		cfg.Settings = DefaultSettings()
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("часовой пояс %q: %w", cfg.Timezone, err)
	}
	for _, d := range []string{cfg.DataPath(), filepath.Join(cfg.DataPath(), "files"), filepath.Join(cfg.DataPath(), "backup")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	keys, err := secure.LoadOrCreateKeys(filepath.Join(cfg.DataPath(), "master.key"))
	if err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataPath(), "db"))
	if err != nil {
		return nil, err
	}
	audit, err := secure.OpenAudit(cfg.AuditPath())
	if err != nil {
		return nil, err
	}
	a = &App{Cfg: cfg, Version: version, DB: db, Keys: keys, Audit: audit, Loc: loc, started: time.Now(), lim: map[string][]time.Time{}}
	a.Users = must(store.Register[User](db, "users"))
	a.Groups = must(store.Register[Group](db, "groups"))
	a.Courses = must(store.Register[Course](db, "courses"))
	a.Items = must(store.Register[Item](db, "items"))
	a.Attempts = must(store.Register[Attempt](db, "attempts"))
	a.Subs = must(store.Register[Submission](db, "submissions"))
	a.Columns = must(store.Register[Column](db, "columns"))
	a.Grades = must(store.Register[Grade](db, "grades"))
	a.Events = must(store.Register[Event](db, "events"))
	a.Attend = must(store.Register[Attendance](db, "attendance"))
	a.NewsC = must(store.Register[News](db, "news"))
	a.Posts = must(store.Register[Post](db, "posts"))
	a.Portfolio = must(store.Register[PortfolioEntry](db, "portfolio"))
	a.Progress = must(store.Register[Progress](db, "progress"))
	a.Notices = must(store.Register[Notice](db, "notices"))
	a.Sessions = must(store.Register[Session](db, "sessions"))
	a.Files = must(store.Register[FileMeta](db, "files"))
	for _, w := range db.Warnings {
		a.Audit.Log(secure.AuditRecord{Event: "хранилище: восстановление после сбоя", Detail: w, OK: true})
	}
	if exe, err := os.Executable(); err == nil {
		if f, err := os.Open(exe); err == nil {
			h := sha256.New()
			io.Copy(h, f)
			f.Close()
			a.BinSum = hex.EncodeToString(h.Sum(nil))
		}
	}
	if err := a.loadTemplates(); err != nil {
		return nil, err
	}
	return a, nil
}

// Close корректно завершает работу с хранилищем.
func (a *App) Close() {
	a.DB.Close()
	a.Audit.Close()
}

// S возвращает текущие настройки (раздел settings файла конфигурации).
func (a *App) S() Settings {
	a.setMu.RLock()
	defer a.setMu.RUnlock()
	return a.Cfg.Settings
}

// SetSettings применяет настройки и сохраняет их в файл конфигурации.
// Если файл недоступен для записи, настройки действуют до перезапуска, а ошибка возвращается.
func (a *App) SetSettings(s Settings) error {
	a.setMu.Lock()
	defer a.setMu.Unlock()
	a.Cfg.Settings = s
	if a.Cfg.path == "" {
		return nil
	}
	return a.Cfg.Save(a.Cfg.path)
}

// ---------- шаблоны ----------

func (a *App) loadTemplates() error {
	funcs := template.FuncMap{
		"dt":      func(t time.Time) string { return fmtTime(t, a.Loc, "02.01.2006 15:04") },
		"date":    func(t time.Time) string { return fmtTime(t, a.Loc, "02.01.2006") },
		"clock":   func(t time.Time) string { return fmtTime(t, a.Loc, "15:04") },
		"dtlocal": func(t time.Time) string { return fmtTime(t, a.Loc, "2006-01-02T15:04") },
		"num":     fmtNum,
		"size":    fmtSize,
		"role":    roleName,
		"kind":    kindName,
		"evkind":  func(k string) string { return eventKinds[k] },
		"has":     func(list []string, v string) bool { return contains(list, v) },
		"hasInt": func(list []int, v int) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
		"add": func(a, b int) int { return a + b },
		"pct": func(a, b any) int {
			x, y := toF(a), toF(b)
			if y <= 0 {
				return 0
			}
			return int(x/y*100 + 0.5)
		},
		"seq": func(n int) []int {
			s := make([]int, n)
			for i := range s {
				s[i] = i
			}
			return s
		},
		"join":  strings.Join,
		"lines": func(s []string) string { return strings.Join(s, "\n") },
		"now":   time.Now,
		"zero":  func(t time.Time) bool { return t.IsZero() },
		"active": func(path, u string) bool {
			if u == "/" {
				return path == "/"
			}
			return strings.HasPrefix(path, u)
		},
		"pt": func(m map[string]float64, k string) string {
			if v, ok := m[k]; ok {
				return fmtNum(v)
			}
			return ""
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
	}
	a.tpl = map[string]*template.Template{}
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(filepath.Base(p), ".html")
		if name == "base" {
			continue
		}
		t, err := template.New("base.html").Funcs(funcs).ParseFS(assets, "templates/base.html", p)
		if err != nil {
			return fmt.Errorf("шаблон %s: %w", name, err)
		}
		a.tpl[name] = t
	}
	return nil
}

func fmtTime(t time.Time, loc *time.Location, layout string) string {
	if t.IsZero() {
		return ""
	}
	return t.In(loc).Format(layout)
}

func fmtNum(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return strings.Replace(s, ".", ",", 1)
}

func fmtSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d КБ", n>>10)
	}
	return fmt.Sprintf("%d Б", n)
}

func roleName(r string) string {
	return map[string]string{RoleAdmin: "Администратор", RoleTeacher: "Преподаватель", RoleStudent: "Учащийся"}[r]
}

func kindName(k string) string {
	return map[string]string{KindLesson: "Лекция", KindFile: "Файл", KindTest: "Тест", KindTask: "Задание"}[k]
}

var eventKinds = map[string]string{"lec": "Лекция", "prac": "Практика", "lab": "Лабораторная", "sem": "Семинар", "cons": "Консультация", "exam": "Экзамен / зачёт"}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------- контекст запроса ----------

// Ctx — контекст обработки запроса.
type Ctx struct {
	*App
	W    http.ResponseWriter
	R    *http.Request
	User *User
	Sess *Session
	IP   string
}

type navItem struct{ URL, Title string }

func (c *Ctx) nav() []navItem {
	switch c.User.Role {
	case RoleAdmin:
		return []navItem{{"/", "Главная"}, {"/admin/users", "Пользователи"}, {"/admin/groups", "Группы"}, {"/courses", "Курсы"},
			{"/schedule", "Расписание"}, {"/rating", "Рейтинг"}, {"/admin/settings", "Настройки"}, {"/admin/audit", "Журнал событий"},
			{"/admin/system", "Система"}, {"/profile", "Профиль"}}
	case RoleTeacher:
		return []navItem{{"/", "Главная"}, {"/courses", "Мои курсы"}, {"/schedule", "Расписание"}, {"/rating", "Рейтинг"}, {"/profile", "Профиль"}}
	}
	return []navItem{{"/", "Главная"}, {"/courses", "Мои курсы"}, {"/schedule", "Расписание"}, {"/grades", "Оценки"},
		{"/rating", "Рейтинг"}, {"/portfolio", "Портфолио"}, {"/profile", "Профиль"}}
}

// Render выводит страницу name с данными data.
func (c *Ctx) Render(name string, data map[string]any) {
	t := c.tpl[name]
	if t == nil {
		http.Error(c.W, "шаблон не найден", 500)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Cfg"] = c.Cfg
	data["Product"], data["Vendor"] = ProductName, Vendor
	data["Version"] = c.Version
	data["Path"] = c.R.URL.Path
	data["Year"] = time.Now().Year()
	if c.User != nil {
		data["Me"] = c.User
		data["CSRF"] = c.Sess.CSRF
		data["Nav"] = c.nav()
		uid := c.User.ID
		data["Unread"] = c.Notices.Count(func(n *Notice) bool { return n.UserID == uid && !n.Read })
	}
	if ck, err := c.R.Cookie("flash"); err == nil {
		if v, err := url.QueryUnescape(ck.Value); err == nil && len(v) > 1 {
			data["FlashKind"], data["Flash"] = v[:1], v[1:]
		}
		http.SetCookie(c.W, &http.Cookie{Name: "flash", Path: "/", MaxAge: -1})
	}
	c.W.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(c.W, data); err != nil {
		log.Printf("шаблон %s: %v", name, err)
	}
}

// Redirect перенаправляет с необязательным сообщением (префикс "!" — ошибка).
func (c *Ctx) Redirect(to, msg string) {
	if msg != "" {
		kind := "i"
		if strings.HasPrefix(msg, "!") {
			kind, msg = "e", msg[1:]
		}
		http.SetCookie(c.W, &http.Cookie{Name: "flash", Value: url.QueryEscape(kind + msg), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	http.Redirect(c.W, c.R, to, http.StatusSeeOther)
}

// Fail выводит страницу ошибки.
func (c *Ctx) Fail(code int, msg string) {
	c.W.WriteHeader(code)
	if c.User == nil {
		fmt.Fprintln(c.W, msg)
		return
	}
	c.Render("error", map[string]any{"Code": code, "Msg": msg})
}

// Deny регистрирует попытку несанкционированного доступа и отказывает.
func (c *Ctx) Deny() {
	c.Log("отказ в доступе", c.R.Method+" "+c.R.URL.Path, false, "")
	c.Fail(http.StatusForbidden, "Недостаточно прав для выполнения операции.")
}

// Log добавляет запись в журнал событий безопасности.
func (c *Ctx) Log(event, object string, ok bool, detail string) {
	r := secure.AuditRecord{Event: event, Object: object, OK: ok, Detail: detail, IP: c.IP}
	if c.User != nil {
		r.UserID, r.Login = c.User.ID, c.User.Login
	}
	c.Audit.Log(r)
}

func (c *Ctx) F(name string) string { return strings.TrimSpace(c.R.FormValue(name)) }

// Back возвращает адрес страницы, с которой пришёл запрос (только локальный).
func (c *Ctx) Back(def string) string {
	if u, err := url.Parse(c.R.Referer()); err == nil && c.R.Referer() != "" && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") {
		return u.RequestURI()
	}
	return def
}

// fromProxy сообщает, что запрос пришёл от доверенного внешнего веб-сервера.
func (a *App) fromProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	for _, p := range strings.Split(a.Cfg.TrustedProxy, ",") {
		if p = strings.TrimSpace(p); p != "" && p == host {
			return true
		}
	}
	return false
}

func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if a.fromProxy(r) {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
			return ip.String()
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			p := strings.Split(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(p[len(p)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}

// isHTTPS сообщает, что пользователь работает по HTTPS: программа принимает TLS сама,
// либо TLS завершается на внешнем веб-сервере (external_url с https или заголовок от доверенного прокси).
func (a *App) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if _, https := a.Cfg.externalHost(); https {
		return true
	}
	return a.fromProxy(r) && r.Header.Get("X-Forwarded-Proto") == "https"
}

// ---------- маршрутизация и защитная обвязка ----------

const (
	anyone = "" // доступ без входа
	authed = "*"
	staff  = "admin,teacher"
)

type handler func(c *Ctx)

func (a *App) secHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cache-Control", "no-store")
	if a.isHTTPS(r) {
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

func (a *App) wrap(roles string, big bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &Ctx{App: a, W: w, R: r, IP: a.clientIP(r)}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("ПАНИКА %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				c.Log("внутренняя ошибка", r.Method+" "+r.URL.Path, false, fmt.Sprint(rec))
				http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
			}
		}()
		a.secHeaders(w, r)
		limit := int64(2 << 20)
		if big {
			limit = int64(a.S().MaxUploadMB)<<20 + 1<<20
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		c.loadSession()
		if r.Method == http.MethodPost {
			if !a.sameOrigin(r) {
				c.Log("отклонён межсайтовый запрос", r.URL.Path, false, r.Header.Get("Origin"))
				http.Error(w, "Запрос отклонён", http.StatusForbidden)
				return
			}
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				if err := r.ParseMultipartForm(8 << 20); err != nil {
					http.Error(w, "Файл слишком велик или запрос повреждён", http.StatusRequestEntityTooLarge)
					return
				}
			} else if err := r.ParseForm(); err != nil {
				http.Error(w, "Некорректный запрос", http.StatusBadRequest)
				return
			}
			if c.Sess != nil && !secure.Equal(r.FormValue("csrf"), c.Sess.CSRF) {
				c.Log("неверный CSRF-токен", r.URL.Path, false, "")
				http.Error(w, "Сеанс устарел. Обновите страницу и повторите действие.", http.StatusForbidden)
				return
			}
		}
		if roles == anyone {
			h(c)
			return
		}
		if c.User == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// обязательные шаги перед началом работы
		p := r.URL.Path
		free := p == "/logout" || strings.HasPrefix(p, "/static/")
		switch {
		case !c.Sess.MFA && !free && p != "/mfa":
			http.Redirect(w, r, "/mfa", http.StatusSeeOther)
			return
		case c.Sess.MFA && (c.User.MustChange || c.User.Consent.IsZero() || c.passwordExpired()) && !free && p != "/password":
			http.Redirect(w, r, "/password", http.StatusSeeOther)
			return
		case c.Sess.MFA && !c.User.MustChange && c.mfaRequired() && !c.User.TOTPEnabled && !free && p != "/password" && !strings.HasPrefix(p, "/profile"):
			c.Redirect("/profile", "!Для вашей роли обязательна двухфакторная аутентификация. Подключите её, чтобы продолжить работу.")
			return
		}
		if roles != authed && !strings.Contains(roles, c.User.Role) {
			c.Deny()
			return
		}
		h(c)
	}
}

// sameOrigin проверяет, что запрос отправлен со страницы самого портала. За внешним веб-сервером
// заголовок Host может отличаться от адреса в браузере, поэтому допускаются также узел из
// external_url и X-Forwarded-Host от доверенного прокси.
func (a *App) sameOrigin(r *http.Request) bool {
	for _, hname := range []string{"Origin", "Referer"} {
		if v := r.Header.Get(hname); v != "" {
			u, err := url.Parse(v)
			if err != nil {
				return false
			}
			if u.Host == r.Host {
				return true
			}
			if ext, _ := a.Cfg.externalHost(); ext != "" && strings.EqualFold(u.Host, ext) {
				return true
			}
			return a.fromProxy(r) && strings.EqualFold(u.Host, strings.TrimSpace(r.Header.Get("X-Forwarded-Host")))
		}
	}
	return true // заголовков нет (не браузер): защита обеспечивается CSRF-токеном и SameSite
}

// Handler возвращает корневой обработчик HTTP.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	g := func(p, roles string, h handler) { mux.HandleFunc("GET "+p, a.wrap(roles, false, h)) }
	po := func(p, roles string, h handler) { mux.HandleFunc("POST "+p, a.wrap(roles, false, h)) }
	up := func(p, roles string, h handler) { mux.HandleFunc("POST "+p, a.wrap(roles, true, h)) }

	static, _ := fs.Sub(assets, "static")
	fsrv := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		a.secHeaders(w, r)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fsrv.ServeHTTP(w, r)
	})

	g("/branding/logo", anyone, (*Ctx).logo)
	g("/login", anyone, (*Ctx).loginPage)
	po("/login", anyone, (*Ctx).loginPost)
	po("/logout", authed, (*Ctx).logout)
	g("/mfa", authed, (*Ctx).mfaPage)
	po("/mfa", authed, (*Ctx).mfaPost)
	g("/password", authed, (*Ctx).passwordPage)
	po("/password", authed, (*Ctx).passwordPost)
	g("/profile", authed, (*Ctx).profile)
	po("/profile/mfa/start", authed, (*Ctx).mfaStart)
	po("/profile/mfa/confirm", authed, (*Ctx).mfaConfirm)
	po("/profile/mfa/disable", authed, (*Ctx).mfaDisable)
	po("/profile/sessions/revoke", authed, (*Ctx).sessionRevoke)
	g("/{$}", authed, (*Ctx).dashboard)
	g("/notices", authed, (*Ctx).notices)
	po("/news", RoleAdmin, (*Ctx).newsCreate)
	po("/news/{id}/delete", staff, (*Ctx).newsDelete)

	g("/courses", authed, (*Ctx).courseList)
	po("/courses", staff, (*Ctx).courseCreate)
	g("/courses/{id}", authed, (*Ctx).courseView)
	g("/courses/{id}/edit", staff, (*Ctx).courseEdit)
	po("/courses/{id}/edit", staff, (*Ctx).courseSave)
	po("/courses/{id}/delete", RoleAdmin, (*Ctx).courseDelete)
	po("/courses/{id}/sections", staff, (*Ctx).sectionSave)
	po("/courses/{id}/items", staff, (*Ctx).itemCreate)
	po("/courses/{id}/news", staff, (*Ctx).newsCreate)
	po("/courses/{id}/posts", authed, (*Ctx).postCreate)
	po("/posts/{id}/delete", authed, (*Ctx).postDelete)
	g("/courses/{id}/journal", staff, (*Ctx).journal)
	g("/courses/{id}/journal.csv", staff, (*Ctx).journalCSV)
	po("/courses/{id}/columns", staff, (*Ctx).columnCreate)
	po("/columns/{id}/delete", staff, (*Ctx).columnDelete)
	po("/courses/{id}/grades", staff, (*Ctx).gradesSave)

	g("/items/{id}", authed, (*Ctx).itemView)
	g("/items/{id}/edit", staff, (*Ctx).itemEdit)
	up("/items/{id}", staff, (*Ctx).itemSave)
	po("/items/{id}/delete", staff, (*Ctx).itemDelete)
	po("/items/{id}/move", staff, (*Ctx).itemMove)
	po("/items/{id}/detach", staff, (*Ctx).itemDetach)
	po("/items/{id}/questions", staff, (*Ctx).questionSave)
	po("/items/{id}/questions/delete", staff, (*Ctx).questionDelete)
	po("/items/{id}/questions/import", staff, (*Ctx).questionImport)
	g("/items/{id}/results", staff, (*Ctx).testResults)
	po("/items/{id}/start", RoleStudent, (*Ctx).attemptStart)
	g("/attempts/{id}", authed, (*Ctx).attemptView)
	po("/attempts/{id}/save", RoleStudent, (*Ctx).attemptSave)
	po("/attempts/{id}/submit", RoleStudent, (*Ctx).attemptSubmit)
	po("/attempts/{id}/annul", staff, (*Ctx).attemptAnnul)
	up("/items/{id}/submit", RoleStudent, (*Ctx).taskSubmit)
	po("/submissions/{id}/grade", staff, (*Ctx).taskGrade)
	g("/files/{id}", authed, (*Ctx).fileServe)

	g("/grades", RoleStudent, (*Ctx).myGrades)
	g("/rating", authed, (*Ctx).rating)
	g("/schedule", authed, (*Ctx).schedule)
	g("/schedule.ics", authed, (*Ctx).scheduleICS)
	po("/schedule", staff, (*Ctx).eventCreate)
	g("/events/{id}", staff, (*Ctx).eventView)
	po("/events/{id}/attendance", staff, (*Ctx).attendanceSave)
	po("/events/{id}/delete", staff, (*Ctx).eventDelete)
	g("/portfolio", authed, (*Ctx).portfolio)
	up("/portfolio", RoleStudent, (*Ctx).portfolioAdd)
	po("/portfolio/{id}/delete", RoleStudent, (*Ctx).portfolioDelete)

	g("/admin/users", RoleAdmin, (*Ctx).adminUsers)
	po("/admin/users", RoleAdmin, (*Ctx).adminUserCreate)
	up("/admin/users/import", RoleAdmin, (*Ctx).adminUserImport)
	g("/admin/users/{id}", RoleAdmin, (*Ctx).adminUser)
	po("/admin/users/{id}", RoleAdmin, (*Ctx).adminUserSave)
	po("/admin/users/{id}/reset", RoleAdmin, (*Ctx).adminUserReset)
	po("/admin/users/{id}/delete", RoleAdmin, (*Ctx).adminUserDelete)
	g("/admin/groups", RoleAdmin, (*Ctx).adminGroups)
	po("/admin/groups", RoleAdmin, (*Ctx).adminGroupSave)
	po("/admin/groups/{id}/delete", RoleAdmin, (*Ctx).adminGroupDelete)
	g("/admin/settings", RoleAdmin, (*Ctx).adminSettings)
	po("/admin/settings", RoleAdmin, (*Ctx).adminSettingsSave)
	g("/admin/audit", RoleAdmin, (*Ctx).adminAudit)
	po("/admin/audit/verify", RoleAdmin, (*Ctx).adminAuditVerify)
	g("/admin/system", RoleAdmin, (*Ctx).adminSystem)
	po("/admin/backup", RoleAdmin, (*Ctx).adminBackup)
	po("/admin/sessions/revoke", RoleAdmin, (*Ctx).adminSessionRevoke)

	mux.HandleFunc("/", a.wrap(authed, false, func(c *Ctx) { c.Fail(http.StatusNotFound, "Страница не найдена.") }))
	return mux
}

// ---------- общие вспомогательные функции ----------

func sortBy[T any](s []T, less func(a, b *T) bool) []T {
	sort.SliceStable(s, func(i, j int) bool { return less(&s[i], &s[j]) })
	return s
}

func (a *App) userName(id string) string {
	if u, ok := a.Users.Get(id); ok {
		return u.FullName
	}
	return "(удалён)"
}

func (a *App) notify(userID, text, link string) {
	id := secure.NewID()
	a.Notices.Put(id, Notice{ID: id, UserID: userID, Text: text, Link: link, Created: time.Now()})
}

func (a *App) parseLocal(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, a.Loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Janitor периодически удаляет просроченные сеансы, завершает истёкшие попытки и сжимает журнал.
func (a *App) Janitor(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s := a.S()
			now := time.Now()
			for _, ss := range a.Sessions.All() {
				if now.Sub(ss.Seen) > time.Duration(s.IdleMinutes)*time.Minute || now.Sub(ss.Created) > time.Duration(s.SessionHours)*time.Hour {
					a.Sessions.Delete(ss.ID)
				}
			}
			for _, at := range a.Attempts.Find(func(x *Attempt) bool {
				return !x.Done && !x.Deadline.IsZero() && now.After(x.Deadline.Add(time.Minute))
			}) {
				a.finishAttempt(at.ID)
			}
			if n++; n%60 == 0 { // раз в час журнал операций сворачивается в читаемый snapshot.json
				a.DB.Compact()
			}
			if lt := now.In(a.Loc); a.Cfg.BackupHour >= 0 && lt.Hour() == a.Cfg.BackupHour {
				name := filepath.Join(a.Cfg.DataPath(), "backup", "kafedra-"+lt.Format("20060102")+".zip")
				if _, err := os.Stat(name); os.IsNotExist(err) {
					a.BackupToFile(name)
				}
			}
		}
	}
}

func toF(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// BackupToFile создаёт резервную копию в файле и удаляет устаревшие автоматические копии.
func (a *App) BackupToFile(name string) error {
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err == nil {
		err = a.Backup(f, false)
		if e := f.Close(); err == nil {
			err = e
		}
	}
	if err == nil {
		err = os.Rename(tmp, name)
	}
	rec := secure.AuditRecord{Event: "резервное копирование", Object: filepath.Base(name), OK: err == nil}
	if err != nil {
		os.Remove(tmp)
		rec.Detail = err.Error()
	}
	a.Audit.Log(rec)
	if old, _ := filepath.Glob(filepath.Join(a.Cfg.DataPath(), "backup", "kafedra-*.zip")); a.Cfg.BackupKeep > 0 && len(old) > a.Cfg.BackupKeep {
		sort.Strings(old)
		for _, o := range old[:len(old)-a.Cfg.BackupKeep] {
			os.Remove(o)
		}
	}
	return err
}

// logo отдаёт логотип: файл из конфигурации (logo_file) либо встроенный.
func (c *Ctx) logo() {
	h := c.W.Header()
	h.Set("Cache-Control", "public, max-age=600")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	types := map[string]string{".png": "image/png", ".svg": "image/svg+xml", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp"}
	if p := c.Cfg.abs(c.Cfg.LogoFile); p != "" {
		if ct := types[strings.ToLower(filepath.Ext(p))]; ct != "" {
			if b, err := os.ReadFile(p); err == nil && len(b) <= 2<<20 {
				h.Set("Content-Type", ct)
				c.W.Write(b)
				return
			}
		}
	}
	b, _ := assets.ReadFile("static/logo.svg")
	h.Set("Content-Type", "image/svg+xml")
	c.W.Write(b)
}
