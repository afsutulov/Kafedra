package app

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kafedra/internal/secure"
)

func regressionApp(t *testing.T) *App {
	t.Helper()
	previous := secure.Iterations
	secure.Iterations = 1000
	t.Cleanup(func() { secure.Iterations = previous })
	a, err := New(Config{DataDir: t.TempDir(), BackupHour: -1}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func regressionSession(t *testing.T, a *App, id, role, group string) (string, string) {
	t.Helper()
	now := time.Now()
	if _, exists := a.Users.Get(id); !exists {
		if err := a.Users.Put(id, User{ID: id, Login: id, Role: role, GroupID: group, PassChanged: now, Consent: now}); err != nil {
			t.Fatal(err)
		}
	}
	token, csrf := secure.Token(), secure.Token()
	sid := secure.HashToken(token)
	if err := a.Sessions.Put(sid, Session{ID: sid, UserID: id, CSRF: csrf, Created: now, Seen: now}); err != nil {
		t.Fatal(err)
	}
	return token, csrf
}

func regressionRequest(h http.Handler, method, path, token string, values url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(values.Encode()))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestFileAccessMatchesPageAccess(t *testing.T) {
	a := regressionApp(t)
	h := a.Handler()
	student, _ := regressionSession(t, a, "student", RoleStudent, "group")
	outsider, _ := regressionSession(t, a, "outsider", RoleTeacher, "")
	teacher, _ := regressionSession(t, a, "teacher", RoleTeacher, "")
	a.Courses.Put("course", Course{ID: "course", TeacherIDs: []string{"teacher"}, GroupIDs: []string{"group"}})
	pf, err := a.storeFile(strings.NewReader("portfolio"), "p.txt", "student", "", "pf", "")
	if err != nil {
		t.Fatal(err)
	}
	// Вложения до обновления не содержали идентификатора материала.
	legacy, err := a.storeFile(strings.NewReader("legacy"), "p.txt", "teacher", "course", "course", "")
	if err != nil {
		t.Fatal(err)
	}
	image, err := a.storeFile(strings.NewReader("imported"), "p.txt", "teacher", "course", "course", "lesson")
	if err != nil {
		t.Fatal(err)
	}
	a.Items.Put("lesson", Item{ID: "lesson", CourseID: "course", Kind: KindLesson, FileIDs: []string{legacy.ID},
		Body: "![рисунок](/files/" + image.ID + ")"})
	for _, f := range []FileMeta{legacy, image} {
		if w := regressionRequest(h, "GET", "/files/"+f.ID, student, nil); w.Code != 403 {
			t.Errorf("draft file: %d", w.Code)
		}
	}
	a.Items.Update("lesson", func(it *Item) error { it.Visible = true; return nil })
	for _, f := range []FileMeta{legacy, image} {
		if w := regressionRequest(h, "GET", "/files/"+f.ID, student, nil); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("published file: %d %s", w.Code, w.Header().Get("Cache-Control"))
		}
	}
	if w := regressionRequest(h, "GET", "/files/"+pf.ID, outsider, nil); w.Code != 403 {
		t.Errorf("unrelated teacher: %d", w.Code)
	}
	if w := regressionRequest(h, "GET", "/files/"+pf.ID, teacher, nil); w.Code != 200 {
		t.Errorf("assigned teacher: %d", w.Code)
	}
}

func TestOldSessionRequiresMFAAfterEnrollment(t *testing.T) {
	a := regressionApp(t)
	h := a.Handler()
	token, csrf := regressionSession(t, a, "admin", RoleAdmin, "")
	old, _ := regressionSession(t, a, "admin", RoleAdmin, "")
	settings := a.S()
	settings.MFARoles = "admin"
	a.SetSettings(settings)
	regressionRequest(h, "POST", "/profile/mfa/start", token, url.Values{"csrf": {csrf}})
	session, _ := a.Sessions.Get(secure.HashToken(token))
	key, err := a.Keys.Open(session.PendingTOTP)
	if err != nil {
		t.Fatal(err)
	}
	regressionRequest(h, "POST", "/profile/mfa/confirm", token,
		url.Values{"csrf": {csrf}, "code": {secure.TOTPCode(string(key), time.Now())}})
	if w := regressionRequest(h, "GET", "/admin/users", token, nil); w.Code != 200 {
		t.Fatalf("enrolled session: %d", w.Code)
	}
	if w := regressionRequest(h, "GET", "/admin/users", old, nil); w.Header().Get("Location") != "/mfa" {
		t.Fatalf("old session: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestAttemptLimitAccessAndHistoricalGrade(t *testing.T) {
	a := regressionApp(t)
	h := a.Handler()
	token, csrf := regressionSession(t, a, "student", RoleStudent, "group")
	teacher, teacherCSRF := regressionSession(t, a, "teacher", RoleTeacher, "")
	a.Courses.Put("course", Course{ID: "course", TeacherIDs: []string{"teacher"}, GroupIDs: []string{"group"}})
	a.Items.Put("test", Item{ID: "test", CourseID: "course", Title: "Тест", Kind: KindTest, Visible: true, MaxAttempts: 1,
		MaxPoints: 10, PassPercent: 50, Questions: []Question{
			{ID: "q", Type: "text", Points: 4, Answers: []string{"x"}},
			{ID: "q2", Type: "text", Points: 1, Answers: []string{"y"}},
		}})
	form := url.Values{"csrf": {csrf}}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			regressionRequest(h, "POST", "/items/test/start", token, form)
		}()
	}
	close(start)
	wg.Wait()
	attempts := a.Attempts.All()
	if len(attempts) != 1 {
		t.Fatalf("created %d attempts", len(attempts))
	}
	at := attempts[0]
	path := "/attempts/" + at.ID
	a.Courses.Update("course", func(co *Course) error { co.GroupIDs = nil; return nil })
	for _, action := range []string{"/save", "/submit"} {
		if w := regressionRequest(h, "POST", path+action, token, form); w.Code != 403 {
			t.Fatalf("revoked attempt %s: %d", action, w.Code)
		}
	}
	a.Courses.Update("course", func(co *Course) error { co.GroupIDs = []string{"group"}; return nil })
	if w := regressionRequest(h, "POST", path+"/submit", token, url.Values{"csrf": {csrf}, "q_q": {"x"}}); w.Code != 303 {
		t.Fatalf("submit: %d", w.Code)
	}
	if w := regressionRequest(h, "POST", "/items/test", teacher, url.Values{
		"csrf": {teacherCSRF}, "title": {"Тест"}, "max_points": {"10"}, "pass_percent": {"100"}, "visible": {"1"},
	}); w.Code != 303 {
		t.Fatalf("edit passing threshold: %d", w.Code)
	}
	if w := regressionRequest(h, "GET", path, token, nil); w.Code != 200 || !strings.Contains(w.Body.String(), ">зачтено<") {
		t.Fatalf("historical pass changed: %d", w.Code)
	}
	if w := regressionRequest(h, "POST", "/items/test", teacher, url.Values{
		"csrf": {teacherCSRF}, "title": {"Тест"}, "max_points": {"5"}, "visible": {"1"},
	}); w.Code != 303 {
		t.Fatalf("edit: %d", w.Code)
	}
	it, _ := a.Items.Get("test")
	if it.MaxPoints != 10 {
		t.Fatal("maximum changed after test started")
	}
	done, _ := a.Attempts.Get(at.ID)
	if !done.Done || done.Points != 8 || done.Grading == nil || done.Grading.PassPercent != 50 {
		t.Fatalf("invalid historical result: %+v", done)
	}
}

func TestOversizeLoginDoesNotCorruptAudit(t *testing.T) {
	a := regressionApp(t)
	w := regressionRequest(a.Handler(), "POST", "/login", "", url.Values{"login": {strings.Repeat("a", 1100000)}})
	if w.Code != 401 {
		t.Fatalf("login: %d", w.Code)
	}
	if n, err := a.Audit.Verify(); err != nil || n != 1 {
		t.Fatalf("audit n=%d: %v", n, err)
	}
}

func TestBackupRejectsMissingFileAndBadRestoreLeavesData(t *testing.T) {
	a := regressionApp(t)
	file, err := a.storeFile(strings.NewReader("material"), "p.txt", "", "course", "course", "")
	if err != nil {
		t.Fatal(err)
	}
	var good, bad bytes.Buffer
	if err := a.Backup(&good, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.filePath(file.ID)); err != nil {
		t.Fatal(err)
	}
	// пропавшее вложение не отменяет копию: оно перечисляется в манифесте, а копия восстанавливается
	if err := a.Backup(&bad, true); err != nil {
		t.Fatalf("копия с пропавшим вложением не создана: %v", err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(bad.Bytes()), int64(bad.Len()))
	manifest := ""
	for _, zf := range zr.File {
		if zf.Name == "manifest.json" {
			rc, _ := zf.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			manifest = string(b)
		}
	}
	if !strings.Contains(manifest, "damaged_files") || !strings.Contains(manifest, file.ID) {
		t.Fatalf("пропавшее вложение не отмечено в манифесте: %s", manifest)
	}
	partial := filepath.Join(t.TempDir(), "partial.zip")
	os.WriteFile(partial, bad.Bytes(), 0o600)
	if err := Restore(partial, t.TempDir(), ""); err == nil {
		t.Fatal("неполная копия восстановлена без явного разрешения")
	}
	if err := Restore(partial, t.TempDir(), "", true); err != nil {
		t.Fatalf("копия с отмеченным повреждённым вложением не восстановлена: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if err := os.WriteFile(archive, good.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(archive, a.Cfg.DataPath(), ""); err == nil {
		t.Fatal("restore allowed with an active store")
	}
	target := t.TempDir()
	auditDir := t.TempDir()
	logFile := filepath.Join(auditDir, "service.log")
	os.WriteFile(logFile, []byte("keep this file"), 0o600)
	if err := Restore(archive, target, auditDir); err != nil {
		t.Fatal(err)
	}
	if log, err := os.ReadFile(logFile); err != nil || string(log) != "keep this file" {
		t.Fatalf("unrelated log was changed: %v", err)
	}
	snapshot := filepath.Join(target, "db", "snapshot.json")
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	bad.Reset()
	z := zip.NewWriter(&bad)
	f, err := z.Create("db/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("invalid snapshot"))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(archive, bad.Bytes(), 0o600)
	if err := Restore(archive, target, auditDir); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if after, err := os.ReadFile(snapshot); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing snapshot changed: %v", err)
	}
}

func TestRestoreRollbackKeepsPreviousFiles(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "snapshot")
	src := filepath.Join(dir, "new")
	marker := filepath.Join(dir, restoreMarker)
	os.WriteFile(dst, []byte("old"), 0o600)
	os.WriteFile(src, []byte("new"), 0o600)
	err := installRestore([]restoreReplacement{
		{dst: dst, src: src},
		{dst: filepath.Join(dir, "second"), src: filepath.Join(dir, "missing")},
	}, marker)
	if err == nil {
		t.Fatal("expected replacement error")
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "old" {
		t.Fatalf("rollback: %q %v", b, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("restore marker was not cleared: %v", err)
	}
}
