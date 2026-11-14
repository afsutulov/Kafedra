package app

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"kafedra/internal/secure"
)

// Раздел «Общение»: групповые чаты по курсу и по учебной группе.
// Сообщения обновляются периодическим опросом сервера — постоянных соединений нет,
// поэтому чат одинаково работает напрямую и за общим веб-сервером.

// chatFileExt — форматы вложений, разрешённые в чате.
var chatFileExt = []string{"jpg", "jpeg", "png", "pdf", "docx"}

type chatRoom struct {
	Key       string // c-<курс> либо g-<группа>
	Title     string
	Kind      string // «Курс» или «Группа»
	Moderator bool
	courseID  string
	groupID   string
	Count     int
	Last      time.Time
}

// teachesGroup сообщает, ведёт ли преподаватель хотя бы один курс у этой группы.
func (c *Ctx) teachesGroup(groupID string) bool {
	for _, co := range c.coursesOf(c.User) {
		if contains(co.GroupIDs, groupID) {
			return true
		}
	}
	return false
}

// chatRoomByKey возвращает комнату, если текущий пользователь в ней состоит и не исключён.
func (c *Ctx) chatRoomByKey(key string) (*chatRoom, bool) {
	kind, id, ok := strings.Cut(key, "-")
	if !ok || id == "" {
		return nil, false
	}
	r := &chatRoom{Key: key}
	switch kind {
	case "c":
		co, ok := c.Courses.Get(id)
		if !ok || !c.canView(&co) {
			return nil, false
		}
		r.Title, r.Kind, r.courseID, r.Moderator = co.Title, "Курс", co.ID, c.canManage(&co)
	case "g":
		g, ok := c.Groups.Get(id)
		if !ok {
			return nil, false
		}
		switch c.User.Role {
		case RoleAdmin:
			r.Moderator = true
		case RoleTeacher:
			if !c.teachesGroup(g.ID) {
				return nil, false
			}
			r.Moderator = true
		default:
			if c.User.GroupID != g.ID {
				return nil, false
			}
		}
		r.Title, r.Kind, r.groupID = "Группа "+g.Name, "Группа", g.ID
	default:
		return nil, false
	}
	if !r.Moderator {
		if _, banned := c.ChatBans.Get(key + ":" + c.User.ID); banned {
			return nil, false
		}
	}
	return r, true
}

func (r *chatRoom) has(p *Post) bool {
	if r.courseID != "" {
		return p.CourseID == r.courseID && p.GroupID == ""
	}
	return p.GroupID == r.groupID && p.CourseID == ""
}

// chatRooms перечисляет комнаты, доступные пользователю.
func (c *Ctx) chatRooms() []chatRoom {
	var keys []string
	groups := map[string]bool{}
	for _, co := range c.coursesOf(c.User) {
		keys = append(keys, "c-"+co.ID)
		if c.User.Role != RoleStudent {
			for _, g := range co.GroupIDs {
				groups[g] = true
			}
		}
	}
	if c.User.Role == RoleStudent && c.User.GroupID != "" {
		groups[c.User.GroupID] = true
	}
	if c.User.Role == RoleAdmin {
		for _, g := range c.Groups.All() {
			groups[g.ID] = true
		}
	}
	for g := range groups {
		keys = append(keys, "g-"+g)
	}
	type stat struct {
		count int
		last  time.Time
	}
	stats := map[string]stat{}
	for _, p := range c.Posts.All() {
		key := ""
		if p.CourseID != "" && p.GroupID == "" {
			key = "c-" + p.CourseID
		}
		if p.GroupID != "" && p.CourseID == "" {
			key = "g-" + p.GroupID
		}
		if key == "" {
			continue
		}
		v := stats[key]
		v.count++
		if p.Created.After(v.last) {
			v.last = p.Created
		}
		stats[key] = v
	}
	var out []chatRoom
	for _, k := range keys {
		r, ok := c.chatRoomByKey(k)
		if !ok {
			continue
		}
		r.Count, r.Last = stats[k].count, stats[k].last
		out = append(out, *r)
	}
	return sortBy(out, func(x, y *chatRoom) bool {
		if x.Kind != y.Kind {
			return x.Kind > y.Kind // сначала курсы
		}
		return x.Title < y.Title
	})
}

