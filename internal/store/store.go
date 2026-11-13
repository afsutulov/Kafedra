package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ErrNotFound возвращается при обращении к отсутствующему документу.
var ErrNotFound = errors.New("документ не найден")

type dumper interface {
	dump() (map[string]json.RawMessage, error)
}

// DB — экземпляр хранилища.
type DB struct {
	mu       sync.RWMutex
	dir      string
	raw      map[string]map[string]json.RawMessage // коллекции, ещё не привязанные к типу
	colls    map[string]dumper
	jf       *os.File
	lock     *os.File
	seq      uint64
	jsize    int64
	writeErr error
	// SyncWrites включает fsync после каждой операции записи (надёжность важнее скорости).
	SyncWrites bool
	// CompactAt — размер журнала в байтах, при превышении которого выполняется сжатие.
	CompactAt int64
	// Warnings — предупреждения, возникшие при восстановлении журнала.
	Warnings []string
}

type snapshot struct {
	Seq   uint64                                `json:"seq"`
	Colls map[string]map[string]json.RawMessage `json:"colls"`
}

type entry struct {
	S uint64          `json:"s"`
	C string          `json:"c"`
	O string          `json:"o"` // p — запись, d — удаление
	I string          `json:"i"`
	D json.RawMessage `json:"d,omitempty"`
}

// LockDir исключает одновременный запуск службы и изменение данных при восстановлении.
func LockDir(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("не удалось заблокировать хранилище: %w", err)
	}
	return f, nil
}

func Open(dir string) (*DB, error) {
	lock, err := LockDir(dir)
	if err != nil {
		return nil, err
	}
	db, err := OpenLocked(dir, lock)
	if err != nil {
		lock.Close()
	}
	return db, err
}

// OpenLocked принимает владение блокировкой только при успешном открытии.
func OpenLocked(dir string, lock *os.File) (*DB, error) {
	db := &DB{dir: dir, raw: map[string]map[string]json.RawMessage{}, colls: map[string]dumper{},
		SyncWrites: true, CompactAt: 32 << 20, lock: lock}
	if b, err := os.ReadFile(filepath.Join(dir, "snapshot.json")); err == nil {
		var s snapshot
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("повреждён снимок хранилища: %w", err)
		}
		if s.Colls == nil {
			return nil, errors.New("в снимке хранилища отсутствуют коллекции")
		}
		db.seq, db.raw = s.Seq, s.Colls
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	jpath := filepath.Join(dir, "journal.jsonl")
	if f, err := os.Open(jpath); err == nil {
		good, err := db.replay(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		if st, _ := os.Stat(jpath); st != nil && st.Size() != good {
			db.Warnings = append(db.Warnings, fmt.Sprintf("журнал усечён до %d байт: отброшена незавершённая запись", good))
			if err := os.Truncate(jpath, good); err != nil {
				return nil, err
			}
		}
		db.jsize = good
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(jpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := SyncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	db.jf = f
	return db, nil
}

// replay применяет записи журнала; возвращает длину корректной части файла.
func (db *DB) replay(r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var good int64
	for {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return good, nil // последняя строка без перевода строки не была записана целиком
		}
		if err != nil {
			return good, err
		}
		corrupt := fmt.Errorf("повреждена полная запись журнала по смещению %d", good)
		body := bytes.TrimRight(line, "\n")
		if len(body) < 10 || body[8] != ' ' {
			return good, corrupt
		}
		var sum uint32
		if _, e := fmt.Sscanf(string(body[:8]), "%08x", &sum); e != nil || crc32.ChecksumIEEE(body[9:]) != sum {
			return good, corrupt
		}
		var e entry
		if json.Unmarshal(body[9:], &e) != nil || e.S == 0 || e.C == "" || e.I == "" ||
			(e.O != "d" && (e.O != "p" || len(e.D) == 0)) {
			return good, corrupt
		}
		good += int64(len(line))
		if e.S <= db.seq {
			continue // уже учтено в снимке
		}
		if e.S != db.seq+1 {
			return good - int64(len(line)), corrupt
		}
		db.seq = e.S
		m := db.raw[e.C]
		if m == nil {
			m = map[string]json.RawMessage{}
			db.raw[e.C] = m
		}
		if e.O == "d" {
			delete(m, e.I)
		} else {
			m[e.I] = e.D
		}
	}
}

func (db *DB) appendEntry(c, op, id string, data []byte) error {
	if db.writeErr != nil {
		return db.writeErr
	}
	b, err := json.Marshal(entry{S: db.seq + 1, C: c, O: op, I: id, D: data})
	if err != nil {
		return err
	}
	line := fmt.Appendf(nil, "%08x %s\n", crc32.ChecksumIEEE(b), b)
	_, err = db.jf.Write(line)
	if err == nil && db.SyncWrites {
		err = db.jf.Sync()
	}
	if err != nil {
		rollback := db.jf.Truncate(db.jsize)
		if rollback == nil {
			rollback = db.jf.Sync()
		}
		if rollback != nil {
			db.writeErr = fmt.Errorf("запись журнала заблокирована после сбоя: %w", errors.Join(err, rollback))
			return db.writeErr
		}
		return err
	}
	db.seq++
	db.jsize += int64(len(line))
	return nil
}

func (db *DB) buildSnapshot() ([]byte, error) {
	s := snapshot{Seq: db.seq, Colls: map[string]map[string]json.RawMessage{}}
	for name, m := range db.raw {
		s.Colls[name] = m
	}
	for name, c := range db.colls {
		m, err := c.dump()
		if err != nil {
			return nil, err
		}
		s.Colls[name] = m
	}
	return json.MarshalIndent(s, "", "  ") // с отступами: файл читается человеком
}

// Snapshot возвращает согласованный снимок всех данных (для резервного копирования).
func (db *DB) Snapshot() ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.buildSnapshot()
}

// Compact записывает снимок на диск и очищает журнал.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.compactLocked()
}

