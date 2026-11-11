// Команда kafedra — программный комплекс «Кафедра» (система управления обучением).
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // встроенная база часовых поясов: не зависит от пакетов операционной системы

	"kafedra/internal/app"
	"kafedra/internal/secure"
)

var version = "1.0.0"

const usage = `ПК «Кафедра» — система управления обучением, версия %s

Использование: kafedra <команда> [параметры]

Команды:
  init          создать конфигурацию, хранилище и учётную запись администратора
  serve         запустить сервер
  backup        создать резервную копию (служба должна быть остановлена;
                на работающей службе: systemctl kill -s USR1 kafedra)
  restore       восстановить данные из резервной копии (служба остановлена)
  passwd        выдать пользователю новый временный пароль (служба остановлена)
  audit-verify  проверить целостность журнала событий безопасности
  gencert       создать самоподписанный сертификат TLS (для пробной эксплуатации)
  version       показать версию

Общий параметр: -config путь к файлу конфигурации (по умолчанию ./config.json)
`

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		fmt.Printf(usage, version)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "serve":
		err = cmdServe(args)
	case "backup":
		err = cmdBackup(args)
	case "restore":
		err = cmdRestore(args)
	case "passwd":
		err = cmdPasswd(args)
	case "audit-verify":
		err = cmdAuditVerify(args)
	case "gencert":
		err = cmdGenCert(args)
	case "version", "-v", "--version":
		fmt.Println(version)
	default:
		fmt.Printf(usage, version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		os.Exit(1)
	}
}

func defaultConfig() app.Config {
	return app.Config{Listen: ":8443", DataDir: "./data", Timezone: "Europe/Moscow", SystemName: "Кафедра", BackupHour: 3, BackupKeep: 7}
}

func loadConfig(path string) (app.Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("не удалось прочитать конфигурацию %s: %w (создайте её командой init)", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("ошибка в файле конфигурации %s: %w", path, err)
	}
	if !filepath.IsAbs(cfg.DataDir) { // относительные пути отсчитываются от каталога конфигурации
		cfg.DataDir = filepath.Join(filepath.Dir(path), cfg.DataDir)
	}
	return cfg, nil
}

func open(args []string, fs *flag.FlagSet) (*app.App, error) {
	cfgPath := fs.String("config", "config.json", "файл конфигурации")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return nil, err
	}
	return app.New(cfg, version)
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "файл конфигурации")
	login := fs.String("admin", "admin", "логин администратора")
	name := fs.String("name", "Администратор системы", "ФИО администратора")
	dataDir := fs.String("data", "", "каталог данных (по умолчанию ./data рядом с конфигурацией)")
	listen := fs.String("listen", "", "адрес и порт (по умолчанию :8443)")
	org := fs.String("org", "", "название организации")
	demo := fs.Bool("demo", false, "добавить демонстрационные данные")
	fs.Parse(args)
	if _, err := os.Stat(*cfgPath); os.IsNotExist(err) {
		cfg := defaultConfig()
		if *dataDir != "" {
			cfg.DataDir = *dataDir
		}
		if *listen != "" {
			cfg.Listen = *listen
		}
		cfg.OrgName = *org
		b, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(*cfgPath, append(b, '\n'), 0o640); err != nil {
			return err
		}
		fmt.Println("Создан файл конфигурации:", *cfgPath)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	a, err := app.New(cfg, version)
	if err != nil {
		return err
	}
	defer a.Close()
	if a.Users.Count(nil) > 0 {
		return errors.New("система уже инициализирована (для сброса пароля используйте команду passwd)")
	}
	pw := secure.GeneratePassword(16)
	if _, err := a.CreateUser(*login, *name, "", app.RoleAdmin, "", pw); err != nil {
		return err
	}
	a.Audit.Log(secure.AuditRecord{Event: "инициализация системы", Login: *login, OK: true, Detail: "версия " + version})
	fmt.Printf("\nСистема инициализирована. Каталог данных: %s\n\n  Администратор: %s\n  Временный пароль: %s\n\n", cfg.DataDir, *login, pw)
	fmt.Println("Пароль показан один раз. При первом входе система потребует сменить его и подключить второй фактор.")
	if *demo {
		creds, err := a.SeedDemo()
		if err != nil {
			return err
		}
		fmt.Println("\nДемонстрационные учётные записи (временные пароли):")
		for _, c := range creds {
			fmt.Printf("  %-10s %s\n", c[0], c[1])
		}
	}
	fmt.Println("\nВАЖНО: сохраните копию файла", filepath.Join(cfg.DataDir, "master.key"), "в защищённом месте — без него невозможно проверить пароли из резервной копии.")
	return nil
}

