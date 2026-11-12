package app

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"kafedra/internal/secure"
)

type client struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, c: &http.Client{Jar: jar}}
}

var reCSRF = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (c *client) get(path string) (int, string) {
	resp, err := c.c.Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := reCSRF.FindSubmatch(b); m != nil {
		c.csrf = string(m[1])
	}
	return resp.StatusCode, string(b)
}

func (c *client) post(path string, kv ...string) (int, string) {
	v := url.Values{"csrf": {c.csrf}}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	resp, err := c.c.PostForm(c.base+path, v)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := reCSRF.FindSubmatch(b); m != nil {
		c.csrf = string(m[1])
	}
	return resp.StatusCode, string(b)
}

func (c *client) upload(path, field, filename string, data []byte, kv ...string) (int, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("csrf", c.csrf)
	for i := 0; i+1 < len(kv); i += 2 {
		w.WriteField(kv[i], kv[i+1])
	}
	fw, _ := w.CreateFormFile(field, filename)
	fw.Write(data)
	w.Close()
	resp, err := c.c.Post(c.base+path, w.FormDataContentType(), &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := reCSRF.FindSubmatch(b); m != nil {
		c.csrf = string(m[1])
	}
	return resp.StatusCode, string(b)
}

// login выполняет вход и обязательную смену временного пароля.
func (c *client) login(login, tempPw, newPw string) {
	c.t.Helper()
	c.post("/login", "login", login, "password", tempPw)
	code, body := c.get("/")
	if code != 200 || !strings.Contains(body, "Необходимо сменить пароль") {
		c.t.Fatalf("%s: ожидалась страница смены пароля, код %d", login, code)
	}
	_, body = c.post("/password", "old", tempPw, "new", newPw, "new2", newPw, "consent", "1")
	if strings.Contains(body, "alert-error") && !strings.Contains(body, "двухфакторная") {
		c.t.Fatalf("%s: пароль не сменён: %s", login, body)
	}
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("в ответе нет %q:\n%.1500s", want, body)
	}
}

func idFrom(t *testing.T, body, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("не найдено %s в:\n%.2000s", pattern, body)
	}
	return m[1]
}

