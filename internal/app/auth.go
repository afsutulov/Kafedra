package app

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kafedra/internal/secure"
)

const cookieName = "kafedra_sid"

func (c *Ctx) loadSession() {
	ck, err := c.R.Cookie(cookieName)
	if err != nil || ck.Value == "" {
		return
	}
	sid := secure.HashToken(ck.Value)
	s, ok := c.Sessions.Get(sid)
	if !ok {
		return
	}
	set := c.S()
	now := time.Now()
	if now.Sub(s.Seen) > time.Duration(set.IdleMinutes)*time.Minute || now.Sub(s.Created) > time.Duration(set.SessionHours)*time.Hour {
		c.Sessions.Delete(sid)
		return
	}
	u, ok := c.Users.Get(s.UserID)
	if !ok || u.Disabled {
		c.Sessions.Delete(sid)
		return
	}
	if now.Sub(s.Seen) > time.Minute {
		c.Sessions.Update(sid, func(x *Session) error { x.Seen = now; return nil })
		s.Seen = now
	}
	c.User, c.Sess = &u, &s
}

func (c *Ctx) mfaRequired() bool {
	switch c.S().MFARoles {
	case "admin":
		return c.User.Role == RoleAdmin
	case "staff":
		return c.User.Role != RoleStudent
	case "all":
		return true
	}
	return false
}

func (c *Ctx) passwordExpired() bool {
	d := c.S().PwMaxAgeDays
	return d > 0 && time.Since(c.User.PassChanged) > time.Duration(d)*24*time.Hour
}

// Ограничение подбора паролей с одного адреса. Считаются только НЕУДАЧНЫЕ попытки: за общим
// веб-сервером или NAT учебного класса с одного адреса входят десятки людей, и счёт всех
// попыток (как было до 1.4.4) не пускал в систему 21-го пользователя. Основная защита учётной
// записи — её блокировка после нескольких неверных паролей.
func (a *App) ipFailLimit() int {
	if n := a.S().IPFailLimit; n > 0 {
		return n
	}
	return 100
}

// tooManyFailures сообщает, что с адреса за 10 минут пришло слишком много неудачных попыток входа.
func (a *App) tooManyFailures(ip string) bool {
	limit := a.ipFailLimit()
	a.limMu.Lock()
	defer a.limMu.Unlock()
	now, n := time.Now(), 0
	for _, t := range a.lim["login:"+ip] {
		if now.Sub(t) < 10*time.Minute {
			n++
		}
	}
	return n >= limit
}

// noteFailure учитывает неудачную попытку входа с адреса.
func (a *App) noteFailure(ip string) {
	a.limMu.Lock()
	defer a.limMu.Unlock()
	now, key := time.Now(), "login:"+ip
	keep := a.lim[key][:0]
	for _, t := range a.lim[key] {
		if now.Sub(t) < 10*time.Minute {
			keep = append(keep, t)
		}
	}
	a.lim[key] = append(keep, now)
	if len(a.lim) > 100000 {
		a.lim = map[string][]time.Time{}
	}
}

