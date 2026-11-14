package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"

	"kafedra/internal/secure"
)

func validEmail(value string) bool {
	addr, err := mail.ParseAddress(value)
	return err == nil && addr.Address == value && len(value) <= 254
}

func (a *App) wakeMailer() {
	select {
	case a.mailWake <- struct{}{}:
	default:
	}
}

// RunCertificateMailer processes the persisted queue with one sender per instance.
func (a *App) RunCertificateMailer(ctx context.Context) {
	if a.Cfg.SMTPAddr == "" || a.Cfg.SMTPFrom == "" {
		return
	}
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for ctx.Err() == nil {
		now := time.Now()
		jobs := a.Certs.Find(func(ct *Certificate) bool {
			return ct.MailTo != "" && ct.Mailed.IsZero() && !ct.MailFailed && !ct.MailNext.After(now)
		})
		for _, ct := range jobs {
			if ctx.Err() != nil {
				return
			}
			a.deliverCertificate(ctx, ct)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-a.mailWake:
		}
	}
}

func sameMailJob(x, y Certificate) bool {
	return x.MailRevision == y.MailRevision && x.MailTo == y.MailTo &&
		x.MailAttempts == y.MailAttempts && x.MailNext.Equal(y.MailNext) &&
		x.Mailed.Equal(y.Mailed) && x.MailFailed == y.MailFailed
}

func canRetryCertificateMail(ct Certificate, email string) bool {
	return validEmail(email) && (ct.MailTo == "" || !strings.EqualFold(ct.MailTo, email) || ct.MailFailed)
}

func (a *App) deliverCertificate(ctx context.Context, ct Certificate) {
	// A job may have been requeued, completed or deleted since the queue scan.
	current, ok := a.Certs.Get(ct.ID)
	if !ok || !sameMailJob(current, ct) || !current.Mailed.IsZero() || current.MailFailed {
		return
	}
	ct = current
	u, exists := a.Users.Get(ct.UserID)
	if !exists || u.Disabled || !strings.EqualFold(u.Email, ct.MailTo) || !validEmail(ct.MailTo) {
		a.finishCertificateMail(ct, errors.New("адрес получателя или учётная запись изменились"), true)
		return
	}
	if a.Users.Count(func(x *User) bool { return x.ID != u.ID && strings.EqualFold(x.Email, ct.MailTo) }) > 0 {
		a.finishCertificateMail(ct, errors.New("адрес получателя используется в нескольких учётных записях"), true)
		return
	}
	if ct.MailNext.After(time.Now()) {
		return
	}
	pdf, err := a.readCertificatePDF(&ct)
	if err != nil {
		a.finishCertificateMail(ct, err, false)
		return
	}
	text := "Здравствуйте, " + ct.StudentName + "!\r\n\r\nВы успешно прошли обучение по курсу «" + ct.CourseTitle + "».\r\n" +
		"Сертификат № " + ct.Number + " приложен к письму.\r\n\r\n" + ct.OrgName + "\r\n"
	messageID := ct.ID
	if ct.MailRevision > 0 {
		messageID = fmt.Sprintf("%s.%d", ct.ID, ct.MailRevision)
	}
	msg := buildCertMail(a.Cfg.SMTPFrom, ct.MailTo, "Сертификат о прохождении обучения", text, "Сертификат "+ct.Number+".pdf", pdf, messageID)
	err = a.sendMail(ctx, ct.MailTo, msg)
	if ctx.Err() != nil && err != nil {
		return
	}
	a.finishCertificateMail(ct, err, false)
}

func (a *App) finishCertificateMail(ct Certificate, sendErr error, permanent bool) {
	now := time.Now()
	err := a.Certs.Update(ct.ID, func(x *Certificate) error {
		if !sameMailJob(*x, ct) || !x.Mailed.IsZero() {
			return errors.New("состояние отправки изменилось")
		}
		x.MailAttempts++
		if sendErr == nil {
			x.Mailed = now
			x.MailNext = time.Time{}
		} else {
			x.MailFailed = permanent || x.MailAttempts >= 6
			x.MailNext = now.Add(time.Minute * time.Duration(1<<min(x.MailAttempts, 6)))
		}
		return nil
	})
	rec := secure.AuditRecord{Event: "отправка сертификата по почте", UserID: ct.UserID, Object: ct.Number, OK: sendErr == nil && err == nil}
	if joined := errors.Join(sendErr, err); joined != nil {
		rec.Detail = joined.Error()
		log.Printf("сертификат %s: %v", ct.Number, joined)
	}
	a.Audit.Log(rec)
}

