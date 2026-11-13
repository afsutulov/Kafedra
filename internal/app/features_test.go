package app

import (
	"archive/zip"
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kafedra/internal/content"
	"kafedra/internal/secure"
)

// Запуск после сбоя питания во время записи журнала событий и после переноса старых месяцев в архив.
func TestAuditSurvivesTornRecordAndArchivedMonths(t *testing.T) {
	dir := t.TempDir()
	au, err := secure.OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	au.Log(secure.AuditRecord{Event: "сентябрь", OK: true, Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)})
	au.Log(secure.AuditRecord{Event: "октябрь", OK: true, Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	au.Log(secure.AuditRecord{Event: "ноябрь", OK: true, Time: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	au.Close()
	// оборванная запись в конце последнего файла
	f, _ := os.OpenFile(filepath.Join(dir, "audit-2026-11.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"seq":4,"time":"2026-11-13T00:00:00Z","event":"обор`)
	f.Close()
	au, err = secure.OpenAudit(dir)
	if err != nil {
		t.Fatalf("служба не запустится после оборванной записи: %v", err)
	}
	au.Close()
	if n, err := secure.VerifyAudit(dir); err != nil || n != 4 { // 3 записи + запись о восстановлении
		t.Fatalf("n=%d err=%v", n, err)
	}
	// самый ранний месяц перенесён в архив — допустимо
	os.Remove(filepath.Join(dir, "audit-2026-09.log"))
	if _, err := secure.VerifyAudit(dir); err == nil {
		t.Fatal("удаление начала журнала не обнаружено")
	}
	if err := secure.AcceptArchivedPrefix(dir); err != nil {
		t.Fatal(err)
	}
	au, err = secure.OpenAudit(dir)
	if err != nil {
		t.Fatalf("служба не запустится после архивирования старого месяца: %v", err)
	}
	au.Log(secure.AuditRecord{Event: "после архива", OK: true})
	au.Close()
	if first := secure.AuditFirstSeq(dir); first != 2 {
		t.Fatalf("первая запись №%d", first)
	}
	// а пропуск в середине — нарушение
	os.Remove(filepath.Join(dir, "audit-2026-10.log"))
	if err := secure.AcceptArchivedPrefix(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "audit-2026-11.log"))
	lines := strings.SplitAfter(string(b), "\n")
	os.WriteFile(filepath.Join(dir, "audit-2026-11.log"), []byte(lines[0]+strings.Join(lines[2:], "")), 0o600)
	if _, err := secure.VerifyAudit(dir); err == nil {
		t.Fatal("удаление записи из середины журнала не обнаружено")
	}
}

type fixture struct {
	a                      *App
	url                    string
	teacher, student       *client
	course, lesson, testID string
}

func newFixture(t *testing.T) *fixture {
	secure.Iterations = 1000
	a, err := New(Config{DataDir: t.TempDir(), BackupHour: -1, OrgName: "АО «Пример»", PortalName: "Портал"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	creds, err := a.SeedDemo()
	if err != nil {
		t.Fatal(err)
	}
	pw := map[string]string{}
	for _, c := range creds {
		pw[c[0]] = c[1]
	}
	srv := newTestServer(t, a)
	fx := &fixture{a: a, url: srv, teacher: newClient(t, srv), student: newClient(t, srv)}
	fx.teacher.login("teacher", pw["teacher"], "Prepod-Parol-2026!")
	fx.student.login("student1", pw["student1"], "Uchenik-Parol-2026!")
	co := a.Courses.All()[0]
	fx.course = co.ID
	for _, it := range a.Items.All() {
		switch it.Kind {
		case KindLesson:
			fx.lesson = it.ID
		case KindTest:
			fx.testID = it.ID
		}
	}
	return fx
}

func xlsxBytes(t *testing.T, rows [][]any) []byte {
	var buf bytes.Buffer
	if err := content.WriteXLSX(&buf, content.Sheet{Name: "Лист", Rows: rows}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTeacherImportsGroupFromExcel(t *testing.T) {
	fx := newFixture(t)
	code, tpl := fx.teacher.get("/templates/students.xlsx")
	if code != 200 || !strings.HasPrefix(tpl, "PK") {
		t.Fatalf("шаблон списка группы: %d", code)
	}
	if rows, err := content.ReadXLSX(strings.NewReader(tpl), int64(len(tpl))); err != nil || rows[0][0] != "ФИО" {
		t.Fatalf("шаблон не читается: %v", err)
	}
	fx.teacher.get("/courses/" + fx.course + "?tab=people")
	file := xlsxBytes(t, [][]any{{"ФИО", "Логин (необязательно)", "Эл. почта (необязательно)"},
		{"Щукин Юрий Жанович", "", "sch@example.ru"}, {"Щукин Юрий Жанович", "", ""}, {"Орлова Анна", "orlova", ""}, {"Плохой Логин", "a b", ""}})
	code, body := fx.teacher.upload("/courses/"+fx.course+"/students/import", "file", "group.xlsx", file, "group", "Новая-1")
	if code != 200 || !strings.HasPrefix(body, "PK") {
		t.Fatalf("импорт группы: код %d: %.300s", code, body)
	}
	res, err := content.ReadXLSX(strings.NewReader(body), int64(len(body)))
	if err != nil || len(res) != 5 {
		t.Fatalf("файл результата: %v %q", err, res)
	}
	if res[1][2] != "schukin.yz" || res[2][2] != "schukin.yz2" || res[3][2] != "orlova" || len(res[1][3]) < 12 || !strings.HasPrefix(res[4][4], "ошибка") || res[4][3] != "" {
		t.Fatalf("результат импорта: %q", res)
	}
	g, ok := fx.a.Groups.First(func(g *Group) bool { return g.Name == "Новая-1" })
	co, _ := fx.a.Courses.Get(fx.course)
	if !ok || !contains(co.GroupIDs, g.ID) || fx.a.Users.Count(func(u *User) bool { return u.GroupID == g.ID && u.Role == RoleStudent && u.MustChange }) != 3 {
		t.Fatal("группа не создана, не назначена курсу либо учащиеся не созданы")
	}
	// созданный учащийся входит с временным паролем
	nc := newClient(t, fx.url)
	nc.login("orlova", res[3][3], "Sovsem-Novyi-Parol-1!")
	// чужую существующую группу преподаватель пополнить не может
	fx.a.Groups.Put("foreign", Group{ID: "foreign", Name: "Чужая"})
	_, body = fx.teacher.upload("/courses/"+fx.course+"/students/import", "file", "g.xlsx", xlsxBytes(t, [][]any{{"Иванов Иван"}}), "group", "Чужая")
	res, _ = content.ReadXLSX(strings.NewReader(body), int64(len(body)))
	if len(res) != 2 || !strings.Contains(res[1][4], "не относится к вашим курсам") {
		t.Fatalf("пополнение чужой группы: %q", res)
	}
	// учащийся не может импортировать
	if code, _ := fx.student.upload("/courses/"+fx.course+"/students/import", "file", "g.xlsx", file, "group", "X"); code != 403 {
		t.Fatalf("учащийся допущен к импорту: %d", code)
	}
}

func TestQuestionsFromExcelAndJournalExport(t *testing.T) {
	fx := newFixture(t)
	if code, tpl := fx.teacher.get("/templates/test.xlsx"); code != 200 {
		t.Fatal("шаблон теста недоступен")
	} else if rows, err := content.ReadXLSX(strings.NewReader(tpl), int64(len(tpl))); err != nil {
		t.Fatal(err)
	} else if qs, err := questionsFromSheet(rows); err != nil || len(qs) != 3 || qs[0].Type != "single" || qs[1].Type != "multi" || qs[1].Points != 2 || qs[2].Type != "text" || len(qs[2].Answers) != 2 {
		t.Fatalf("примеры из шаблона не разбираются: %v %+v", err, qs)
	}
	before, _ := fx.a.Items.Get(fx.testID)
	fx.teacher.get("/items/" + fx.testID + "/edit")
	good := xlsxBytes(t, [][]any{{"Вопрос", "Баллы", "Правильный ответ", "Вариант 1", "Вариант 2"}, {"2+2?", 1, 2, "3", "4"}, {"Столица России?", nil, "Москва"}})
	_, body := fx.teacher.upload("/items/"+fx.testID+"/questions/xlsx", "file", "q.xlsx", good)
	mustContain(t, body, "Загружено вопросов из Excel: 2")
	after, _ := fx.a.Items.Get(fx.testID)
	if len(after.Questions) != len(before.Questions)+2 || !after.Questions[len(after.Questions)-2].Options[1].Correct {
		t.Fatal("вопросы не добавлены")
	}
	bad := xlsxBytes(t, [][]any{{"Вопрос"}, {"Хороший?", 1, 1, "да", "нет"}, {"Плохой?", 1, 5, "да", "нет"}})
	_, body = fx.teacher.upload("/items/"+fx.testID+"/questions/xlsx", "file", "q.xlsx", bad)
	mustContain(t, body, "строка 3")
	if again, _ := fx.a.Items.Get(fx.testID); len(again.Questions) != len(after.Questions) {
		t.Fatal("при ошибке в файле часть вопросов всё же загружена")
	}
	_, body = fx.teacher.upload("/items/"+fx.testID+"/questions/xlsx", "file", "q.xls", []byte("x"))
	mustContain(t, body, "нужен файл формата .xlsx")
	// выгрузка журнала
	code, x := fx.teacher.get("/courses/" + fx.course + "/journal.xlsx")
	rows, err := content.ReadXLSX(strings.NewReader(x), int64(len(x)))
	if code != 200 || err != nil || rows[0][0] != "ФИО" || len(rows) < 4 {
		t.Fatalf("журнал в Excel: %d %v %q", code, err, rows)
	}
	found := false
	for _, r := range rows {
		found = found || (r[0] == "Иванов Пётр Сергеевич" && r[len(r)-5] == "5") // итог: 5 баллов за семинар
	}
	if !found {
		t.Fatalf("в журнале нет ожидаемой строки: %q", rows)
	}
	if code, _ := fx.student.get("/courses/" + fx.course + "/journal.xlsx"); code != 403 {
		t.Fatal("журнал доступен учащемуся")
	}
}

func TestStudentDocumentsAndCertificate(t *testing.T) {
	fx := newFixture(t)
	for _, p := range []string{"/docs/anketa.docx", "/docs/soglasie.docx"} {
		code, body := fx.student.get(p)
		md, _, err := content.ImportDOCX(strings.NewReader(body), int64(len(body)), nil)
		if code != 200 || err != nil || !strings.Contains(md, "Иванов Пётр Сергеевич") {
			t.Fatalf("%s: %d %v", p, code, err)
		}
	}
	// бланки доступны уже при первом входе, до смены пароля
	fresh := newClient(t, fx.url)
	u, _ := fx.a.Users.First(func(u *User) bool { return u.Login == "student2" })
	fx.a.setPassword(u.ID, "Vremennyi-Parol-77!", secure.Policy{}, 5, true)
	fresh.post("/login", "login", "student2", "password", "Vremennyi-Parol-77!")
	if _, body := fresh.get("/"); !strings.Contains(body, "/docs/anketa.docx") || !strings.Contains(body, "Смирнова Анна Викторовна") {
		t.Fatal("на странице первого входа нет ссылок на анкету и согласие")
	}
	if code, _ := fresh.get("/docs/anketa.docx"); code != 200 {
		t.Fatalf("анкета недоступна при первом входе: %d", code)
	}
	// курс не завершён — ни заявления, ни сертификата
	_, body := fx.student.get("/courses/" + fx.course + "/zayavlenie.docx")
	mustContain(t, body, "после прохождения всех разделов")
	fx.student.get("/courses/" + fx.course)
	_, body = fx.student.post("/courses/" + fx.course + "/certificate")
	mustContain(t, body, "после прохождения всех разделов")
	// оставляем в курсе только лекцию и изучаем её
	for _, it := range fx.a.Items.All() {
		if it.Kind != KindLesson {
			fx.a.Items.Update(it.ID, func(x *Item) error { x.Visible = false; return nil })
		}
	}
	fx.student.get("/items/" + fx.lesson)
	_, body = fx.student.get("/courses/" + fx.course)
	mustContain(t, body, "Обучение по курсу завершено")
	code, z := fx.student.get("/courses/" + fx.course + "/zayavlenie.docx")
	if md, _, err := content.ImportDOCX(strings.NewReader(z), int64(len(z)), nil); code != 200 || err != nil || !strings.Contains(md, "Программирование на Go") {
		t.Fatalf("заявление: %d %v", code, err)
	}
	if _, _, err := fx.a.certFont(); err != nil {
		t.Skip("в системе нет шрифта TrueType: ", err)
	}
	code, pdf := fx.student.post("/courses/" + fx.course + "/certificate")
	if code != 200 || !strings.HasPrefix(pdf, "%PDF-1.5") {
		t.Fatalf("сертификат: %d %.200s", code, pdf)
	}
	ct, ok := fx.a.Certs.First(func(x *Certificate) bool { return true })
	if !ok || ct.StudentName != "Иванов Пётр Сергеевич" || ct.Teachers != "Смирнова Анна Викторовна" || ct.OrgName != "АО «Пример»" || !strings.HasPrefix(ct.Number, "К-") {
		t.Fatalf("сведения сертификата: %+v", ct)
	}
	// Выданный PDF сохраняется даже после удаления или замены системного шрифта.
	fx.a.Cfg.CertificateFont = "/missing-certificate-font.ttf"
	if code, saved := fx.student.get("/certificates/" + ct.ID); code != 200 || saved != pdf {
		t.Fatalf("содержимое выданного PDF изменилось: %d", code)
	}
	// повторный запрос не создаёт второй сертификат
	fx.student.get("/courses/" + fx.course)
	fx.student.post("/courses/" + fx.course + "/certificate")
	if fx.a.Certs.Count(nil) != 1 {
		t.Fatal("выдан второй сертификат")
	}
	// преподаватель курса видит сертификат, посторонний учащийся — нет
	if code, _ := fx.teacher.get("/certificates/" + ct.ID); code != 200 {
		t.Fatalf("преподаватель не видит сертификат: %d", code)
	}
	fresh.post("/password", "old", "Vremennyi-Parol-77!", "new", "Drugoi-Uchenik-Parol-5!", "new2", "Drugoi-Uchenik-Parol-5!", "consent", "1")
	if code, body := fresh.get("/certificates/" + ct.ID); code != 403 || strings.HasPrefix(body, "%PDF") {
		t.Fatalf("чужой сертификат доступен другому учащемуся: %d", code)
	}
	os.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o600)
}

// Ответы, пришедшие до срока, принимаются, даже если обработка задержалась из-за очереди на запись.
func TestSubmitTimeIsTakenAtArrival(t *testing.T) {
	fx := newFixture(t)
	fx.a.Items.Update(fx.testID, func(x *Item) error { x.TimeLimit = 1; return nil })
	fx.student.get("/items/" + fx.testID)
	_, body := fx.student.post("/items/" + fx.testID + "/start")
	attemptID := idFrom(t, body, `action="/attempts/([0-9a-f]{24})/submit"`)
	at, _ := fx.a.Attempts.Get(attemptID)
	var kv []string
	for _, q := range at.Questions {
		if q.Type == "text" {
			kv = append(kv, "q_"+q.ID, "go test")
		}
		for i, o := range q.Options {
			if o.Correct {
				kv = append(kv, "q_"+q.ID, string(rune('0'+i)))
			}
		}
	}
	// срок истекает через 0,3 с; запрос уходит сразу, но блокировка записи занята ещё 1,5 с сверх допуска нет —
	// поэтому сдвигаем срок так, чтобы обработка началась уже после «срок + допуск»
	fx.a.Attempts.Update(attemptID, func(x *Attempt) error {
		x.Deadline = time.Now().Add(300*time.Millisecond - attemptSubmitGrace)
		return nil
	})
	fx.a.writeMu.Lock()
	go func() { time.Sleep(1500 * time.Millisecond); fx.a.writeMu.Unlock() }()
	_, body = fx.student.post("/attempts/"+attemptID+"/submit", kv...)
	done, _ := fx.a.Attempts.Get(attemptID)
	if !done.Done || done.Score != done.MaxScore {
		t.Fatalf("ответы, пришедшие вовремя, отброшены: %v из %v", done.Score, done.MaxScore)
	}
	mustContain(t, body, "4 из 4")
}

func newTestServer(t *testing.T, a *App) string {
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTeacherCannotAttachForeignGroupByEditingCourse(t *testing.T) {
	fx := newFixture(t)
	if err := fx.a.Groups.Put("foreign", Group{ID: "foreign", Name: "Чужая"}); err != nil {
		t.Fatal(err)
	}
	fx.teacher.get("/courses/" + fx.course + "/edit")
	code, _ := fx.teacher.post("/courses/"+fx.course+"/edit", "title", "Изменён", "groups", "foreign")
	co, _ := fx.a.Courses.Get(fx.course)
	if code != 403 || contains(co.GroupIDs, "foreign") {
		t.Fatalf("права преподавателя расширены: %d %v", code, co.GroupIDs)
	}
}

func TestExcelQuestionColumnsAndValidation(t *testing.T) {
	valid := [][]string{{"Вопрос", "Баллы", "Правильный ответ"}, {"Вопрос?", "2,5", "3", "первый", "", "третий"}}
	qs, err := questionsFromSheet(valid)
	if err != nil || len(qs) != 1 || !qs[0].Options[1].Correct || qs[0].Points != 2.5 {
		t.Fatalf("ответ привязан к другой колонке: %v %+v", err, qs)
	}
	for _, row := range [][]string{
		{"Вопрос?", "1", "2", "первый", "", "третий"},
		{"Вопрос?", "NaN", "1", "да", "нет"},
		{"", "1", "1", "да", "нет"},
	} {
		qs, err := questionsFromSheet(append(valid, row))
		if err == nil || !strings.Contains(err.Error(), "строка 3") || len(qs) > 0 {
			t.Fatalf("ошибка строки не выявлена: %v %+v", err, qs)
		}
	}
}

func TestCertificateRequiresPassedTestAndGradedTask(t *testing.T) {
	fx := newFixture(t)
	user, _ := fx.a.Users.First(func(u *User) bool { return u.Login == "student1" })
	test, _ := fx.a.Items.Get(fx.testID)
	task, _ := fx.a.Items.First(func(it *Item) bool { return it.Kind == KindTask })
	fx.student.get("/items/" + fx.lesson)
	attempt := Attempt{ID: "failed", UserID: user.ID, CourseID: fx.course, ItemID: test.ID, Done: true, MaxScore: 10}
	fx.a.Attempts.Put(attempt.ID, attempt)
	sub := Submission{ID: task.ID + ":" + user.ID, ItemID: task.ID, CourseID: fx.course, UserID: user.ID}
	fx.a.Subs.Put(sub.ID, sub)
	fx.student.get("/courses/" + fx.course)
	_, body := fx.student.post("/courses/" + fx.course + "/certificate")
	mustContain(t, body, "после прохождения всех разделов")
	fx.a.Attempts.Update(attempt.ID, func(x *Attempt) error { x.Score = 10; return nil })
	if fx.a.completed(user.ID, fx.course) {
		t.Fatal("проверка задания не завершена")
	}
	fx.a.Subs.Update(sub.ID, func(x *Submission) error { x.Graded = true; return nil })
	if !fx.a.completed(user.ID, fx.course) {
		t.Fatal("курс с зачтённым тестом и проверенным заданием не завершён")
	}
}

func TestBackupKeepsAttachmentRemovedAfterSnapshot(t *testing.T) {
	a := regressionApp(t)
	f, err := a.storeFile(strings.NewReader("material"), "p.txt", "", "course", "course", "")
	if err != nil {
		t.Fatal(err)
	}
	a.writeMu.Lock()
	plan, err := a.backupPrepare()
	a.writeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseBackupFiles()
	a.removeFile(f.ID)
	if _, ok := a.Files.Get(f.ID); ok {
		t.Fatal("вложение по-прежнему доступно в базе")
	}
	var buf bytes.Buffer
	damaged, err := a.backupWrite(&buf, plan, true)
	if err != nil || len(damaged) != 0 {
		t.Fatalf("вложение потеряно в копии: %v %v", damaged, err)
	}
	z, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range z.File {
		if entry.Name == "files/"+f.ID[:2]+"/"+f.ID {
			return
		}
	}
	t.Fatal("вложение отсутствует в копии")
}

func TestIncompleteAutomaticBackupDoesNotRotateLastCompleteCopy(t *testing.T) {
	a := regressionApp(t)
	a.Cfg.BackupKeep = 1
	f, err := a.storeFile(strings.NewReader("material"), "p.txt", "", "course", "course", "")
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(a.Cfg.DataPath(), "backup")
	good := filepath.Join(backupDir, "kafedra-20260101.zip")
	if err := a.BackupToFile(good); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.filePath(f.ID)); err != nil {
		t.Fatal(err)
	}
	if err := a.BackupToFile(filepath.Join(backupDir, "kafedra-20260102.zip")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatalf("предыдущая полная копия утрачена: %v", err)
	}
}