func (db *DB) compactLocked() error {
	if db.writeErr != nil {
		return db.writeErr
	}
	b, err := db.buildSnapshot()
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(db.dir, "snapshot.json"), b, 0o600); err != nil {
		return err
	}
	if err := db.jf.Truncate(0); err != nil {
		return err
	}
	db.jsize = 0
	return db.jf.Sync()
}

// Close выполняет сжатие и закрывает файлы.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	err := db.compactLocked()
	return errors.Join(err, db.Release())
}

// Release закрывает дескрипторы без изменения файлов.
func (db *DB) Release() error {
	return errors.Join(db.jf.Close(), db.lock.Close())
}

// WriteFileAtomic записывает файл через временный файл с fsync и переименованием.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Coll — типизированная коллекция документов.
type Coll[T any] struct {
	db   *DB
	name string
	m    map[string]T
}

// Register привязывает коллекцию name к типу T и загружает её документы.
func Register[T any](db *DB, name string) (*Coll[T], error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, dup := db.colls[name]; dup {
		return nil, fmt.Errorf("коллекция %q уже зарегистрирована", name)
	}
	c := &Coll[T]{db: db, name: name, m: map[string]T{}}
	for id, raw := range db.raw[name] {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("коллекция %s, документ %s: %w", name, id, err)
		}
		c.m[id] = v
	}
	delete(db.raw, name)
	db.colls[name] = c
	return c, nil
}

func (c *Coll[T]) dump() (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(c.m))
	for id, v := range c.m {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out[id] = b
	}
	return out, nil
}

func (c *Coll[T]) putLocked(id string, v T) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var stored T // независимая копия: хранимые значения никогда не разделяют память с вызывающим кодом
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	if err := c.db.appendEntry(c.name, "p", id, b); err != nil {
		return err
	}
	c.m[id] = stored
	if c.db.CompactAt > 0 && c.db.jsize > c.db.CompactAt {
		if err := c.db.compactLocked(); err != nil {
			log.Printf("не удалось свернуть журнал хранилища: %v", err)
		}
	}
	return nil
}

// Put сохраняет документ под идентификатором id.
func (c *Coll[T]) Put(id string, v T) error {
	if id == "" {
		return errors.New("пустой идентификатор")
	}
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.putLocked(id, v)
}

// Get возвращает глубокую копию документа.
func (c *Coll[T]) Get(id string) (T, bool) {
	c.db.mu.RLock()
	v, ok := c.m[id]
	c.db.mu.RUnlock()
	if !ok {
		return v, false
	}
	return clone(v), true
}

func clone[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	json.Unmarshal(b, &out)
	return out
}

// Update атомарно изменяет документ функцией fn.
func (c *Coll[T]) Update(id string, fn func(*T) error) error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	v, ok := c.m[id]
	if !ok {
		return ErrNotFound
	}
	cp := clone(v)
	if err := fn(&cp); err != nil {
		return err
	}
	return c.putLocked(id, cp)
}

// Delete удаляет документ (отсутствие документа ошибкой не считается).
func (c *Coll[T]) Delete(id string) error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	if _, ok := c.m[id]; !ok {
		return nil
	}
	if err := c.db.appendEntry(c.name, "d", id, nil); err != nil {
		return err
	}
	delete(c.m, id)
	return nil
}

// Find возвращает документы, удовлетворяющие условию (nil — все).
// Возвращённые значения предназначены только для чтения.
func (c *Coll[T]) Find(pred func(*T) bool) []T {
	c.db.mu.RLock()
	defer c.db.mu.RUnlock()
	var out []T
	for _, v := range c.m {
		if pred == nil || pred(&v) {
			out = append(out, v)
		}
	}
	return out
}

// All возвращает все документы коллекции.
func (c *Coll[T]) All() []T { return c.Find(nil) }

// First возвращает первый документ, удовлетворяющий условию.
func (c *Coll[T]) First(pred func(*T) bool) (T, bool) {
	c.db.mu.RLock()
	defer c.db.mu.RUnlock()
	for _, v := range c.m {
		if pred(&v) {
			return clone(v), true
		}
	}
	var zero T
	return zero, false
}

// Count возвращает число документов, удовлетворяющих условию.
func (c *Coll[T]) Count(pred func(*T) bool) int {
	c.db.mu.RLock()
	defer c.db.mu.RUnlock()
	if pred == nil {
		return len(c.m)
	}
	n := 0
	for _, v := range c.m {
		if pred(&v) {
			n++
		}
	}
	return n
}
