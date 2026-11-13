package secure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPasswords(t *testing.T) {
	Iterations = 1000
	k, _ := NewKeys(make([]byte, 32))
	h := k.HashPassword("Сложный-Пароль-2026")
	if ok, _ := k.VerifyPassword("Сложный-Пароль-2026", h); !ok {
		t.Fatal("верный пароль отвергнут")
	}
	if ok, _ := k.VerifyPassword("другой", h); ok {
		t.Fatal("неверный пароль принят")
	}
	k2, _ := NewKeys(append(make([]byte, 31), 1))
	if ok, _ := k2.VerifyPassword("Сложный-Пароль-2026", h); ok {
		t.Fatal("хеш проверен без исходного мастер-ключа")
	}
	Iterations = 2000
	if _, rehash := k.VerifyPassword("Сложный-Пароль-2026", h); !rehash {
		t.Fatal("не запрошено обновление хеша")
	}
}

func TestTOTP(t *testing.T) {
	// контрольный вектор RFC 6238 (SHA-1, секрет "12345678901234567890", t=59 → 94287082)
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	if c := TOTPCode(secret, time.Unix(59, 0)); c != "287082" {
		t.Fatalf("код %s, ожидался 287082", c)
	}
	now := time.Now()
	step, ok := VerifyTOTP(secret, TOTPCode(secret, now), now, 0, 1)
	if !ok {
		t.Fatal("код не принят")
	}
	if _, ok := VerifyTOTP(secret, TOTPCode(secret, now), now, step, 1); ok {
		t.Fatal("код принят повторно")
	}
}

func TestSealAndPolicy(t *testing.T) {
	k, _ := NewKeys(make([]byte, 32))
	if p, err := k.Open(k.Seal([]byte("секрет"))); err != nil || string(p) != "секрет" {
		t.Fatal("ошибка шифрования")
	}
	pol := Policy{MinLength: 12, NeedUpper: true, NeedLower: true, NeedDigit: true, NeedSymbol: true}
	if len(CheckPassword("short", "u", pol)) == 0 {
		t.Fatal("слабый пароль принят")
	}
	for i := 0; i < 50; i++ {
		if e := CheckPassword(GeneratePassword(16), "user", pol); len(e) != 0 {
			t.Fatalf("сгенерированный пароль не прошёл политику: %v", e)
		}
	}
}

func TestAuditChain(t *testing.T) {
	dir := t.TempDir()
	a, _ := OpenAudit(dir)
	for i := 0; i < 5; i++ {
		a.Log(AuditRecord{Event: "тест", Login: "admin", OK: true})
	}
	a.Close()
	a, _ = OpenAudit(dir) // продолжение цепочки после перезапуска
	a.Log(AuditRecord{Event: "после перезапуска", OK: true})
	a.Close()
	if n, err := VerifyAudit(dir); err != nil || n != 6 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	f := AuditFiles(dir)[0]
	b, _ := os.ReadFile(f)
	os.WriteFile(filepath.Join(dir, filepath.Base(f)), []byte(strings.Replace(string(b), `"login":"admin"`, `"login":"guest"`, 1)), 0o600)
	if _, err := VerifyAudit(dir); err == nil {
		t.Fatal("подмена записи не обнаружена")
	}
}

func TestAuditTailDeletionAndLegacyLargeRecord(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := AuditRecord{Seq: 1, Time: now, Event: "legacy", Login: strings.Repeat("a", 1100000)}
	record.Hash = record.sum()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "audit-"+now.Format("2006-01")+".log")
	legacy := append(raw, '\n')
	if err := os.WriteFile(file, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.Log(AuditRecord{Time: now, Event: "new", Detail: strings.Repeat("x", 1100000)})
	a.Close()
	if n, err := VerifyAudit(dir); err != nil || n != 2 {
		t.Fatalf("audit n=%d: %v", n, err)
	}
	if err := os.WriteFile(file, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAudit(dir); err == nil {
		t.Fatal("tail deletion was not detected")
	}
}
