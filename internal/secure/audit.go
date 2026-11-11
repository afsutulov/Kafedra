package secure

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// AuditRecord — запись журнала регистрации событий безопасности.
// Записи связаны в цепочку: Hash = SHA-256(Prev + содержимое записи), поэтому
// удаление или изменение любой записи обнаруживается проверкой VerifyAudit.
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
	mu   sync.Mutex
	dir  string
	seq  uint64
	prev string
	f    *os.File
	name string
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []AuditRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return out, fmt.Errorf("%s: повреждённая запись после №%d", filepath.Base(path), len(out))
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// OpenAudit открывает журнал и восстанавливает состояние цепочки.
func OpenAudit(dir string) (*Audit, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &Audit{dir: dir}
	files := AuditFiles(dir)
	for i := len(files) - 1; i >= 0; i-- {
		recs, err := ReadAuditFile(files[i])
		if err != nil {
			return nil, err
		}
		if len(recs) > 0 {
			last := recs[len(recs)-1]
			a.seq, a.prev = last.Seq, last.Hash
			break
		}
	}
	return a, nil
}

// Log добавляет запись в журнал.
func (a *Audit) Log(r AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	r.Time = r.Time.UTC().Truncate(time.Millisecond)
	name := filepath.Join(a.dir, "audit-"+r.Time.Format("2006-01")+".log")
	if a.f == nil || a.name != name {
		if a.f != nil {
			a.f.Close()
		}
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ОШИБКА: журнал аудита недоступен:", err)
			return
		}
		a.f, a.name = f, name
	}
	a.seq++
	r.Seq, r.Prev = a.seq, a.prev
	r.Hash = r.sum()
	a.prev = r.Hash
	b, _ := json.Marshal(r)
	b = append(b, '\n')
	a.f.Write(b)
	a.f.Sync()
	if a.Echo != nil {
		a.Echo(b)
	}
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
	prev, n := "", 0
	var seq uint64
	for _, f := range AuditFiles(dir) {
		recs, err := ReadAuditFile(f)
		if err != nil {
			return n, err
		}
		for _, r := range recs {
			if r.Prev != prev || r.Seq != seq+1 || r.sum() != r.Hash {
				return n, fmt.Errorf("нарушена целостность журнала: файл %s, запись №%d", filepath.Base(f), r.Seq)
			}
			prev, seq = r.Hash, r.Seq
			n++
		}
	}
	return n, nil
}
