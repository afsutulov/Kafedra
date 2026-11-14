package secure

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"kafedra/internal/store"
)

// AuditRecord — запись журнала регистрации событий безопасности.
// Цепочка SHA-256 и контрольная точка позволяют обнаружить повреждение
// журнала, в том числе усечение его хвоста.
type AuditRecord struct {
	Seq    uint64    `json:"seq"`
	Time   time.Time `json:"time"`
	UserID string    `json:"user_id,omitempty"`
	Login  string    `json:"login,omitempty"`
	IP     string    `json:"ip,omitempty"`
	Event  string    `json:"event"`
	Object string    `json:"object,omitempty"`
	OK     bool      `json:"ok"`
	Detail string    `json:"detail,omitempty"`
	Prev   string    `json:"prev"`
	Hash   string    `json:"hash"`
}

func (r *AuditRecord) sum() string {
	c := *r
	c.Hash = ""
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Audit — журнал событий безопасности (файлы audit-ГГГГ-ММ.log, только дозапись).
type Audit struct {
	mu     sync.Mutex
	dir    string
	seq    uint64
	prev   string
	f      *os.File
	name   string
	failed error
	first  uint64
	anchor string
	// Echo, если задан, получает каждую запись (например, для вывода в journald/SIEM).
	Echo func(line []byte)
}

// AuditFiles возвращает имена файлов журнала в хронологическом порядке.
func AuditFiles(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "audit-*.log"))
	sort.Strings(m)
	return m
}

// ReadAuditFile читает записи одного файла журнала.
func ReadAuditFile(path string) ([]AuditRecord, error) {
	var out []AuditRecord
	err := scanAuditFile(path, func(r AuditRecord) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func scanAuditFile(path string, visit func(AuditRecord) error) error {
	return scanAuditPrefix(path, -1, visit)
}

func scanAuditPrefix(path string, limit int64, visit func(AuditRecord) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var input io.Reader = f
	if limit >= 0 {
		input = io.LimitReader(input, limit)
	}
	return scanAuditReader(input, filepath.Base(path), visit)
}

func scanAuditReader(input io.Reader, name string, visit func(AuditRecord) error) error {
	sc := bufio.NewScanner(input)
	// Записи старых версий могли превышать прежний лимит в 1 МиБ.
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for n := 1; sc.Scan(); n++ {
		var r AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return fmt.Errorf("%s: повреждена строка №%d", name, n)
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	return sc.Err()
}

// OpenAudit открывает журнал и восстанавливает состояние цепочки.
func OpenAudit(dir string) (*Audit, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &Audit{dir: dir}
	repaired, err := repairAuditTail(dir)
	if err != nil {
		return nil, err
	}
	n, last, head, err := verifyAudit(dir)
	if err != nil {
		return nil, fmt.Errorf("%w (порядок действий — в руководстве администратора, раздел «Журнал событий»)", err)
	}
	a.seq, a.prev = last, head
	a.first, a.anchor, err = auditStart(dir)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		if err := a.saveHead(); err != nil {
			return nil, err
		}
	}
	if repaired != "" {
		a.Log(AuditRecord{Event: "журнал событий: восстановление после сбоя", OK: true, Detail: repaired})
	}
	return a, nil
}

// repairAuditTail отбрасывает незавершённую последнюю строку последнего файла журнала:
// так выглядит запись, прерванная сбоем питания. Полные строки не затрагиваются.
func repairAuditTail(dir string) (string, error) {
	files := AuditFiles(dir)
	if len(files) == 0 {
		return "", nil
	}
	name := files[len(files)-1]
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return "", err
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], st.Size()-1); err != nil || last[0] == '\n' {
		return "", err
	}
	const maxTail = 32 << 20
	offset := max(st.Size()-maxTail, 0)
	b := make([]byte, st.Size()-offset)
	if _, err := f.ReadAt(b, offset); err != nil {
		return "", err
	}
	keep := bytes.LastIndexByte(b, '\n') + 1
	if keep == 0 && offset != 0 {
		return "", errors.New("слишком длинная последняя запись аудита")
	}
	var r AuditRecord
	if json.Unmarshal(b[keep:], &r) == nil {
		// Полная запись без перевода строки должна пройти обычную проверку хеша.
		if _, _, _, err := verifyAudit(dir); err != nil {
			return "", err
		}
		if _, err := f.WriteAt([]byte{'\n'}, st.Size()); err != nil {
			return "", err
		}
		return "", f.Sync()
	}
	checkpoint, err := readAuditHead(dir)
	if err != nil {
		return "", err
	}
	// Сначала сверяем сохранившийся префикс с контрольной точкой.
	if _, _, _, err := verifyAuditWithHead(dir, checkpoint, name, offset+int64(keep)); err != nil {
		return "", err
	}
	if err := f.Truncate(offset + int64(keep)); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return fmt.Sprintf("в файле %s отброшена незавершённая запись (%d байт)", filepath.Base(name), len(b)-keep), nil
}

const AuditHeadFile = "audit-head.json"

type auditHead struct {
	Seq    uint64 `json:"seq"`
	Hash   string `json:"hash"`
	First  uint64 `json:"first,omitempty"`
	Anchor string `json:"anchor,omitempty"`
}

func (a *Audit) saveHead() error {
	b, err := json.Marshal(auditHead{Seq: a.seq, Hash: a.prev, First: a.first, Anchor: a.anchor})
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(filepath.Join(a.dir, AuditHeadFile), b, 0o600)
}

func (a *Audit) Verify() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return VerifyAudit(a.dir)
}

func boundedText(s string) string {
	const maxBytes = 4096
	if len(s) > maxBytes {
		s = s[:maxBytes] + "…"
	}
	return strings.ToValidUTF8(s, "�")
}

