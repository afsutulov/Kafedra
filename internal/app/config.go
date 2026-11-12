package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Сведения о программном продукте. Не настраиваются: отображаются в нижней строке интерфейса.
const (
	ProductName = "ПК «Кафедра»"
	Vendor      = "ООО «Рога и копыта»"
)

// Config — файл конфигурации config.json: все основные настройки экземпляра.
type Config struct {
	Listen       string `json:"listen"`        // адрес и порт
	DataDir      string `json:"data_dir"`      // каталог данных
	LogFile      string `json:"log_file"`      // файл журнала работы службы ("-" — только стандартный вывод)
	AuditDir     string `json:"audit_dir"`     // каталог журнала событий безопасности (пусто — <data_dir>/audit)
	TLSCert      string `json:"tls_cert"`      // сертификат TLS
	TLSKey       string `json:"tls_key"`       // ключ TLS
	TrustedProxy string `json:"trusted_proxy"` // IP обратного прокси
	Timezone     string `json:"timezone"`      // часовой пояс
	PortalName   string `json:"portal_name"`   // первая строка заголовка: название портала
	OrgName      string `json:"org_name"`      // вторая строка заголовка: название организации
	LogoFile     string `json:"logo_file"`     // файл логотипа (PNG, SVG, JPEG, WebP); пусто — встроенный
	BackupHour   int    `json:"backup_hour"`   // час ежесуточного резервного копирования (-1 — отключено)
	BackupKeep   int    `json:"backup_keep"`   // сколько последних копий хранить

	Settings Settings `json:"settings"` // безопасность, учебный процесс, файлы (изменяются и из веб-интерфейса)

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
	cfg.path = path
	cfg.base = filepath.Dir(path)
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
	return os.WriteFile(path, append(b, '\n'), mode)
}

// abs преобразует путь из конфигурации в абсолютный (относительные — от каталога конфигурации).
func (c Config) abs(p string) string {
	if p == "" || filepath.IsAbs(p) || c.base == "" {
		return p
	}
	return filepath.Join(c.base, p)
}

// DataPath возвращает абсолютный путь каталога данных.
func (c Config) DataPath() string { return c.abs(c.DataDir) }

// LogPath возвращает абсолютный путь файла журнала работы ("" — не задан).
func (c Config) LogPath() string {
	if c.LogFile == "-" {
		return ""
	}
	return c.abs(c.LogFile)
}

// AuditPath возвращает каталог журнала событий безопасности.
func (c Config) AuditPath() string {
	if c.AuditDir != "" {
		return c.abs(c.AuditDir)
	}
	return filepath.Join(c.DataPath(), "audit")
}
