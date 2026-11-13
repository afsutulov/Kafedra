package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

type doc struct {
	Name string
	Tags []string
}

func TestPersistenceAndRecovery(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Register[doc](db, "docs")
	c.Put("a", doc{Name: "один", Tags: []string{"x"}})
	c.Put("b", doc{Name: "два"})
	c.Update("a", func(d *doc) error { d.Tags = append(d.Tags, "y"); return nil })
	c.Delete("b")
	// имитация сбоя: журнал не сжат, в хвосте мусор
	db.jf.Close()
	db.lock.Close() // «гибель» процесса снимает блокировку
	f, _ := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("deadbeef {\"s\":99,оборвано")
	f.Close()

	db2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(db2.Warnings) != 1 {
		t.Fatalf("ожидалось предупреждение об усечении, получено %v", db2.Warnings)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("повторное открытие занятого хранилища должно быть запрещено")
	}
	c2, _ := Register[doc](db2, "docs")
	a, ok := c2.Get("a")
	if !ok || len(a.Tags) != 2 || a.Name != "один" {
		t.Fatalf("документ не восстановлен: %+v", a)
	}
	if _, ok := c2.Get("b"); ok {
		t.Fatal("удалённый документ восстановлен")
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}
	db3, _ := Open(dir)
	c3, _ := Register[doc](db3, "docs")
	if c3.Count(nil) != 1 {
		t.Fatal("снимок не загружен")
	}
	// изоляция: изменение полученной копии не влияет на хранилище
	g, _ := c3.Get("a")
	g.Tags[0] = "изменено"
	g2, _ := c3.Get("a")
	if g2.Tags[0] != "x" {
		t.Fatal("нарушена изоляция копий")
	}
}

func TestCorruptJournalRecordDoesNotDiscardValidSuffix(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Register[doc](db, "docs")
	for _, id := range []string{"a", "b", "c"} {
		if err := c.Put(id, doc{Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	db.Release()
	p := filepath.Join(dir, "journal.jsonl")
	data, _ := os.ReadFile(p)
	data[bytes.IndexByte(data, '\n')+1] = '!'
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir); err == nil {
		reopened.Release()
		t.Fatal("corrupted journal accepted")
	}
	if after, err := os.ReadFile(p); err != nil || !bytes.Equal(data, after) {
		t.Fatalf("journal changed: %v", err)
	}
	lock, err := LockDir(dir)
	if err != nil {
		t.Fatalf("failed Open leaked lock: %v", err)
	}
	lock.Close()
}