func TestEndToEnd(t *testing.T) {
	secure.Iterations = 1000
	dir := t.TempDir()
	a, err := New(Config{DataDir: dir, BackupHour: -1}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	set := a.S()
	set.MFARoles = "admin" // по умолчанию второй фактор необязателен; здесь проверяется обязательный режим
	a.SetSettings(set)
	const adminTemp = "Temp-Admin-Pass-1!"
	if _, err := a.CreateUser("admin", "Админ", "", RoleAdmin, "", adminTemp); err != nil {
		t.Fatal(err)
	}
	creds, err := a.SeedDemo()
	if err != nil {
		t.Fatal(err)
	}
	pw := map[string]string{}
	for _, c := range creds {
		pw[c[0]] = c[1]
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// --- без входа доступ закрыт
	anon := newClient(t, srv.URL)
	if _, body := anon.get("/courses"); !strings.Contains(body, `name="password"`) {
		t.Fatal("аноним не перенаправлен на страницу входа")
	}
	if code, _ := anon.post("/login", "login", "admin", "password", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("неверный пароль: код %d", code)
	}

	// --- администратор: смена пароля, обязательный второй фактор
	adm := newClient(t, srv.URL)
	adm.login("admin", adminTemp, "Glavnyi-Parol-2026!")
	_, body := adm.get("/admin/users")
	mustContain(t, body, "обязательна двухфакторная аутентификация")
	adm.post("/profile/mfa/start")
	_, body = adm.get("/profile")
	secret := idFrom(t, body, `<div class="secret">([A-Z2-7]+)</div>`)
	adm.post("/profile/mfa/confirm", "code", secure.TOTPCode(secret, time.Now()))
	code, body := adm.get("/admin/users")
	if code != 200 {
		t.Fatalf("админ не получил доступ после подключения 2ФА: %d", code)
	}
	mustContain(t, body, "Смирнова Анна Викторовна")
	// повторный вход требует код
	adm2 := newClient(t, srv.URL)
	adm2.post("/login", "login", "admin", "password", "Glavnyi-Parol-2026!")
	_, body = adm2.get("/admin/users")
	mustContain(t, body, "Одноразовый код")
	if code, _ := adm2.post("/mfa", "code", "000000"); code != http.StatusUnauthorized {
		t.Fatalf("неверный код 2ФА принят: %d", code)
	}

	// --- преподаватель: лекция из DOCX, вложение, публикация
	tch := newClient(t, srv.URL)
	tch.login("teacher", pw["teacher"], "Prepod-Parol-2026!")
	_, body = tch.get("/courses")
	courseID := idFrom(t, body, `href="/courses/([0-9a-f]{24})"`)
	_, body = tch.get("/courses/" + courseID)
	sectionID := idFrom(t, body, `name="section" value="([0-9a-f]{24})"`)
	_, body = tch.post("/courses/"+courseID+"/items", "kind", "lesson", "title", "Лекция из Word", "section", sectionID)
	lessonID := idFrom(t, body, `action="/items/([0-9a-f]{24})"`)
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("word/document.xml")
	f.Write([]byte(`<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Импортированный абзац</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	_, body = tch.upload("/items/"+lessonID, "import", "lecture.docx", zbuf.Bytes(), "title", "Лекция из Word", "section", sectionID, "visible", "1", "then", "view")
	mustContain(t, body, "Импортированный абзац")
	// исполняемый файл и подмена расширения отклоняются
	_, body = tch.upload("/items/"+lessonID, "attach", "virus.pdf", []byte("MZ\x90\x00binary"), "title", "Лекция из Word", "section", sectionID, "visible", "1")
	mustContain(t, body, "исполняемые файлы запрещены")
	_, body = tch.upload("/items/"+lessonID, "attach", "run.exe", []byte("data"), "title", "Лекция из Word", "section", sectionID, "visible", "1")
	mustContain(t, body, "не разрешён")
	_, body = tch.upload("/items/"+lessonID, "attach", "slides.pdf", []byte("%PDF-1.7 test"), "title", "Лекция из Word", "section", sectionID, "visible", "1", "then", "view")
	fileID := idFrom(t, body, `src="/files/([0-9a-f]{24})"`)

	// --- учащийся: материалы, тест, задание
	st := newClient(t, srv.URL)
	st.login("student1", pw["student1"], "Uchenik-Parol-2026!")
	_, body = st.get("/courses/" + courseID)
	mustContain(t, body, "Лекция из Word")
	if code, body := st.get("/files/" + fileID); code != 200 || !strings.HasPrefix(body, "%PDF") {
		t.Fatalf("учащийся не получил файл курса: %d", code)
	}
	testID := idFrom(t, body2(st.get("/courses/"+courseID)), `class="kind test">Тест</span>\s*<a class="title" href="/items/([0-9a-f]{24})"`)
	taskID := idFrom(t, body2(st.get("/courses/"+courseID)), `class="kind task">Задание</span>\s*<a class="title" href="/items/([0-9a-f]{24})"`)
	st.get("/items/" + testID)
	_, body = st.post("/items/" + testID + "/start")
	attemptID := idFrom(t, body, `action="/attempts/([0-9a-f]{24})/submit"`)
	if strings.Contains(body, "верный вариант") || strings.Contains(body, "Correct") {
		t.Fatal("страница теста раскрывает правильные ответы")
	}
	// ответы: берём правильные из снимка попытки
	at, _ := a.Attempts.Get(attemptID)
	var kv []string
	for _, q := range at.Questions {
		if q.Type == "text" {
			kv = append(kv, "q_"+q.ID, " GO  Test ")
			continue
		}
		for i, o := range q.Options {
			if o.Correct {
				kv = append(kv, "q_"+q.ID, string(rune('0'+i)))
			}
		}
	}
	_, body = st.post("/attempts/"+attemptID+"/submit", kv...)
	mustContain(t, body, "зачтено")
	mustContain(t, body, "4 из 4")
	st.get("/items/" + taskID)
	_, body = st.upload("/items/"+taskID+"/submit", "file", "solution.zip", []byte("PK\x03\x04zipdata"), "text", "Решение в архиве")
	mustContain(t, body, "на проверке")
	subFile := idFrom(t, body, `href="/files/([0-9a-f]{24})">solution.zip`)

	// --- разграничение доступа
	for _, p := range []string{"/admin/users", "/admin/audit", "/admin/system", "/courses/" + courseID + "/journal", "/items/" + lessonID + "/edit", "/items/" + testID + "/results"} {
		if code, _ := st.get(p); code != http.StatusForbidden {
			t.Errorf("учащийся получил доступ к %s (код %d)", p, code)
		}
	}
	other := newClient(t, srv.URL)
	other.login("student2", pw["student2"], "Uchenik2-Parol-2026!")
	if code, _ := other.get("/files/" + subFile); code != http.StatusForbidden {
		t.Errorf("чужая работа доступна другому учащемуся (код %d)", code)
	}
	if code, _ := other.get("/attempts/" + attemptID); code != http.StatusForbidden {
		t.Errorf("чужая попытка доступна другому учащемуся (код %d)", code)
	}
	if code, _ := tch.get("/admin/users"); code != http.StatusForbidden {
		t.Errorf("преподаватель получил доступ к администрированию (код %d)", code)
	}
	// запрос без CSRF-токена отклоняется
	saved := st.csrf
	st.csrf = "подделка"
	if code, _ := st.post("/courses/"+courseID+"/posts", "body", "x"); code != http.StatusForbidden {
		t.Errorf("запрос с неверным CSRF-токеном принят (код %d)", code)
	}
	st.csrf = saved
	// межсайтовый запрос отклоняется по заголовку Origin
	req, _ := http.NewRequest("POST", srv.URL+"/logout", strings.NewReader("csrf="+url.QueryEscape(saved)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := st.c.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("межсайтовый запрос принят (код %d)", resp.StatusCode)
	}

	// --- преподаватель оценивает работу, журнал и рейтинг
	_, body = tch.get("/items/" + taskID)
	subID := idFrom(t, body, `action="/submissions/([0-9a-f:]+)/grade"`)
	tch.post("/submissions/"+subID+"/grade", "points", "18", "comment", "Хорошо")
	_, body = tch.get("/courses/" + courseID + "/journal")
	mustContain(t, body, "Иванов Пётр Сергеевич")
	mustContain(t, body, "<strong>33</strong>") // 10 (тест) + 18 (задание) + 5 (семинар)
	_, body = st.get("/grades")
	mustContain(t, body, "33 из 35")
	_, body = st.get("/rating")
	mustContain(t, body, "(вы)")
	if strings.Contains(body, "Кузнецова") {
		t.Error("рейтинг раскрывает ФИО других учащихся при отключённой настройке")
	}
	_, body = st.get("/notices")
	mustContain(t, body, "оценена: 18 из 20")
	if code, body := tch.get("/courses/" + courseID + "/journal.csv"); code != 200 || !strings.Contains(body, "Иванов") {
		t.Error("выгрузка журнала не работает")
	}

	// --- расписание и посещаемость
	_, body = tch.get("/schedule")
	target := idFrom(t, body, `<option value="([0-9a-f]{24}:[0-9a-f]{24})">`)
	_, body = tch.post("/schedule", "target", target, "kind", "lab", "date", time.Now().In(a.Loc).Format("2006-01-02"), "from", "23:00", "to", "23:30", "weeks", "1")
	mustContain(t, body, "Лабораторная")
	if code, body := st.get("/schedule.ics"); code != 200 || !strings.Contains(body, "BEGIN:VEVENT") {
		t.Error("экспорт календаря не работает")
	}

	// --- блокировка после неудачных попыток
	bad := newClient(t, srv.URL)
	for i := 0; i < 5; i++ {
		bad.post("/login", "login", "student3", "password", "wrong-password")
	}
	bad.post("/login", "login", "student3", "password", pw["student3"])
	if _, body := bad.get("/"); !strings.Contains(body, `name="password"`) {
		t.Error("учётная запись не заблокирована после 5 неудачных попыток")
	}

	// --- журнал событий и резервная копия
	if n, err := secure.VerifyAudit(a.Cfg.AuditPath()); err != nil || n < 20 {
		t.Fatalf("журнал событий: n=%d err=%v", n, err)
	}
	_, body = adm.get("/admin/audit?fail=1")
	mustContain(t, body, "отказ в доступе")
	mustContain(t, body, "блокировка учётной записи")
	var zb bytes.Buffer
	if err := a.Backup(&zb, true); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "b.zip")
	os.WriteFile(zipPath, zb.Bytes(), 0o600)
	restored := t.TempDir()
	if err := Restore(zipPath, restored, ""); err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{DataDir: restored, BackupHour: -1}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Users.Count(nil) != a.Users.Count(nil) || b.Files.Count(nil) != a.Files.Count(nil) {
		t.Fatal("восстановленные данные не совпадают с исходными")
	}
	u, _ := b.Users.First(func(u *User) bool { return u.Login == "teacher" })
	if ok, _ := b.Keys.VerifyPassword("Prepod-Parol-2026!", u.PassHash); !ok {
		t.Fatal("после восстановления пароль не проверяется")
	}

	// --- удаление учётной записи удаляет персональные данные
	a.DeleteUser(at.UserID)
	if a.Attempts.Count(func(x *Attempt) bool { return x.UserID == at.UserID }) != 0 || a.Subs.Count(func(x *Submission) bool { return x.UserID == at.UserID }) != 0 {
		t.Fatal("данные удалённого пользователя сохранились")
	}
}

func body2(_ int, b string) string { return b }

func TestParseQuestions(t *testing.T) {
	qs, err := ParseQuestions("? Вопрос 1\n+ да\n- нет\n\n? Вопрос 2\nс кодом\n+ a\n+ b\n- c\n* 3\n\n? Вопрос 3\n= 42\n")
	if err != nil || len(qs) != 3 {
		t.Fatalf("%v %d", err, len(qs))
	}
	if qs[0].Type != "single" || qs[1].Type != "multi" || qs[1].Points != 3 || qs[2].Type != "text" || !strings.Contains(qs[1].Text, "с кодом") {
		t.Fatalf("%+v", qs)
	}
	if _, err := ParseQuestions("? Без правильного\n- a\n- b\n"); err == nil {
		t.Fatal("вопрос без правильного ответа принят")
	}
	// частичный зачёт: 1 верный из 2 → половина; верный + неверный → 0
	q := qs[1]
	if got := scoreQuestion(&q, Answer{Opts: []int{0}}); got != 1.5 {
		t.Fatalf("частичный зачёт: %v", got)
	}
	if got := scoreQuestion(&q, Answer{Opts: []int{0, 2}}); got != 0 {
		t.Fatalf("штраф за неверный вариант: %v", got)
	}
}

// Второй фактор по умолчанию необязателен; настройки читаются из config.json и записываются в него
// в удобочитаемом виде.
func TestConfigAndOptionalMFA(t *testing.T) {
	secure.Iterations = 1000
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	cfg.PortalName, cfg.OrgName, cfg.LogFile, cfg.DataDir = "Корпоративный университет", "АО «Пример»", "logs/kafedra.log", "./data"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "\n  \"portal_name\": \"Корпоративный университет\"") || !strings.Contains(string(raw), "\n    \"mfa_required_for\": \"none\"") {
		t.Fatalf("конфигурация не в читаемом виде:\n%s", raw)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataPath() != filepath.Join(dir, "data") || cfg.LogPath() != filepath.Join(dir, "logs", "kafedra.log") {
		t.Fatalf("пути: %s %s", cfg.DataPath(), cfg.LogPath())
	}
	a, err := New(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.CreateUser("admin", "Админ", "", RoleAdmin, "", "Temp-Parol-12345!")
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.login("admin", "Temp-Parol-12345!", "Glavnyi-Parol-2026!")
	code, body := c.get("/admin/users") // без подключения второго фактора
	if code != 200 || strings.Contains(body, "обязательна двухфакторная") {
		t.Fatalf("администратор без 2ФА не допущен: %d", code)
	}
	mustContain(t, body, "Корпоративный университет")
	mustContain(t, body, "АО «Пример»")
	mustContain(t, body, "© ГБУ ПК «Центр информационного развития ПК»")
	if strings.Contains(body, "версия") {
		t.Fatal("в интерфейсе выводится номер версии")
	}
	// изменение настроек из интерфейса попадает в config.json
	c.get("/admin/settings")
	c.post("/admin/settings", "pw_min_len", "14", "mfa_roles", "none", "lock_attempts", "7", "allowed_ext", "pdf docx", "totp_skew", "2")
	saved, _ := LoadConfig(path)
	if saved.Settings.PwMinLen != 14 || saved.Settings.LockAttempts != 7 || saved.Settings.TOTPSkew != 2 || saved.PortalName != "Корпоративный университет" {
		t.Fatalf("настройки не сохранены в файл: %+v", saved.Settings)
	}
	// снимок базы данных — с отступами
	a.DB.Compact()
	snap, _ := os.ReadFile(filepath.Join(cfg.DataPath(), "db", "snapshot.json"))
	if !strings.Contains(string(snap), "\n        \"Login\": \"admin\"") {
		t.Fatalf("snapshot.json не отформатирован:\n%.400s", snap)
	}
	// логотип: встроенный и из файла
	if _, body := c.get("/branding/logo"); !strings.Contains(body, "<svg") {
		t.Fatal("встроенный логотип не отдаётся")
	}
}
