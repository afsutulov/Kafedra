// Пакет secure — криптографические и защитные механизмы ПК «Кафедра»:
// хранение паролей, ключи, токены, одноразовые коды (TOTP), парольная политика.
// Используется только стандартная библиотека Go.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Keys — набор ключей, выведенных из мастер-ключа экземпляра.
// Мастер-ключ хранится в отдельном файле с правами 0600 вне хранилища данных:
// копии базы данных без этого файла недостаточно для подбора паролей.
type Keys struct {
	pepper []byte // «перец» для хешей паролей
	seal   []byte // шифрование секретов TOTP
}

// LoadOrCreateKeys читает мастер-ключ из файла или создаёт новый.
func LoadOrCreateKeys(path string) (*Keys, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b = make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(b)+"\n"), 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		if st, e := os.Stat(path); e == nil && st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("файл ключа %s доступен посторонним (права %o): установите 0600", path, st.Mode().Perm())
		}
		b, err = hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(b) != 32 {
			return nil, errors.New("файл мастер-ключа повреждён")
		}
	}
	return NewKeys(b)
}

// NewKeys выводит рабочие ключи из мастер-ключа (HKDF-SHA256).
func NewKeys(master []byte) (*Keys, error) {
	k := &Keys{}
	var err error
	if k.pepper, err = hkdf.Key(sha256.New, master, nil, "kafedra/password-pepper/v1", 32); err != nil {
		return nil, err
	}
	if k.seal, err = hkdf.Key(sha256.New, master, nil, "kafedra/seal/v1", 32); err != nil {
		return nil, err
	}
	return k, nil
}

// Iterations — число итераций PBKDF2-HMAC-SHA256 для новых хешей.
var Iterations = 600000

// HashPassword возвращает строку вида pbkdf2-sha256$итерации$соль$хеш.
func (k *Keys) HashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	return k.hashWith(password, salt, Iterations)
}

func (k *Keys) hashWith(password string, salt []byte, iter int) string {
	mac := hmac.New(sha256.New, k.pepper)
	mac.Write([]byte(password))
	dk, _ := pbkdf2.Key(sha256.New, hex.EncodeToString(mac.Sum(nil)), salt, iter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk))
}

// VerifyPassword проверяет пароль; rehash=true, если хеш создан с устаревшими параметрами.
func (k *Keys) VerifyPassword(password, encoded string) (ok, rehash bool) {
	p := strings.Split(encoded, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		k.hashWith(password, make([]byte, 16), Iterations) // выравнивание времени ответа
		return false, false
	}
	iter, err := strconv.Atoi(p[1])
	salt, err2 := base64.RawStdEncoding.DecodeString(p[2])
	if err != nil || err2 != nil || iter < 1 || iter > 10_000_000 {
		return false, false
	}
	ok = subtle.ConstantTimeCompare([]byte(k.hashWith(password, salt, iter)), []byte(encoded)) == 1
	return ok, ok && iter < Iterations
}

// Token возвращает криптографически стойкий случайный токен (256 бит).
func Token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewID возвращает случайный идентификатор документа (96 бит).
func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// HashToken возвращает SHA-256 токена: в хранилище попадает только хеш.
func HashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

// Equal — сравнение строк за постоянное время.
func Equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// Seal шифрует данные (AES-256-GCM).
func (k *Keys) Seal(plain []byte) string {
	blk, _ := aes.NewCipher(k.seal)
	g, _ := cipher.NewGCM(blk)
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return base64.RawStdEncoding.EncodeToString(g.Seal(nonce, nonce, plain, nil))
}