// allow — ограничение частоты попыток по ключу (20 за 10 минут); используется для кодов второго фактора.
func (a *App) allow(ip string) bool {
	a.limMu.Lock()
	defer a.limMu.Unlock()
	now := time.Now()
	keep := a.lim[ip][:0]
	for _, t := range a.lim[ip] {
		if now.Sub(t) < 10*time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 20 {
		a.lim[ip] = keep
		return false
	}
	a.lim[ip] = append(keep, now)
	if len(a.lim) > 100000 {
		a.lim = map[string][]time.Time{}
	}
	return true
}

func (c *Ctx) loginPage() {
	if c.User != nil {
		http.Redirect(c.W, c.R, "/", http.StatusSeeOther)
		return
	}
	c.Render("login", map[string]any{"Banner": c.S().Banner})
}

func (c *Ctx) loginPost() {
	login := strings.ToLower(c.F("login"))
	auditLogin := login
	if len(auditLogin) > 40 {
		auditLogin = auditLogin[:40]
	}
	fail := func(reason string) {
		c.noteFailure(c.IP)
		c.Audit.Log(secure.AuditRecord{Event: "вход в систему", Login: auditLogin, IP: c.IP, OK: false, Detail: reason})
		c.W.WriteHeader(http.StatusUnauthorized)
		// одинаковое сообщение для всех причин: не раскрываем существование учётной записи
		c.Render("login", map[string]any{"Banner": c.S().Banner, "Error": "Неверный логин или пароль либо учётная запись заблокирована."})
	}
	if c.tooManyFailures(c.IP) {
		c.Audit.Log(secure.AuditRecord{Event: "вход в систему", Login: auditLogin, IP: c.IP, OK: false, Detail: "превышено число неудачных попыток с адреса"})
		c.W.WriteHeader(http.StatusTooManyRequests)
		c.Render("login", map[string]any{"Banner": c.S().Banner, "Error": "Слишком много неудачных попыток входа с вашего адреса. Повторите через несколько минут."})
		return
	}
	if !reLogin.MatchString(login) {
		fail("некорректный формат логина")
		return
	}
	u, found := c.Users.First(func(u *User) bool { return u.Login == login })
	ok, rehash := c.Keys.VerifyPassword(c.R.FormValue("password"), u.PassHash) // выполняется и для несуществующих логинов
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if found {
		current, exists := c.Users.Get(u.ID)
		if !exists || current.PassHash != u.PassHash || current.Login != u.Login {
			fail("учётная запись изменилась, повторите вход")
			return
		}
		u = current
	}
	set := c.S()
	now := time.Now()
	switch {
	case !found:
		fail("неизвестный логин")
		return
	case u.Disabled:
		fail("учётная запись отключена")
		return
	case now.Before(u.LockedUntil):
		fail("учётная запись временно заблокирована")
		return
	case !ok:
		var locked bool
		if err := c.Users.Update(u.ID, func(x *User) error {
			x.Failed++
			if set.LockAttempts > 0 && x.Failed >= set.LockAttempts {
				x.Failed, x.LockedUntil, locked = 0, now.Add(time.Duration(set.LockMinutes)*time.Minute), true
			}
			return nil
		}); err != nil {
			fail("не удалось сохранить счётчик ошибок входа")
			return
		}
		if locked {
			c.Audit.Log(secure.AuditRecord{Event: "блокировка учётной записи", Login: login, UserID: u.ID, IP: c.IP, OK: false, Detail: "превышено число неудачных попыток входа"})
		}
		fail("неверный пароль")
		return
	}
	if err := c.Users.Update(u.ID, func(x *User) error {
		x.Failed, x.LastLogin, x.LastIP = 0, now, c.IP
		if rehash {
			x.PassHash = c.Keys.HashPassword(c.R.FormValue("password"))
		}
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить сведения о входе.")
		return
	}
	// ограничение числа одновременных сеансов: вытесняется самый старый
	mine := sortBy(c.Sessions.Find(func(s *Session) bool { return s.UserID == u.ID }), func(a, b *Session) bool { return a.Created.Before(b.Created) })
	for len(mine) >= set.MaxSessions && set.MaxSessions > 0 {
		if err := c.Sessions.Delete(mine[0].ID); err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось завершить прежний сеанс.")
			return
		}
		mine = mine[1:]
	}
	token := secure.Token() // новый идентификатор сеанса при каждом входе (защита от фиксации сеанса)
	ua := c.R.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	if err := c.Sessions.Put(secure.HashToken(token), Session{ID: secure.HashToken(token), UserID: u.ID, CSRF: secure.Token(),
		Created: now, Seen: now, IP: c.IP, UA: ua}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось создать сеанс.")
		return
	}
	http.SetCookie(c.W, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: c.isHTTPS(c.R), SameSite: http.SameSiteStrictMode})
	c.Audit.Log(secure.AuditRecord{Event: "вход в систему", Login: login, UserID: u.ID, IP: c.IP, OK: true})
	http.Redirect(c.W, c.R, "/", http.StatusSeeOther)
}

func (c *Ctx) logout() {
	if err := c.Sessions.Delete(c.Sess.ID); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось завершить сеанс.")
		return
	}
	http.SetCookie(c.W, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true})
	c.Log("выход из системы", "", true, "")
	http.Redirect(c.W, c.R, "/login", http.StatusSeeOther)
}

func (c *Ctx) needsMFACode() bool {
	return c.User.TOTPEnabled && (c.Sess.MFAVerified == "" || c.Sess.MFAVerified != secure.HashToken(c.User.TOTPSecret))
}

