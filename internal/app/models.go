// Пакет app — прикладная логика и веб-интерфейс ПК «Кафедра».
package app

import "time"

// Роли пользователей.
const (
	RoleAdmin   = "admin"
	RoleTeacher = "teacher"
	RoleStudent = "student"
)

// User — учётная запись.
type User struct {
	ID            string
	Login         string
	FullName      string
	Email         string
	Role          string
	GroupID       string // для учащихся
	PassHash      string
	PassHistory   []string
	PassChanged   time.Time
	MustChange    bool
	Disabled      bool
	Failed        int
	LockedUntil   time.Time
	TOTPSecret    string // зашифрован мастер-ключом
	TOTPEnabled   bool
	TOTPStep      int64
	QuestSent     time.Time `json:",omitempty"` // учащийся отметил, что отправил анкету
	QuestApproved time.Time `json:",omitempty"` // анкета проверена, доступ к обучению открыт
	QuestBy       string    `json:",omitempty"`
	Consent       time.Time // дата согласия с правилами обработки персональных данных
	Created       time.Time
	LastLogin     time.Time
	LastIP        string
}

// Group — учебная группа.
type Group struct {
	ID   string
	Name string
	Note string
}

// Section — раздел (модуль) курса.
type Section struct {
	ID    string
	Title string
}

// Course — учебный курс (дисциплина).
type Course struct {
	ID          string
	Title       string
	Description string
	TeacherIDs  []string
	GroupIDs    []string
	Sections    []Section
	Archived    bool
	Created     time.Time
}

// Виды элементов курса.
const (
	KindLesson = "lesson"
	KindFile   = "file"
	KindTest   = "test"
	KindTask   = "task"
)

// Option — вариант ответа.
type Option struct {
	Text    string
	Correct bool
}

// Question — вопрос теста. Type: single, multi, text.
type Question struct {
	ID      string
	Type    string
	Text    string
	Options []Option
	Answers []string // допустимые ответы для вопросов с вводом
	Points  float64
}

// Item — элемент курса: страница-лекция, файл, тест или задание.
type Item struct {
	ID        string
	CourseID  string
	SectionID string
	Kind      string
	Title     string
	Order     int
	Visible   bool
	Created   time.Time
	Updated   time.Time

	Body    string   // Markdown: текст лекции или условие задания
	FileIDs []string // вложения

	// Оценивание (тест, задание)
	MaxPoints float64
	OpenAt    time.Time
	CloseAt   time.Time // срок сдачи

	// Параметры теста
	TimeLimit   int // минут, 0 — без ограничения
	MaxAttempts int // 0 — без ограничения
	Shuffle     bool
	PoolSize    int // сколько вопросов выдавать из банка, 0 — все
	PassPercent int
	ShowAnswers bool
	HidePoints  bool `json:",omitempty"` // «Скрывать баллы во время прохождения»: учащийся видит только «Вопрос N из M»
	Questions   []Question
}

// Answer — ответ на вопрос.
type Answer struct {
	Opts []int
	Text string
}

// Attempt — попытка прохождения теста.
type Attempt struct {
	ID        string
	ItemID    string
	CourseID  string
	UserID    string
	Started   time.Time
	Deadline  time.Time
	Finished  time.Time
	Done      bool
	Grading   *AttemptGrading `json:",omitempty"`
	Questions []Question      // снимок выданных вопросов (в порядке выдачи)
	Answers   map[string]Answer
	Score     float64
	MaxScore  float64
	Points    float64 // баллы в журнал
	IP        string
}

type AttemptGrading struct {
	MaxPoints   float64
	PassPercent int
}

// Submission — работа, сданная по заданию.
type Submission struct {
	ID        string
	ItemID    string
	CourseID  string
	UserID    string
	Text      string
	FileID    string
	Submitted time.Time
	Late      bool
	Graded    bool
	Points    float64
	Comment   string
	GradedBy  string
	GradedAt  time.Time
}

// Column — столбец журнала, созданный преподавателем вручную.
type Column struct {
	ID        string
	CourseID  string
	Title     string
	MaxPoints float64
	Date      time.Time
}

// Grade — оценка в ручном столбце журнала. ID = ColumnID + ":" + UserID.
type Grade struct {
	ID       string
	ColumnID string
	CourseID string
	UserID   string
	Points   float64
	By       string
	At       time.Time
}

// Event — занятие в расписании.
type Event struct {
	ID        string
	CourseID  string
	GroupID   string
	TeacherID string
	Kind      string
	Start     time.Time
	End       time.Time
	Room      string
	Note      string
}

// Attendance — отметка посещаемости. ID = EventID + ":" + UserID.
// Status: p — присутствовал, a — отсутствовал, e — уважительная причина, l — опоздал.
type Attendance struct {
	ID       string
	EventID  string
	CourseID string
	UserID   string
	Status   string
}

// News — объявление (CourseID пуст для общесистемных).
type News struct {
	ID       string
	CourseID string
	AuthorID string
	Title    string
	Body     string
	Created  time.Time
}

// Post — сообщение в обсуждении курса.
// Post — сообщение чата. Чат курса: CourseID задан; чат учебной группы: задан GroupID.
type Post struct {
	ID       string
	CourseID string
	GroupID  string `json:",omitempty"`
	AuthorID string
	Body     string
	FileID   string `json:",omitempty"` // вложение (изображение, PDF, DOCX)
	Created  time.Time
}

// ChatBan — исключение участника из чата модератором. ID = комната + ":" + UserID.
type ChatBan struct {
	ID     string
	Room   string
	UserID string
	By     string
	At     time.Time
}

