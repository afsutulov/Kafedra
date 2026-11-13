package app

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"kafedra/internal/secure"
	"kafedra/internal/store"
)

const restoreMarker = "restore-in-progress"

var (
	fileIDPattern    = regexp.MustCompile(`^[0-9a-f]{24}$`)
	auditFilePattern = regexp.MustCompile(`^audit-[0-9]{4}-[0-9]{2}\.log$`)
)

type backupManifest struct {
	Product   string    `json:"product"`
	Version   string    `json:"version"`
	Created   time.Time `json:"created"`
	WithKey   bool      `json:"with_key"`
	KeyDigest string    `json:"key_sha256,omitempty"`
	AuditHead bool      `json:"audit_head,omitempty"`
	Damaged   []string  `json:"damaged_files,omitempty"` // вложения, отсутствовавшие или повреждённые на момент копирования
}

func (c *Ctx) adminBackup() {
	f, err := os.CreateTemp(filepath.Join(c.Cfg.DataPath(), "backup"), ".download-*")
	if err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось подготовить резервную копию.")
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	// под блокировкой записи — только снимок базы и перечень файлов; копирование идёт после неё
	c.writeMu.Lock()
	allowed := c.recheck(RoleAdmin)
	var plan *backupPlan
	if allowed {
		plan, err = c.backupPrepare()
	}
	c.writeMu.Unlock()
	if !allowed {
		c.Deny()
		return
	}
	var damaged []string
	if plan != nil {
		defer c.releaseBackupFiles()
	}
	if err == nil {
		damaged, err = c.backupWrite(f, plan, false)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		c.Log("создание резервной копии", "", false, err.Error())
		c.Fail(http.StatusServiceUnavailable, "Не удалось создать резервную копию.")
		return
	}
	c.Log("создание резервной копии", "", len(damaged) == 0, "выгрузка через веб-интерфейс")
	c.reportDamaged(damaged)
	c.W.Header().Set("Content-Type", "application/zip")
	c.W.Header().Set("Content-Disposition", `attachment; filename="kafedra-backup-`+time.Now().In(c.Loc).Format("20060102-1504")+`.zip"`)
	http.ServeContent(c.W, c.R, "", time.Time{}, f)
}

// reportDamaged регистрирует вложения, которые отсутствуют на диске или не прошли сверку.
// Копия при этом создаётся: одна испорченная запись не должна оставлять систему без копий.
func (a *App) reportDamaged(ids []string) {
	if len(ids) == 0 {
		return
	}
	detail := fmt.Sprintf("отсутствуют или повреждены вложения (%d): %s", len(ids), strings.Join(ids, ", "))
	log.Printf("ВНИМАНИЕ: резервная копия создана, но %s", detail)
	a.Audit.Log(secure.AuditRecord{Event: "резервное копирование: повреждённые вложения", OK: false, Detail: detail})
}

// Backup записывает резервную копию (ZIP): снимок базы, вложения, журнал событий.
// Блокировка записи удерживается только на время снятия снимка базы.
func (a *App) Backup(w io.Writer, withKey bool) error {
	_, err := a.backup(w, withKey)
	return err
}

func (a *App) backup(w io.Writer, withKey bool) (bool, error) {
	a.writeMu.Lock()
	plan, err := a.backupPrepare()
	a.writeMu.Unlock()
	if err != nil {
		return false, err
	}
	defer a.releaseBackupFiles()
	damaged, err := a.backupWrite(w, plan, withKey)
	if err == nil {
		a.reportDamaged(damaged)
	}
	return len(damaged) != 0, err
}

type backupPlan struct {
	snap   []byte
	files  []FileMeta
	key    []byte
	digest string
}

// backupPrepare фиксирует согласованное состояние базы. Вызывать при захваченной writeMu.
func (a *App) backupPrepare() (*backupPlan, error) {
	snap, err := a.DB.Snapshot()
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(filepath.Join(a.Cfg.DataPath(), "master.key"))
	if err != nil {
		return nil, err
	}
	digest, err := masterKeyDigest(key)
	if err != nil {
		return nil, err
	}
	a.fileMu.Lock()
	a.backupReaders++
	a.fileMu.Unlock()
	return &backupPlan{snap: snap, files: a.Files.All(), key: key, digest: digest}, nil
}

func (a *App) releaseBackupFiles() {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	a.backupReaders--
	if a.backupReaders == 0 {
		for _, id := range a.pendingFileRemovals {
			os.Remove(a.filePath(id))
		}
		a.pendingFileRemovals = nil
	}
}