// Open расшифровывает данные, зашифрованные Seal.
func (k *Keys) Open(sealed string) ([]byte, error) {
	b, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(k.seal)
	g, _ := cipher.NewGCM(blk)
	if len(b) < g.NonceSize() {
		return nil, errors.New("некорректные данные")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
}

// ---- TOTP (RFC 6238, HMAC-SHA1, 6 цифр, шаг 30 с) ----

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret создаёт секрет второго фактора в кодировке Base32.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

func totpAt(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	h := mac.Sum(nil)
	off := h[len(h)-1] & 0x0f
	v := binary.BigEndian.Uint32(h[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000), nil
}

// TOTPCode возвращает код для момента времени t.
func TOTPCode(secret string, t time.Time) string {
	c, _ := totpAt(secret, t.Unix()/30)
	return c
}

// VerifyTOTP проверяет код с допуском ±1 шаг. lastStep — последний использованный шаг
// (защита от повторного использования кода); возвращается новый шаг.
func VerifyTOTP(secret, code string, t time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	cur := t.Unix() / 30
	for _, s := range []int64{cur, cur - 1, cur + 1} {
		if s <= lastStep {
			continue
		}
		if c, err := totpAt(secret, s); err == nil && Equal(c, code) {
			return s, true
		}
	}
	return 0, false
}

// ---- Парольная политика ----

// Policy — требования к паролям.
type Policy struct {
	MinLength  int
	NeedUpper  bool
	NeedLower  bool
	NeedDigit  bool
	NeedSymbol bool
}

var commonPasswords = map[string]bool{
	"password": true, "qwerty123456": true, "123456789012": true, "qwertyuiop": true, "1q2w3e4r5t6y": true,
	"пароль": true, "йцукенгшщз": true, "password1234": true, "admin1234567": true, "administrator": true,
}

// CheckPassword возвращает список нарушений парольной политики.
func CheckPassword(pw, login string, p Policy) []string {
	var errs []string
	if n := len([]rune(pw)); n < p.MinLength {
		errs = append(errs, fmt.Sprintf("длина пароля должна быть не менее %d символов", p.MinLength))
	}
	if len(pw) > 256 {
		errs = append(errs, "пароль слишком длинный")
	}
	var up, lo, dg, sy bool
	for _, r := range pw {
		switch {
		case unicode.IsUpper(r):
			up = true
		case unicode.IsLower(r):
			lo = true
		case unicode.IsDigit(r):
			dg = true
		default:
			sy = true
		}
	}
	if p.NeedUpper && !up {
		errs = append(errs, "нужна хотя бы одна заглавная буква")
	}
	if p.NeedLower && !lo {
		errs = append(errs, "нужна хотя бы одна строчная буква")
	}
	if p.NeedDigit && !dg {
		errs = append(errs, "нужна хотя бы одна цифра")
	}
	if p.NeedSymbol && !sy {
		errs = append(errs, "нужен хотя бы один специальный символ")
	}
	l := strings.ToLower(pw)
	if login != "" && strings.Contains(l, strings.ToLower(login)) {
		errs = append(errs, "пароль не должен содержать логин")
	}
	if commonPasswords[l] {
		errs = append(errs, "пароль входит в список распространённых")
	}
	return errs
}

// GeneratePassword создаёт случайный пароль, удовлетворяющий типовой политике.
func GeneratePassword(n int) string {
	const (
		lo = "abcdefghijkmnpqrstuvwxyz"
		up = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		dg = "23456789"
		sy = "!@#$%*-_=+"
	)
	if n < 12 {
		n = 12
	}
	all := lo + up + dg + sy
	pick := func(set string) byte {
		var b [2]byte
		for {
			rand.Read(b[:])
			v := int(binary.BigEndian.Uint16(b[:]))
			if lim := 65536 - 65536%len(set); v < lim {
				return set[v%len(set)]
			}
		}
	}
	out := []byte{pick(lo), pick(up), pick(dg), pick(sy)}
	for len(out) < n {
		out = append(out, pick(all))
	}
	for i := len(out) - 1; i > 0; i-- { // перемешивание Фишера — Йетса
		var b [2]byte
		rand.Read(b[:])
		j := int(binary.BigEndian.Uint16(b[:])) % (i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