func (c *Ctx) chatList() {
	c.Render("chat_list", map[string]any{"Rooms": c.chatRooms()})
}

type chatMsg struct {
	Post     Post
	Author   string
	Role     string
	Mine     bool
	File     *FileMeta
	FileType string
}

func (c *Ctx) chatMessages(r *chatRoom) []chatMsg {
	posts := c.Posts.Top(300, r.has, func(x, y *Post) bool {
		if x.Created.Equal(y.Created) {
			return x.ID > y.ID
		}
		return x.Created.After(y.Created)
	})
	authors := map[string]User{}
	var out []chatMsg
	for i := len(posts) - 1; i >= 0; i-- {
		p := posts[i]
		u, cached := authors[p.AuthorID]
		if !cached {
			var found bool
			u, found = c.Users.Get(p.AuthorID)
			if !found {
				u.FullName = "(удалён)"
			}
			authors[p.AuthorID] = u
		}
		m := chatMsg{Post: p, Author: u.FullName, Role: u.Role, Mine: p.AuthorID == c.User.ID}
		if p.FileID != "" {
			if f, ok := c.Files.Get(p.FileID); ok {
				m.File, m.FileType = &f, fileType(f.Ext)
			}
		}
		out = append(out, m)
	}
	return out
}

// chatMembers возвращает учащихся комнаты (для модератора) и признак исключения.
func (c *Ctx) chatMembers(r *chatRoom) (members []User, banned map[string]bool) {
	banned = map[string]bool{}
	for _, b := range c.ChatBans.Find(func(b *ChatBan) bool { return b.Room == r.Key }) {
		banned[b.UserID] = true
	}
	if r.courseID != "" {
		if co, ok := c.Courses.Get(r.courseID); ok {
			members = c.students(&co, "")
		}
	} else {
		members = sortBy(c.Users.Find(func(u *User) bool { return u.Role == RoleStudent && !u.Disabled && u.GroupID == r.groupID }),
			func(x, y *User) bool { return x.FullName < y.FullName })
	}
	return
}

func (c *Ctx) chatData(r *chatRoom) map[string]any {
	data := map[string]any{"Room": r, "Msgs": c.chatMessages(r), "MaxMB": c.S().ChatMaxFileMB}
	if r.Moderator {
		data["Members"], data["Banned"] = c.chatMembers(r)
	}
	return data
}

func (c *Ctx) room() (*chatRoom, bool) {
	r, ok := c.chatRoomByKey(c.R.PathValue("room"))
	if !ok {
		c.Deny()
	}
	return r, ok
}

func (c *Ctx) chatView() {
	r, ok := c.room()
	if !ok {
		return
	}
	c.Render("chat", c.chatData(r))
}

// chatFragment отдаёт только ленту сообщений — её периодически запрашивает страница чата.
func (c *Ctx) chatFragment() {
	r, ok := c.chatRoomByKey(c.R.PathValue("room"))
	if !ok {
		c.W.WriteHeader(http.StatusForbidden)
		return
	}
	data := map[string]any{"Room": r, "Msgs": c.chatMessages(r)}
	data["Me"], data["CSRF"] = c.User, c.Sess.CSRF
	c.W.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.W.Header().Set("X-Kafedra-Fragment", "chat")
	c.tpl["chat"].ExecuteTemplate(c.W, "messages", data)
}

