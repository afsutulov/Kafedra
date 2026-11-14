package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// Сведения о программном продукте. Не настраиваются: отображаются в нижней строке интерфейса.
const (
	ProductName = "ПК «Кафедра»"
	Vendor      = "ГБУ ПК «Центр информационного развития ПК»"
)

// Config — файл конфигурации config.json: все основные настройки экземпляра.
type Config struct {
	Listen                      string `json:"listen"`        // адрес и порт
	DataDir                     string `json:"data_dir"`      // каталог данных
	LogFile                     string `json:"log_file"`      // файл журнала работы службы ("-" — только стандартный вывод)
	AuditDir                    string `json:"audit_dir"`     // каталог журнала событий безопасности (пусто — <data_dir>/audit)
	TLSCert                     string `json:"tls_cert"`      // сертификат TLS (пусто — работа по HTTP за внешним веб-сервером)
	TLSKey                      string `json:"tls_key"`       // ключ TLS
	ExternalURL                 string `json:"external_url"`  // адрес, по которому портал открывают пользователи, например https://lms.example.ru
	TrustedProxy                string `json:"trusted_proxy"` // IP внешнего веб-сервера (прокси); несколько — через запятую
	Timezone                    string `json:"timezone"`      // часовой пояс
	PortalName                  string `json:"portal_name"`   // первая строка заголовка: название портала
	OrgName                     string `json:"org_name"`      // вторая строка заголовка: название организации
	LogoFile                    string `json:"logo_file"`     // файл логотипа (PNG, SVG, JPEG, WebP); пусто — встроенный
	PersonalDataOperatorAddress string `json:"personal_data_operator_address,omitempty"`
	CertificateIcon             string `json:"certificate_icon"` // значок на сертификате (PNG или JPEG); пусто — логотип портала либо встроенный
	CertificateFont             string `json:"certificate_font"` // шрифт TrueType для сертификатов; пусто — поиск в системе
	SMTPAddr                    string `json:"smtp_addr"`        // почтовый сервер для отправки сертификатов, "узел:порт"; пусто — письма не отправляются
	SMTPFrom                    string `json:"smtp_from"`        // адрес отправителя
	SMTPUser                    string `json:"smtp_user"`        // учётная запись на почтовом сервере (если требуется)
	SMTPPassword                string `json:"smtp_password"`    // пароль учётной записи почтового сервера
	SMTPAllowPlain              bool   `json:"smtp_allow_plain"` // разрешить отправку без шифрования на сервер, не предлагающий STARTTLS (только без smtp_user)
	BackupHour                  int    `json:"backup_hour"`      // час ежесуточного резервного копирования (-1 — отключено)
	BackupKeep                  int    `json:"backup_keep"`      // сколько последних копий хранить

	Settings Settings `json:"settings"` // безопасность, учебный процесс, файлы (изменяются и из веб-интерфейса)

	// LegacySystemName — параметр версии 1.0; принимается, чтобы старый файл конфигурации загружался, и не используется.
	LegacySystemName string `json:"system_name,omitempty"`

	path string // откуда загружен файл
	base string // каталог файла конфигурации: от него отсчитываются относительные пути
}

// Расположение по умолчанию: всё, что нужно программе, лежит в одном каталоге /opt/kafedra;
// отдельно — только журнал работы.
const (
	DefaultHome       = "/opt/kafedra"
	DefaultConfigPath = DefaultHome + "/config.json"
	DefaultLogFile    = "/var/log/kafedra/kafedra.log"
)

// DefaultConfig возвращает конфигурацию по умолчанию.
func DefaultConfig() Config {
	return Config{Listen: ":8443", DataDir: DefaultHome + "/data", LogFile: DefaultLogFile, Timezone: "Europe/Moscow", PortalName: "Учебный портал",
		BackupHour: 3, BackupKeep: 7, Settings: DefaultSettings()}
}