func cmdServe(args []string) error {
	a, err := open(args, flag.NewFlagSet("serve", flag.ExitOnError))
	if err != nil {
		return err
	}
	defer a.Close()
	if a.Users.Count(nil) == 0 {
		return errors.New("система не инициализирована: выполните команду init")
	}
	srv := &http.Server{
		Addr:              a.Cfg.Listen,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	// события безопасности дублируются в стандартный вывод (journald → SIEM)
	a.Audit.Echo = func(line []byte) { os.Stdout.Write(append([]byte("AUDIT "), line...)) }
	stop := make(chan struct{})
	go a.Janitor(stop)
	useTLS := a.Cfg.TLSCert != "" && a.Cfg.TLSKey != ""
	a.Audit.Log(secure.AuditRecord{Event: "запуск системы", OK: true, Detail: fmt.Sprintf("версия %s, sha256 исполняемого файла %s, TLS: %v", version, a.BinSum, useTLS)})
	errc := make(chan error, 1)
	go func() {
		if useTLS {
			log.Printf("ПК «Кафедра» %s: приём соединений HTTPS на %s", version, a.Cfg.Listen)
			errc <- srv.ListenAndServeTLS(a.Cfg.TLSCert, a.Cfg.TLSKey)
		} else {
			if host, _, _ := net.SplitHostPort(a.Cfg.Listen); host != "127.0.0.1" && host != "localhost" && host != "::1" {
				log.Printf("ПРЕДУПРЕЖДЕНИЕ: TLS не настроен, а сервер доступен по сети. Укажите tls_cert и tls_key либо разместите сервер за обратным прокси с TLS и слушайте 127.0.0.1.")
			}
			log.Printf("ПК «Кафедра» %s: приём соединений HTTP на %s", version, a.Cfg.Listen)
			errc <- srv.ListenAndServe()
		}
	}()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	for {
		select {
		case err := <-errc:
			close(stop)
			return err
		case s := <-sig:
			if s == syscall.SIGUSR1 {
				name := filepath.Join(a.Cfg.DataDir, "backup", "kafedra-"+time.Now().In(a.Loc).Format("20060102-150405")+".zip")
				if err := a.BackupToFile(name); err != nil {
					log.Printf("резервное копирование: %v", err)
				} else {
					log.Printf("создана резервная копия %s", name)
				}
				continue
			}
			log.Printf("получен сигнал %v, завершение работы", s)
			close(stop)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			srv.Shutdown(ctx)
			a.Audit.Log(secure.AuditRecord{Event: "остановка системы", OK: true})
			return nil
		}
	}
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("out", "kafedra-backup-"+time.Now().Format("20060102-150405")+".zip", "файл резервной копии")
	withKey := fs.Bool("with-key", false, "включить мастер-ключ (копию хранить в защищённом месте)")
	a, err := open(args, fs)
	if err != nil {
		return err
	}
	defer a.Close()
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := a.Backup(f, *withKey); err != nil {
		f.Close()
		return err
	}
	a.Audit.Log(secure.AuditRecord{Event: "резервное копирование", Object: *out, OK: true, Detail: fmt.Sprintf("командная строка, с ключом: %v", *withKey)})
	fmt.Println("Резервная копия создана:", *out)
	return f.Close()
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "файл конфигурации")
	in := fs.String("in", "", "файл резервной копии")
	fs.Parse(args)
	if *in == "" {
		return errors.New("укажите файл: -in копия.zip")
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	// открытие хранилища гарантирует, что служба остановлена (исключительная блокировка)
	a, err := app.New(cfg, version)
	if err != nil {
		return err
	}
	a.Audit.Close()
	a.DB.Close()
	if err := app.Restore(*in, cfg.DataDir); err != nil {
		return err
	}
	fmt.Println("Данные восстановлены в", cfg.DataDir)
	if n, err := secure.VerifyAudit(filepath.Join(cfg.DataDir, "audit")); err != nil {
		fmt.Println("ВНИМАНИЕ:", err)
	} else {
		fmt.Println("Целостность журнала событий подтверждена, записей:", n)
	}
	return nil
}

func cmdPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	login := fs.String("login", "admin", "логин пользователя")
	a, err := open(args, fs)
	if err != nil {
		return err
	}
	defer a.Close()
	u, ok := a.Users.First(func(u *app.User) bool { return u.Login == *login })
	if !ok {
		return fmt.Errorf("пользователь %q не найден", *login)
	}
	pw := secure.GeneratePassword(16)
	hash := a.Keys.HashPassword(pw)
	a.Users.Update(u.ID, func(x *app.User) error {
		x.PassHash, x.PassChanged, x.MustChange, x.Failed, x.LockedUntil, x.Disabled = hash, time.Now(), true, 0, time.Time{}, false
		x.TOTPEnabled, x.TOTPSecret = false, ""
		return nil
	})
	for _, s := range a.Sessions.Find(func(s *app.Session) bool { return s.UserID == u.ID }) {
		a.Sessions.Delete(s.ID)
	}
	a.Audit.Log(secure.AuditRecord{Event: "сброс пароля из командной строки", Login: *login, OK: true, Detail: "сброшены блокировка и второй фактор"})
	fmt.Printf("Пользователь: %s\nВременный пароль: %s\nВторой фактор сброшен, блокировка снята.\n", *login, pw)
	return nil
}

func cmdAuditVerify(args []string) error {
	fs := flag.NewFlagSet("audit-verify", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "файл конфигурации")
	fs.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	n, err := secure.VerifyAudit(filepath.Join(cfg.DataDir, "audit"))
	if err != nil {
		return err
	}
	fmt.Println("Целостность журнала событий подтверждена, записей:", n)
	return nil
}

func cmdGenCert(args []string) error {
	fs := flag.NewFlagSet("gencert", flag.ExitOnError)
	host := fs.String("host", "localhost", "имя узла или IP-адрес сервера")
	dir := fs.String("out", ".", "каталог для файлов cert.pem и key.pem")
	fs.Parse(args)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: *host, Organization: []string{"Kafedra LMS"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	if ip := net.ParseIP(*host); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	} else {
		tpl.DNSNames = []string{*host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	fmt.Printf("Созданы %s и %s (самоподписанный сертификат на 1 год для %s).\nДля промышленной эксплуатации используйте сертификат удостоверяющего центра.\n",
		filepath.Join(*dir, "cert.pem"), filepath.Join(*dir, "key.pem"), *host)
	return nil
}