func (c *Ctx) chatPost() {
	r, ok := c.room()
	if !ok {
		return
	}
	back := "/chat/" + r.Key
	body := c.F("body")
	if len([]rune(body)) > 5000 {
		c.Redirect(back, "!Сообщение слишком длинное (не более 5000 знаков).")
		return
	}
	p := Post{ID: secure.NewID(), CourseID: r.courseID, GroupID: r.groupID, AuthorID: c.User.ID, Body: body, Created: time.Now()}
	if c.R.MultipartForm != nil && len(c.R.MultipartForm.File["file"]) > 1 {
		c.Redirect(back, "!Прикрепите не более одного файла.")
		return
	}
	if c.R.MultipartForm != nil && len(c.R.MultipartForm.File["file"]) == 1 {
		fh := c.R.MultipartForm.File["file"][0]
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fh.Filename), "."))
		limit := int64(max(c.S().ChatMaxFileMB, 1)) << 20
		switch {
		case !contains(chatFileExt, ext):
			c.Log("чат: вложение отклонено", r.Title, false, fh.Filename)
			c.Redirect(back, "!В чат можно отправлять только изображения (JPG, PNG), PDF и DOCX.")
			return
		case fh.Size > limit:
			c.Redirect(back, fmt.Sprintf("!Файл больше %d МБ.", limit>>20))
			return
		}
		if err := validateRestrictedUpload(fh, ext); err != nil {
			c.Redirect(back, "!Файл не принят: "+err.Error()+".")
			return
		}
		m, err := c.saveUpload(fh, r.Key, "chat", p.ID)
		if err != nil {
			c.Redirect(back, "!Файл не принят: "+err.Error()+".")
			return
		}
		p.FileID = m.ID
	}
	if p.Body == "" && p.FileID == "" {
		c.Redirect(back, "!Введите сообщение или приложите файл.")
		return
	}
	if err := c.Posts.Put(p.ID, p); err != nil {
		c.removeFile(p.FileID)
		c.Fail(http.StatusServiceUnavailable, "Не удалось отправить сообщение.")
		return
	}
	c.Redirect(back, "")
}

func (c *Ctx) chatDelete() {
	r, ok := c.room()
	if !ok {
		return
	}
	p, ok := c.Posts.Get(c.F("id"))
	if !ok || !r.has(&p) || (p.AuthorID != c.User.ID && !r.Moderator) {
		c.Deny()
		return
	}
	if err := c.Posts.Delete(p.ID); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось удалить сообщение.")
		return
	}
	c.removeFile(p.FileID)
	if p.AuthorID != c.User.ID {
		c.Log("чат: сообщение удалено модератором", r.Title, true, "автор: "+c.userName(p.AuthorID))
	}
	c.Redirect("/chat/"+r.Key, "Сообщение удалено.")
}

// chatBan — исключение участника из чата и возврат в чат (только модератор).
func (c *Ctx) chatBan() {
	r, ok := c.room()
	if !ok {
		return
	}
	if !r.Moderator {
		c.Deny()
		return
	}
	u, ok := c.Users.Get(c.F("user"))
	members, _ := c.chatMembers(r)
	member := false
	for _, m := range members {
		member = member || m.ID == u.ID
	}
	if !ok || !member {
		c.Redirect("/chat/"+r.Key, "!Участник не найден в этом чате.")
		return
	}
	id := r.Key + ":" + u.ID
	if c.F("action") == "restore" {
		if err := c.ChatBans.Delete(id); err != nil {
			c.Fail(http.StatusServiceUnavailable, "Не удалось вернуть участника.")
			return
		}
		c.Log("чат: участник возвращён", r.Title, true, u.Login)
		c.Redirect("/chat/"+r.Key, u.FullName+" снова участвует в чате.")
		return
	}
	if err := c.ChatBans.Put(id, ChatBan{ID: id, Room: r.Key, UserID: u.ID, By: c.User.ID, At: time.Now()}); err != nil {
		c.Fail(http.StatusServiceUnavailable, "Не удалось исключить участника.")
		return
	}
	c.Log("чат: участник исключён", r.Title, true, u.Login)
	c.notify(u.ID, "Вы исключены из чата «"+r.Title+"» модератором.", "")
	c.Redirect("/chat/"+r.Key, u.FullName+" исключён(а) из чата.")
}

func (c *Ctx) canReadChatFile(m FileMeta) bool {
	room, ok := c.chatRoomByKey(m.CourseID)
	if !ok {
		return false
	}
	if m.ItemID != "" {
		p, found := c.Posts.Get(m.ItemID)
		return found && p.FileID == m.ID && room.has(&p)
	}
	_, found := c.Posts.First(func(p *Post) bool { return p.FileID == m.ID && room.has(p) })
	return found
}

// dropChat удаляет сообщения, вложения и исключения комнаты (при удалении курса или группы).
func (a *App) dropChat(match func(*Post) bool, roomKey string) {
	for _, p := range a.Posts.Find(match) {
		if a.Posts.Delete(p.ID) == nil {
			a.removeFile(p.FileID)
		}
	}
	for _, b := range a.ChatBans.Find(func(b *ChatBan) bool { return b.Room == roomKey }) {
		a.ChatBans.Delete(b.ID)
	}
}