// LoadConfig читает файл конфигурации; отсутствующие параметры получают значения по умолчанию.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("не удалось прочитать конфигурацию %s: %w (создайте её командой init)", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("ошибка в файле конфигурации %s: %w", path, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("%s: после конфигурации обнаружены лишние данные", path)
	}
	cfg.LegacySystemName = ""
	// Параметры, меняющие порядок работы учащихся, включены только в новых установках.
	// В файле, созданном прежней версией, их нет — при обновлении они остаются выключенными,
	// пока администратор не включит их в «Настройках».
	var raw struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	json.Unmarshal(b, &raw)
	if _, ok := raw.Settings["questionnaire_required"]; !ok {
		cfg.Settings.QuestRequired = false
	}
	if _, ok := raw.Settings["certificate_requires_application"]; !ok {
		cfg.Settings.CertNeedsApp = false
	}
	cfg.path = path
	cfg.base = filepath.Dir(path)
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return cfg, fmt.Errorf("%s: tls_cert и tls_key задаются вместе; для работы без сертификатов оставьте оба пустыми", path)
	}
	if cfg.ExternalURL != "" {
		u, err := url.Parse(cfg.ExternalURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return cfg, fmt.Errorf("%s: external_url должен иметь вид https://имя-узла", path)
		}
	}
	if cfg.SMTPAllowPlain && cfg.SMTPUser != "" {
		return cfg, fmt.Errorf("%s: smtp_allow_plain нельзя сочетать с smtp_user — пароль без шифрования не передаётся", path)
	}
	if cfg.SMTPAddr != "" || cfg.SMTPFrom != "" {
		host, port, err := net.SplitHostPort(cfg.SMTPAddr)
		number, portErr := strconv.Atoi(port)
		if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 || !validEmail(cfg.SMTPFrom) {
			return cfg, fmt.Errorf("%s: укажите корректные smtp_addr (узел:порт) и smtp_from (адрес отправителя)", path)
		}
	}
	return cfg, nil
}

// Save записывает конфигурацию в файл в удобочитаемом виде (с отступами).
func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = bytes.ReplaceAll(b, []byte(`&`), []byte("&")) // без экранирования, мешающего чтению
	b = bytes.ReplaceAll(b, []byte(`<`), []byte("<"))
	b = bytes.ReplaceAll(b, []byte(`>`), []byte(">"))
	mode := os.FileMode(0o640)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if c.SMTPPassword != "" {
		mode = 0o600
		if err := os.Chmod(path, mode); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.WriteFile(path, append(b, '\n'), mode)
}

// abs преобразует путь из конфигурации в абсолютный (относительные — от каталога конфигурации).
func (c *Config) abs(p string) string {
	if p == "" || filepath.IsAbs(p) || c.base == "" {
		return p
	}
	return filepath.Join(c.base, p)
}

// UseTLS сообщает, принимает ли программа соединения HTTPS сама.
func (c *Config) UseTLS() bool { return c.TLSCert != "" && c.TLSKey != "" }

// CertPath и KeyPath возвращают абсолютные пути сертификата и ключа.
func (c *Config) CertPath() string { return c.abs(c.TLSCert) }
func (c *Config) KeyPath() string  { return c.abs(c.TLSKey) }

// externalHost возвращает узел и признак HTTPS из external_url.
func (c *Config) externalHost() (host string, https bool) {
	if u, err := url.Parse(c.ExternalURL); err == nil && c.ExternalURL != "" {
		return u.Host, u.Scheme == "https"
	}
	return "", false
}

// BehindProxy сообщает, что TLS завершается на внешнем веб-сервере.
func (c *Config) BehindProxy() bool {
	return !c.UseTLS() && (c.ExternalURL != "" || c.TrustedProxy != "")
}

// DataPath возвращает абсолютный путь каталога данных.
func (c *Config) DataPath() string { return c.abs(c.DataDir) }

// LogPath возвращает абсолютный путь файла журнала работы ("" — не задан).
func (c *Config) LogPath() string {
	if c.LogFile == "-" {
		return ""
	}
	return c.abs(c.LogFile)
}

// AuditPath возвращает каталог журнала событий безопасности.
func (c *Config) AuditPath() string {
	if c.AuditDir != "" {
		return c.abs(c.AuditDir)
	}
	return filepath.Join(c.DataPath(), "audit")
}