// Certificate — выданный сертификат о прохождении курса. Сведения фиксируются на момент выдачи.
type Certificate struct {
	ID           string
	Number       string
	UserID       string
	CourseID     string
	StudentName  string
	CourseTitle  string
	OrgName      string
	Teachers     string
	Percent      float64
	Issued       time.Time
	Mailed       time.Time `json:",omitempty"` // когда сертификат отправлен учащемуся по электронной почте
	MailTo       string    `json:",omitempty"`
	MailNext     time.Time `json:",omitempty"`
	MailAttempts int       `json:",omitempty"`
	MailRevision uint64    `json:",omitempty"` // generation of a manually requeued delivery
	MailFailed   bool      `json:",omitempty"`
	PDFFileID    string    `json:",omitempty"`
	PDFHash      string    `json:",omitempty"`
}

// CertApplication — загруженный учащимся скан подписанного заявления на сертификат. ID = CourseID + ":" + UserID.
type CertApplication struct {
	ID         string
	CourseID   string
	UserID     string
	FileID     string
	Uploaded   time.Time
	Decision   string    `json:",omitempty"`
	ReviewedBy string    `json:",omitempty"`
	Reviewed   time.Time `json:",omitempty"`
}

// PortfolioEntry — запись электронного портфолио учащегося.
type PortfolioEntry struct {
	ID          string
	UserID      string
	Title       string
	Description string
	FileID      string
	Created     time.Time
}

// Progress — факт изучения материала. ID = UserID + ":" + ItemID.
type Progress struct {
	ID       string
	UserID   string
	ItemID   string
	CourseID string
	At       time.Time
}

// Notice — персональное уведомление.
type Notice struct {
	ID      string
	UserID  string
	Text    string
	Link    string
	Created time.Time
	Read    bool
}

// Session — сеанс пользователя. ID — SHA-256 от токена, сам токен не хранится.
type Session struct {
	ID          string
	UserID      string
	CSRF        string
	Created     time.Time
	Seen        time.Time
	IP          string
	UA          string
	MFAVerified string `json:",omitempty"` // версия секрета, подтверждённая в этом сеансе
	PendingTOTP string // секрет на этапе подключения (зашифрован)
}

// FileMeta — сведения о загруженном файле. Содержимое лежит в каталоге files.
// Scope: course — материалы курса, sub — сданные работы, pf — портфолио.
type FileMeta struct {
	ID       string
	Name     string
	Ext      string
	Size     int64
	SHA256   string
	OwnerID  string
	CourseID string
	Scope    string
	Created  time.Time
	ItemID   string `json:",omitempty"`
}

// Settings — настраиваемые администратором параметры.
type Settings struct {
	PwMinLen       int    `json:"password_min_length"`
	PwUpper        bool   `json:"password_need_upper"`
	PwLower        bool   `json:"password_need_lower"`
	PwDigit        bool   `json:"password_need_digit"`
	PwSymbol       bool   `json:"password_need_symbol"`
	PwHistory      int    `json:"password_history"`
	PwMaxAgeDays   int    `json:"password_max_age_days"`
	LockAttempts   int    `json:"lock_after_failed_attempts"`
	LockMinutes    int    `json:"lock_minutes"`
	IPFailLimit    int    `json:"login_failures_per_address"` // неудачных попыток входа с одного адреса за 10 минут до временного отказа
	IdleMinutes    int    `json:"session_idle_minutes"`
	SessionHours   int    `json:"session_max_hours"`
	MaxSessions    int    `json:"max_sessions_per_user"`
	MFARoles       string `json:"mfa_required_for"`       // none, admin, staff, all
	TOTPSkew       int    `json:"totp_time_window_steps"` // допуск расхождения часов для одноразовых кодов, шагов по 30 с
	Grade5         int    `json:"grade5_from_percent"`
	Grade4         int    `json:"grade4_from_percent"`
	Grade3         int    `json:"grade3_from_percent"`
	RatingNames    bool   `json:"rating_show_names"`
	AllowedExt     string `json:"allowed_extensions"`
	MaxUploadMB    int    `json:"max_upload_mb"`
	Banner         string `json:"login_banner"`
	ChatMaxFileMB  int    `json:"chat_max_file_mb"`                 // предельный размер вложения в чате, МБ
	QuestRequired  bool   `json:"questionnaire_required"`           // до проверки анкеты учащийся не допускается к материалам
	QuestEmail     string `json:"questionnaire_email"`              // адрес, на который учащиеся отправляют анкету
	CertNeedsApp   bool   `json:"certificate_requires_application"` // сертификат выдаётся после загрузки подписанного заявления
	TeacherImport  bool   `json:"teachers_import_students"`         // преподаватели могут добавлять учащихся из Excel
	TeacherCourses bool   `json:"teachers_create_courses"`          // преподаватели могут создавать курсы сами
}

// DefaultSettings возвращает безопасные значения по умолчанию.
func DefaultSettings() Settings {
	return Settings{
		PwMinLen: 12, PwUpper: true, PwLower: true, PwDigit: true, PwSymbol: true,
		PwHistory: 5, PwMaxAgeDays: 90, LockAttempts: 5, LockMinutes: 15, IPFailLimit: 100,
		IdleMinutes: 30, SessionHours: 12, MaxSessions: 3, MFARoles: "none", TOTPSkew: 1,
		Grade5: 85, Grade4: 70, Grade3: 50, RatingNames: false,
		AllowedExt:     "pdf docx doc pptx ppt xlsx xls odt odp ods txt md csv png jpg jpeg gif webp mp4 webm mp3 ogg zip 7z html go py c cpp h java js ts sql json xml ipynb",
		MaxUploadMB:    200,
		Banner:         "Доступ разрешён только зарегистрированным пользователям. Действия в системе регистрируются.",
		TeacherCourses: true, TeacherImport: true, CertNeedsApp: true, QuestRequired: true, ChatMaxFileMB: 10,
	}
}