func (c *Ctx) mfaPage() {
	if !c.needsMFACode() {
		http.Redirect(c.W, c.R, "/", http.StatusSeeOther)
		return
	}
	c.Render("mfa", nil)
}

func (c *Ctx) mfaPost() {
	if !c.needsMFACode() {
		http.Redirect(c.W, c.R, "/", http.StatusSeeOther)
		return
	}
	if !c.allow("mfa:" + c.User.ID) {
		c.Sessions.Delete(c.Sess.ID)
		c.Log("второй фактор", "", false, "превышено число попыток, сеанс завершён")
		http.Redirect(c.W, c.R, "/login", http.StatusSeeOther)
		return
	}
	secret, err := c.Keys.Open(c.User.TOTPSecret)
	step, ok := int64(0), false
	if err == nil {
		step, ok = secure.VerifyTOTP(string(secret), c.F("code"), time.Now(), c.User.TOTPStep, c.S().TOTPSkew)
	}
	if !ok {
		c.Log("второй фактор", "", false, "неверный код")
		c.W.WriteHeader(http.StatusUnauthorized)
		c.Render("mfa", map[string]any{"Error": "Неверный код. Проверьте время на устройстве и повторите."})
		return
	}
	if err := c.Users.Update(c.User.ID, func(x *User) error {
		if !x.TOTPEnabled || x.TOTPSecret != c.User.TOTPSecret || step <= x.TOTPStep {
			return errors.New("код уже использован либо второй фактор изменён")
		}
		x.TOTPStep = step
		return nil
	}); err != nil {
		c.Fail(http.StatusConflict, "Не удалось подтвердить одноразовый код. Введите новый код.")
		return
	}
	if err := c.Sessions.Update(c.Sess.ID, func(s *Session) error { s.MFAVerified = secure.HashToken(c.User.TOTPSecret); return nil }); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить состояние сеанса.")
		return
	}
	c.Log("второй фактор", "", true, "")
	http.Redirect(c.W, c.R, "/", http.StatusSeeOther)
}

func (c *Ctx) policy() secure.Policy {
	s := c.S()
	return secure.Policy{MinLength: s.PwMinLen, NeedUpper: s.PwUpper, NeedLower: s.PwLower, NeedDigit: s.PwDigit, NeedSymbol: s.PwSymbol}
}

// preparePassword проверяет новый пароль по политике и истории и вычисляет его хеш.
// В хранилище ничего не пишет: это самая долгая часть смены пароля (до восьми вычислений
// PBKDF2), и выполнять её нужно ВНЕ общей блокировки записи.
func (a *App) preparePassword(u User, pw string, pol secure.Policy, checkHistory, skipCurrent bool) (string, error) {
	if errs := secure.CheckPassword(pw, u.Login, pol); len(errs) > 0 {
		return "", errors.New(strings.Join(errs, "; "))
	}
	if checkHistory { // при сбросе администратором история не проверяется
		old := u.PassHistory
		if !skipCurrent { // вызывающий уже убедился, что новый пароль отличается от действующего
			old = append([]string{u.PassHash}, old...)
		}
		for _, h := range old {
			if ok, _ := a.Keys.VerifyPassword(pw, h); ok {
				return "", errors.New("пароль совпадает с одним из предыдущих")
			}
		}
	}
	return a.Keys.HashPassword(pw), nil
}

var errPasswordChanged = errors.New("учётная запись изменилась, повторите действие")

// storePassword записывает заранее вычисленный хеш, если пароль с момента проверки не менялся.
func (a *App) storePassword(userID, expectHash, hash string, history int, mustChange bool) error {
	return a.Users.Update(userID, func(x *User) error {
		if x.PassHash != expectHash {
			return errPasswordChanged
		}
		if x.PassHash != "" {
			x.PassHistory = append([]string{x.PassHash}, x.PassHistory...)
		}
		if len(x.PassHistory) > history {
			x.PassHistory = x.PassHistory[:max(history, 0)]
		}
		x.PassHash, x.PassChanged, x.MustChange, x.Failed, x.LockedUntil = hash, time.Now(), mustChange, 0, time.Time{}
		return nil
	})
}