// certificateMailRetry ставит сертификат в очередь отправки заново: после неудачи, после смены
// адреса в профиле либо для сертификата, выданного до настройки почты.
func (c *Ctx) certificateMailRetry() {
	ct, ok := c.Certs.Get(c.R.PathValue("id"))
	if !ok {
		c.Fail(http.StatusNotFound, "Сертификат не найден.")
		return
	}
	co, found := c.Courses.Get(ct.CourseID)
	if ct.UserID != c.User.ID && c.User.Role != RoleAdmin && !(found && c.canManage(&co)) {
		c.Deny()
		return
	}
	back := c.Back("/courses/" + ct.CourseID)
	if c.Cfg.SMTPAddr == "" || c.Cfg.SMTPFrom == "" {
		c.Redirect(back, "!Отправка почты не настроена. Обратитесь к администратору.")
		return
	}
	u, exists := c.Users.Get(ct.UserID)
	if !exists || u.Disabled || !validEmail(u.Email) {
		c.Redirect(back, "!В профиле учащегося нет корректного адреса электронной почты.")
		return
	}
	errAlreadyQueued := errors.New("Письмо уже ожидает отправки либо отправлено на этот адрес.")
	if err := c.Certs.Update(ct.ID, func(x *Certificate) error {
		if !canRetryCertificateMail(*x, u.Email) {
			return errAlreadyQueued
		}
		x.MailRevision++
		x.MailTo, x.MailNext, x.MailAttempts, x.MailFailed, x.Mailed = u.Email, time.Now(), 0, false, time.Time{}
		return nil
	}); err != nil {
		if errors.Is(err, errAlreadyQueued) {
			c.Redirect(back, errAlreadyQueued.Error())
			return
		}
		c.Fail(http.StatusServiceUnavailable, "Не удалось поставить письмо в очередь.")
		return
	}
	c.Log("повторная отправка сертификата", ct.Number, true, "")
	c.wakeMailer()
	c.Redirect(back, "Письмо поставлено в очередь отправки.")
}

func (a *App) readCertificatePDF(ct *Certificate) ([]byte, error) {
	if ct.PDFFileID == "" {
		return a.certificatePDF(ct)
	}
	m, ok := a.Files.Get(ct.PDFFileID)
	if !ok || !fileIDPattern.MatchString(m.ID) || m.Scope != "certificate" || m.OwnerID != ct.UserID || m.SHA256 != ct.PDFHash {
		return nil, errors.New("не найдена запись PDF")
	}
	f, err := os.Open(a.filePath(m.ID))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	if err := copyVerifiedFile(&buf, f, &m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *App) sendMail(ctx context.Context, to string, msg []byte) error {
	host, port, err := net.SplitHostPort(a.Cfg.SMTPAddr)
	if err != nil {
		return fmt.Errorf("smtp_addr: %w", err)
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", a.Cfg.SMTPAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := conn.SetDeadline(time.Now().Add(45 * time.Second)); err != nil {
		return err
	}
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	var transport net.Conn = conn
	if port == "465" {
		transport = tls.Client(conn, cfg)
	}
	cl, err := smtp.NewClient(transport, host)
	if err != nil {
		return err
	}
	defer cl.Close()
	if port != "465" {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(cfg); err != nil {
				return err
			}
		} else if ip, ok := conn.RemoteAddr().(*net.TCPAddr); !ok || !ip.IP.IsLoopback() {
			// Внутренний почтовый сервер закрытого контура может не поддерживать шифрование.
			// Отправка открытым текстом допускается только по явному разрешению и без пароля.
			if !a.Cfg.SMTPAllowPlain || a.Cfg.SMTPUser != "" {
				return errors.New("почтовый сервер не предлагает STARTTLS (для внутреннего сервера без шифрования задайте smtp_allow_plain)")
			}
		}
	}
	if a.Cfg.SMTPUser != "" {
		if err := cl.Auth(smtp.PlainAuth("", a.Cfg.SMTPUser, a.Cfg.SMTPPassword, host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(a.Cfg.SMTPFrom); err != nil {
		return err
	}
	if err := cl.Rcpt(to); err != nil {
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	// The server has accepted the message after DATA; a failed QUIT must not trigger a retry.
	return w.Close()
}