// backupWrite формирует архив. Возвращает идентификаторы вложений, которые отсутствуют
// на диске либо не совпали по размеру или контрольной сумме; они перечисляются в манифесте.
func (a *App) backupWrite(w io.Writer, plan *backupPlan, withKey bool) ([]string, error) {
	manifest := backupManifest{
		Product: "kafedra", Version: a.Version, Created: time.Now().UTC(),
		WithKey: withKey, KeyDigest: plan.digest,
	}
	zw := zip.NewWriter(w)
	if err := writeZIPEntry(zw, "db/snapshot.json", plan.snap); err != nil {
		return nil, err
	}
	for _, m := range plan.files {
		if !fileIDPattern.MatchString(m.ID) {
			return nil, fmt.Errorf("некорректный идентификатор файла %q", m.ID)
		}
		name := "files/" + m.ID[:2] + "/" + m.ID
		if err := copyToZIP(zw, name, a.filePath(m.ID), &m); err != nil {
			if !errors.Is(err, errFileDamaged) && !os.IsNotExist(err) {
				return nil, fmt.Errorf("вложение %s: %w", m.ID, err)
			}
			// файл удалён после снятия снимка, пропал или испорчен: копия продолжается
			manifest.Damaged = append(manifest.Damaged, m.ID)
		}
	}
	if err := a.Audit.WithFiles(func(files []string) error {
		for _, filename := range files {
			name := filepath.Base(filename)
			manifest.AuditHead = manifest.AuditHead || name == secure.AuditHeadFile
			if err := copyToZIP(zw, "audit/"+name, filename, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if withKey {
		if err := writeZIPEntry(zw, "master.key", plan.key); err != nil {
			return nil, err
		}
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeZIPEntry(zw, "manifest.json", encoded); err != nil {
		return nil, err
	}
	return manifest.Damaged, zw.Close()
}

var errFileDamaged = errors.New("размер или контрольная сумма файла не совпадает")

func writeZIPEntry(zw *zip.Writer, name string, data []byte) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func copyToZIP(zw *zip.Writer, name, source string, meta *FileMeta) error {
	src, err := os.Open(source)
	if err != nil {
		return err // отсутствующий файл не создаёт запись в архиве
	}
	defer src.Close()
	if meta != nil {
		if err := copyVerifiedFile(io.Discard, src, meta); err != nil {
			return err
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	dst, err := zw.Create(name)
	if err != nil {
		return err
	}
	if meta != nil {
		if err := copyVerifiedFile(dst, src, meta); err != nil {
			// Ошибка после начала записи ZIP недопустима для частичной копии.
			return fmt.Errorf("вложение изменилось при копировании: %v", err)
		}
		return nil
	}
	_, err = io.Copy(dst, src)
	return err
}

func copyVerifiedFile(dst io.Writer, src io.Reader, meta *FileMeta) error {
	h := sha256.New()
	if meta.Size < 0 || meta.Size >= 1<<62 {
		return errors.New("недопустимый размер вложения")
	}
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, meta.Size+1))
	if err != nil {
		return err
	}
	if n != meta.Size || hex.EncodeToString(h.Sum(nil)) != meta.SHA256 {
		return errFileDamaged
	}
	return nil
}

func masterKeyDigest(key []byte) (string, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(string(key)))
	if err != nil || len(raw) != 32 {
		return "", errors.New("некорректный мастер-ключ")
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func validBackupEntry(name string) bool {
	if name == "manifest.json" || name == "db/snapshot.json" || name == "master.key" {
		return true
	}
	parts := strings.Split(name, "/")
	if len(parts) == 2 && parts[0] == "audit" {
		return parts[1] == secure.AuditHeadFile || auditFilePattern.MatchString(parts[1])
	}
	return len(parts) == 3 && parts[0] == "files" && fileIDPattern.MatchString(parts[2]) && parts[1] == parts[2][:2]
}

// Restore проверяет копию во временном каталоге и удерживает блокировку до конца замены.
func Restore(zipPath, dataDir, auditDir string, allowIncomplete ...bool) error {
	if auditDir == "" {
		auditDir = filepath.Join(dataDir, "audit")
	}
	filesDir := filepath.Join(dataDir, "files")
	absFiles, err := filepath.Abs(filesDir)
	if err != nil {
		return err
	}
	absAudit, err := filepath.Abs(auditDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absFiles, absAudit)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("каталог аудита не должен находиться внутри каталога вложений")
	}
	dbDir := filepath.Join(dataDir, "db")
	lock, err := store.LockDir(dbDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	stage, err := os.MkdirTemp(dataDir, ".restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, sub := range []string{"db", "files", "audit"} {
		if err := os.Mkdir(filepath.Join(stage, sub), 0o700); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		name := f.Name
		if name != path.Clean(name) || !validBackupEntry(name) || seen[name] || !f.Mode().IsRegular() {
			return fmt.Errorf("недопустимая запись резервной копии %q", name)
		}
		seen[name] = true
		if err := extractBackupFile(f, filepath.Join(stage, filepath.FromSlash(name))); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if !seen["db/snapshot.json"] {
		return errors.New("в архиве отсутствует снимок базы")
	}
	if err := validateRestoredData(stage, dataDir, seen, len(allowIncomplete) != 0 && allowIncomplete[0]); err != nil {
		return err
	}
	// Готовим замену на том же томе и сохраняем остальные файлы каталога аудита.
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		return err
	}
	auditStage, err := os.MkdirTemp(auditDir, ".restore-audit-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(auditStage)
	auditEntries, err := os.ReadDir(filepath.Join(stage, "audit"))
	if err != nil {
		return err
	}
	oldAuditEntries, err := os.ReadDir(auditDir)
	if err != nil {
		return err
	}
	var auditReplacements []restoreReplacement
	for _, entry := range oldAuditEntries {
		name := entry.Name()
		if validBackupEntry("audit/"+name) && !seen["audit/"+name] {
			if !entry.Type().IsRegular() {
				return fmt.Errorf("недопустимый файл аудита %q", name)
			}
			auditReplacements = append(auditReplacements, restoreReplacement{dst: filepath.Join(auditDir, name)})
		}
	}
	for _, entry := range auditEntries {
		name := entry.Name()
		src := filepath.Join(auditStage, name)
		dst := filepath.Join(auditDir, name)
		if info, err := os.Lstat(dst); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("недопустимый файл аудита %q", name)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := copyRestoreFile(filepath.Join(stage, "audit", name), src); err != nil {
			return err
		}
		auditReplacements = append(auditReplacements, restoreReplacement{dst: dst, src: src})
	}
	for _, dir := range []string{auditStage, filepath.Join(stage, "files"), stage} {
		if err := store.SyncDir(dir); err != nil {
			return err
		}
	}
	replacements := []restoreReplacement{
		{dst: filepath.Join(dbDir, "snapshot.json"), src: filepath.Join(stage, "db", "snapshot.json")},
		{dst: filepath.Join(dbDir, "journal.jsonl")},
		{dst: filepath.Join(dataDir, "files"), src: filepath.Join(stage, "files")},
	}
	replacements = append(replacements, auditReplacements...)
	if seen["master.key"] {
		replacements = append(replacements, restoreReplacement{dst: filepath.Join(dataDir, "master.key"), src: filepath.Join(stage, "master.key")})
	}
	return installRestore(replacements, filepath.Join(dbDir, restoreMarker))
}

func extractBackupFile(f *zip.File, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeRestoreFile(dst, rc)
}

func writeRestoreFile(dst string, src io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, src)
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err == nil {
		err = store.SyncDir(filepath.Dir(dst))
	}
	return err
}

func copyRestoreFile(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeRestoreFile(dst, f)
}

func validateRestoredData(stage, dataDir string, seen map[string]bool, allowIncomplete bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("повреждён снимок базы: %v", r)
		}
	}()
	db, err := store.Open(filepath.Join(stage, "db"))
	if err != nil {
		return err
	}
	defer db.Release()
	a := new(App)
	a.registerCollections(db)
	var manifest backupManifest
	if seen["manifest.json"] {
		b, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &manifest); err != nil || manifest.Product != "kafedra" || manifest.WithKey != seen["master.key"] {
			return errors.New("некорректный манифест резервной копии")
		}
	}
	damaged := map[string]bool{}
	for _, id := range manifest.Damaged {
		if !fileIDPattern.MatchString(id) || damaged[id] {
			return errors.New("некорректный список повреждённых вложений")
		}
		damaged[id] = true
	}
	if len(damaged) > 0 && !allowIncomplete {
		return fmt.Errorf("копия неполная: повреждённых вложений %d; для восстановления без них требуется -allow-incomplete", len(damaged))
	}
	for _, f := range a.Files.All() {
		if !fileIDPattern.MatchString(f.ID) {
			return fmt.Errorf("недопустимый идентификатор вложения %q", f.ID)
		}
		if err := verifyRestoredFile(stage, &f); err != nil {
			if !damaged[f.ID] {
				return fmt.Errorf("вложение %s: %w", f.ID, err)
			}
			log.Printf("восстановление без повреждённого вложения %s (%s)", f.ID, f.Name)
			if err := os.Remove(filepath.Join(stage, "files", f.ID[:2], f.ID)); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := a.Files.Delete(f.ID); err != nil {
				return err
			}
		}
	}
	if _, err := secure.VerifyAudit(filepath.Join(stage, "audit")); err != nil {
		return err
	}
	if manifest.AuditHead && !seen["audit/"+secure.AuditHeadFile] {
		return errors.New("в копии отсутствует контрольная точка аудита")
	}
	keyDir := dataDir
	if seen["master.key"] {
		keyDir = stage
	}
	if a.Users.Count(nil) > 0 || seen["master.key"] || manifest.KeyDigest != "" {
		b, err := os.ReadFile(filepath.Join(keyDir, "master.key"))
		if err != nil {
			return fmt.Errorf("для восстановления необходим исходный мастер-ключ: %w", err)
		}
		digest, err := masterKeyDigest(b)
		if err != nil {
			return err
		}
		if manifest.KeyDigest != "" && digest != manifest.KeyDigest {
			return errors.New("мастер-ключ не соответствует резервной копии")
		}
	}
	for _, session := range a.Sessions.All() {
		if err := a.Sessions.Delete(session.ID); err != nil {
			return err
		}
	}
	return db.Compact()
}

func verifyRestoredFile(stage string, meta *FileMeta) error {
	src, err := os.Open(filepath.Join(stage, "files", meta.ID[:2], meta.ID))
	if err != nil {
		return err
	}
	defer src.Close()
	return copyVerifiedFile(io.Discard, src, meta)
}

type restoreReplacement struct {
	dst       string
	src       string
	old       string
	installed bool
}

func installRestore(replacements []restoreReplacement, marker string) error {
	_, markerErr := os.Stat(marker)
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return markerErr
	}
	if err := store.WriteFileAtomic(marker, []byte("restore\n"), 0o600); err != nil {
		return err
	}
	for i := range replacements {
		if err := replacements[i].install(); err != nil {
			var rollback error
			for j := i; j >= 0; j-- {
				rollback = errors.Join(rollback, replacements[j].rollback())
			}
			if rollback == nil && os.IsNotExist(markerErr) {
				rollback = clearRestoreMarker(marker)
			}
			return errors.Join(err, rollback)
		}
	}
	if err := clearRestoreMarker(marker); err != nil {
		return err
	}
	for _, r := range replacements {
		if r.old != "" {
			os.RemoveAll(filepath.Dir(r.old))
		}
	}
	return nil
}

func (r *restoreReplacement) install() error {
	dir := filepath.Dir(r.dst)
	if _, err := os.Lstat(r.dst); err == nil {
		previous, err := os.MkdirTemp(dir, ".pre-restore-*")
		if err != nil {
			return err
		}
		old := filepath.Join(previous, "previous")
		if err := os.Rename(r.dst, old); err != nil {
			os.Remove(previous)
			return err
		}
		r.old = old
		if err := store.SyncDir(previous); err != nil {
			return err
		}
		if err := store.SyncDir(dir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if r.src != "" {
		if err := os.Rename(r.src, r.dst); err != nil {
			return err
		}
		r.installed = true
	}
	return store.SyncDir(dir)
}

func (r *restoreReplacement) rollback() error {
	if r.installed {
		if err := os.RemoveAll(r.dst); err != nil {
			return err
		}
	}
	if r.old != "" {
		if err := os.Rename(r.old, r.dst); err != nil {
			return fmt.Errorf("исходные данные сохранены в %s: %w", r.old, err)
		}
		if err := os.Remove(filepath.Dir(r.old)); err != nil {
			return err
		}
	}
	return store.SyncDir(filepath.Dir(r.dst))
}

func clearRestoreMarker(marker string) error {
	if err := os.Remove(marker); err != nil {
		return err
	}
	return store.SyncDir(filepath.Dir(marker))
}