// setPassword проверяет новый пароль по политике и истории и сохраняет его.
func (a *App) setPassword(userID, pw string, pol secure.Policy, history int, mustChange bool) error {
	u, ok := a.Users.Get(userID)
	if !ok {
		return errors.New("пользователь не найден")
	}
	hash, err := a.preparePassword(u, pw, pol, !mustChange, false)
	if err != nil {
		return err
	}
	return a.storePassword(userID, u.PassHash, hash, history, mustChange)
}

func (c *Ctx) passwordPage() {
	c.Render("password", map[string]any{"S": c.S(), "Forced": c.User.MustChange || c.passwordExpired(), "NeedConsent": c.User.Consent.IsZero(),
		"TeacherMails": c.teacherMails()})
}

// teacherMails возвращает преподавателей действующих курсов учащегося.
func (c *Ctx) teacherMails() []User {
	if c.User.Role != RoleStudent {
		return nil
	}
	seen := map[string]bool{}
	var out []User
	for _, co := range c.coursesOf(c.User) {
		for _, id := range co.TeacherIDs {
			if u, ok := c.Users.Get(id); ok && u.Role == RoleTeacher && !u.Disabled && !seen[id] {
				seen[id] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// passwordPost — смена пароля и согласие на обработку персональных данных. Обработчик сам берёт
// блокировку записи (см. selfLocking): проверка текущего пароля, сверка с историей и вычисление
// нового хеша выполняются до неё. Иначе одновременная смена временных паролей учебной группой
// на десятки секунд останавливала сохранение ответов и сдачу тестов у всех пользователей.
func (c *Ctx) passwordPost() {
	needConsent := c.User.Consent.IsZero()
	forced := c.User.MustChange || c.passwordExpired()
	bad := func(msg string) {
		c.W.WriteHeader(http.StatusBadRequest)
		c.Render("password", map[string]any{"S": c.S(), "Forced": forced, "NeedConsent": needConsent, "Error": msg, "TeacherMails": c.teacherMails()})
	}
	if needConsent && c.F("consent") != "1" {
		bad("Для работы в системе необходимо согласие на обработку персональных данных.")
		return
	}
	changing := forced || c.R.FormValue("new") != ""
	userID, oldHash, newHash := c.User.ID, c.User.PassHash, ""
	if changing {
		if ok, _ := c.Keys.VerifyPassword(c.R.FormValue("old"), oldHash); !ok {
			c.Log("смена пароля", "", false, "неверный текущий пароль")
			bad("Текущий пароль указан неверно.")
			return
		}
		if c.R.FormValue("new") != c.R.FormValue("new2") {
			bad("Новый пароль и его подтверждение не совпадают.")
			return
		}
		// текущий пароль только что подтверждён: совпадение с ним видно без вычисления хеша
		if c.R.FormValue("new") == c.R.FormValue("old") {
			bad("Пароль не принят: пароль совпадает с одним из предыдущих.")
			return
		}
		var err error
		if newHash, err = c.preparePassword(*c.User, c.R.FormValue("new"), c.policy(), true, true); err != nil {
			bad("Пароль не принят: " + err.Error() + ".")
			return
		}
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// сеанс и учётная запись могли измениться, пока вычислялся хеш
	c.User, c.Sess = nil, nil
	c.loadSession()
	if c.User == nil || c.User.ID != userID || c.User.PassHash != oldHash || !secure.Equal(c.F("csrf"), c.Sess.CSRF) {
		c.Fail(http.StatusConflict, "Учётная запись или сеанс изменились. Повторите действие.")
		return
	}
	if changing {
		if err := c.revokeSessions(c.User.ID, c.Sess.ID); err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось завершить другие сеансы.")
			return
		}
		if err := c.storePassword(userID, oldHash, newHash, c.S().PwHistory, false); err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить пароль.")
			return
		}
		c.Log("смена пароля", "", true, "")
	}
	if needConsent {
		if err := c.Users.Update(c.User.ID, func(x *User) error { x.Consent = time.Now(); return nil }); err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось сохранить согласие.")
			return
		}
		c.Log("согласие на обработку персональных данных", "", true, "")
	}
	c.Redirect("/", "Изменения сохранены.")
}

func (c *Ctx) profile() {
	data := map[string]any{"S": c.S(), "MFARequired": c.mfaRequired(), "TeacherMails": c.teacherMails()}
	if c.Sess.PendingTOTP != "" {
		if sec, err := c.Keys.Open(c.Sess.PendingTOTP); err == nil {
			data["Secret"] = string(sec)
			data["OTPAuth"] = "otpauth://totp/" + url.PathEscape(c.Cfg.PortalName+":"+c.User.Login) + "?secret=" + string(sec) + "&issuer=" + url.QueryEscape(c.Cfg.PortalName)
		}
	}
	data["Sessions"] = sortBy(c.Sessions.Find(func(s *Session) bool { return s.UserID == c.User.ID }), func(a, b *Session) bool { return a.Seen.After(b.Seen) })
	data["SessID"] = c.Sess.ID
	if c.User.GroupID != "" {
		g, _ := c.Groups.Get(c.User.GroupID)
		data["Group"] = g.Name
	}
	c.Render("profile", data)
}

func (c *Ctx) mfaStart() {
	if c.User.TOTPEnabled {
		c.Redirect("/profile", "!Сначала отключите действующий второй фактор.")
		return
	}
	sec := secure.NewTOTPSecret()
	if err := c.Sessions.Update(c.Sess.ID, func(s *Session) error {
		s.PendingTOTP = c.Keys.Seal([]byte(sec))
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось начать подключение второго фактора.")
		return
	}
	c.Redirect("/profile", "")
}

func (c *Ctx) mfaConfirm() {
	if c.User.TOTPEnabled {
		c.Redirect("/profile", "!Двухфакторная аутентификация уже подключена.")
		return
	}
	sec, err := c.Keys.Open(c.Sess.PendingTOTP)
	if err != nil || c.Sess.PendingTOTP == "" {
		c.Redirect("/profile", "!Сначала начните подключение.")
		return
	}
	step, ok := secure.VerifyTOTP(string(sec), c.F("code"), time.Now(), 0, c.S().TOTPSkew)
	if !ok {
		c.Redirect("/profile", "!Код не подошёл. Проверьте время на устройстве и введите новый код.")
		return
	}
	sealed := c.Sess.PendingTOTP
	if err := c.Users.Update(c.User.ID, func(x *User) error {
		x.TOTPSecret, x.TOTPEnabled, x.TOTPStep = sealed, true, step
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось подключить второй фактор.")
		return
	}
	if err := c.Sessions.Update(c.Sess.ID, func(s *Session) error {
		s.PendingTOTP, s.MFAVerified = "", secure.HashToken(sealed)
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Второй фактор подключён. Подтвердите его новым одноразовым кодом.")
		return
	}
	c.Log("подключение двухфакторной аутентификации", "", true, "")
	c.Redirect("/profile", "Двухфакторная аутентификация подключена.")
}

func (c *Ctx) mfaDisable() {
	if c.mfaRequired() {
		c.Redirect("/profile", "!Для вашей роли двухфакторная аутентификация обязательна.")
		return
	}
	if ok, _ := c.Keys.VerifyPassword(c.R.FormValue("password"), c.User.PassHash); !ok {
		c.Log("отключение двухфакторной аутентификации", "", false, "неверный пароль")
		c.Redirect("/profile", "!Неверный пароль.")
		return
	}
	if err := c.revokeSessions(c.User.ID, c.Sess.ID); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось завершить другие сеансы.")
		return
	}
	if err := c.Users.Update(c.User.ID, func(x *User) error {
		x.TOTPSecret, x.TOTPEnabled, x.TOTPStep = "", false, 0
		return nil
	}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось отключить второй фактор.")
		return
	}
	c.Log("отключение двухфакторной аутентификации", "", true, "")
	c.Redirect("/profile", "Двухфакторная аутентификация отключена.")
}

func (c *Ctx) sessionRevoke() {
	id := c.F("id")
	if s, ok := c.Sessions.Get(id); ok && s.UserID == c.User.ID && id != c.Sess.ID {
		c.Sessions.Delete(id)
		c.Log("завершение сеанса", id[:12], true, "")
	}
	c.Redirect("/profile", "Сеанс завершён.")
}

func (a *App) revokeSessions(userID, except string) error {
	var err error
	for _, s := range a.Sessions.Find(func(s *Session) bool { return s.UserID == userID && s.ID != except }) {
		err = errors.Join(err, a.Sessions.Delete(s.ID))
	}
	return err
}