// Log добавляет запись в журнал и обновляет контрольную точку после fsync.
func (a *Audit) Log(r AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.UserID, r.Login, r.IP = boundedText(r.UserID), boundedText(r.Login), boundedText(r.IP)
	r.Event, r.Object, r.Detail = boundedText(r.Event), boundedText(r.Object), boundedText(r.Detail)
	if err := a.append(r); err != nil {
		log.Printf("не удалось сохранить запись аудита: %v", err)
	}
}

func (a *Audit) append(r AuditRecord) error {
	if a.failed != nil {
		return a.failed
	}
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	r.Time = r.Time.UTC().Truncate(time.Millisecond)
	name := filepath.Join(a.dir, "audit-"+r.Time.Format("2006-01")+".log")
	if a.f == nil || a.name != name {
		if a.f != nil {
			a.f.Close()
			a.f = nil
		}
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		a.f, a.name = f, name
	}
	r.Seq, r.Prev = a.seq+1, a.prev
	r.Hash = r.sum()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	pos, err := a.f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err = a.f.Write(b); err == nil {
		err = a.f.Sync()
	}
	if err != nil {
		if rollback := a.f.Truncate(pos); rollback != nil {
			a.failed = errors.Join(err, rollback)
		}
		return err
	}
	a.seq, a.prev = r.Seq, r.Hash
	if a.first == 0 {
		a.first = r.Seq
	}
	if a.Echo != nil {
		a.Echo(b)
	}
	return a.saveHead()
}

// Close закрывает файл журнала.
func (a *Audit) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		a.f.Close()
		a.f = nil
	}
}

// VerifyAudit проверяет целостность цепочки всех файлов журнала.
// Возвращает число проверенных записей.
func VerifyAudit(dir string) (int, error) {
	n, _, _, err := verifyAudit(dir)
	return n, err
}

// verifyAudit проверяет непрерывность цепочки имеющихся файлов и контрольную точку.
// Файлы за самые ранние месяцы могут быть перенесены в архив: тогда цепочка начинается
// не с первой записи. Пропуск в середине или в конце считается нарушением.
// Возвращает число проверенных записей, номер и хеш последней записи.
func verifyAudit(dir string) (int, uint64, string, error) {
	checkpoint, err := readAuditHead(dir)
	if err != nil {
		return 0, 0, "", err
	}
	return verifyAuditWithHead(dir, checkpoint, "", -1)
}

func readAuditHead(dir string) (auditHead, error) {
	var checkpoint auditHead
	b, err := os.ReadFile(filepath.Join(dir, AuditHeadFile))
	if err == nil {
		if json.Unmarshal(b, &checkpoint) != nil || checkpoint.Seq == 0 || len(checkpoint.Hash) != sha256.Size*2 ||
			checkpoint.First > checkpoint.Seq || (checkpoint.First > 1 && len(checkpoint.Anchor) != sha256.Size*2) {
			return checkpoint, errors.New("повреждена контрольная точка журнала аудита")
		}
	} else if !os.IsNotExist(err) {
		return checkpoint, err
	}
	return checkpoint, nil
}

func auditStart(dir string) (uint64, string, error) {
	for _, name := range AuditFiles(dir) {
		var first AuditRecord
		err := scanAuditFile(name, func(r AuditRecord) error {
			first = r
			return io.EOF
		})
		if errors.Is(err, io.EOF) {
			return first.Seq, first.Prev, nil
		}
		if err != nil {
			return 0, "", err
		}
	}
	return 0, "", nil
}

// AcceptArchivedPrefix записывает новую границу после переноса начальных файлов журнала.
func AcceptArchivedPrefix(dir string) error {
	checkpoint, err := readAuditHead(dir)
	if err != nil {
		return err
	}
	first, anchor, err := auditStart(dir)
	if err != nil {
		return err
	}
	if checkpoint.Seq == 0 || first < 2 || first < checkpoint.First || first > checkpoint.Seq {
		return errors.New("невозможно подтвердить архивирование по контрольной точке")
	}
	checkpoint.First, checkpoint.Anchor = first, anchor
	_, last, head, err := verifyAuditWithHead(dir, checkpoint, "", -1)
	if err != nil {
		return err
	}
	a := &Audit{dir: dir, seq: last, prev: head, first: first, anchor: anchor}
	return a.saveHead()
}

func verifyAuditWithHead(dir string, checkpoint auditHead, lastFile string, lastLimit int64) (int, uint64, string, error) {
	prev, n := "", 0
	var seq uint64
	first := true
	for _, f := range AuditFiles(dir) {
		limit := int64(-1)
		if f == lastFile {
			limit = lastLimit
		}
		err := scanAuditPrefix(f, limit, func(r AuditRecord) error {
			if first {
				expected := max(checkpoint.First, 1)
				if r.Seq != expected || r.Prev != checkpoint.Anchor {
					return errors.New("изменилось начало журнала: проверьте архив или подтвердите перенос через audit-verify -accept-archived-prefix")
				}
				prev, seq = checkpoint.Anchor, expected-1
			}
			first = false
			if r.Prev != prev || r.Seq != seq+1 || r.sum() != r.Hash || (r.Seq == checkpoint.Seq && r.Hash != checkpoint.Hash) {
				return fmt.Errorf("нарушена целостность журнала: файл %s, запись №%d", filepath.Base(f), r.Seq)
			}
			prev, seq = r.Hash, r.Seq
			n++
			return nil
		})
		if err != nil {
			return n, seq, prev, err
		}
	}
	if seq < checkpoint.Seq {
		return n, seq, prev, fmt.Errorf("журнал усечён: последняя запись №%d, контрольная точка №%d", seq, checkpoint.Seq)
	}
	return n, seq, prev, nil
}
