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
	ID          string
	Login       string
	FullName    string
	Email       string
	Role        string
	GroupID     string // для учащихся
	PassHash    string
	PassHistory []string
	PassChanged time.Time
	MustChange  bool
	Disabled    bool
	Failed      int
	LockedUntil time.Time
	TOTPSecret  string // зашифрован мастер-ключом
	TOTPEnabled bool
	TOTPStep    int64
	Consent     time.Time // дата согласия с правилами обработки персональных данных
	Created     time.Time
	LastLogin   time.Time
	LastIP      string
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
	Questions []Question // снимок выданных вопросов (в порядке выдачи)
	Answers   map[string]Answer
	Score     float64
	MaxScore  float64
	Points    float64 // баллы в журнал
	IP        string
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
type Post struct {
	ID       string
	CourseID string
	AuthorID string
	Body     string
	Created  time.Time
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
	MFA         bool   // второй фактор пройден либо не требуется
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
}

// Settings — настраиваемые администратором параметры.
type Settings struct {
	PwMinLen       int
	PwUpper        bool
	PwLower        bool
	PwDigit        bool
	PwSymbol       bool
	PwHistory      int
	PwMaxAgeDays   int
	LockAttempts   int
	LockMinutes    int
	IdleMinutes    int
	SessionHours   int
	MaxSessions    int
	MFARoles       string // none, admin, staff, all
	Grade5         int
	Grade4         int
	Grade3         int
	RatingNames    bool
	AllowedExt     string
	MaxUploadMB    int
	Banner         string
	TeacherCourses bool // преподаватели могут создавать курсы сами
}

// DefaultSettings возвращает безопасные значения по умолчанию.
func DefaultSettings() Settings {
	return Settings{
		PwMinLen: 12, PwUpper: true, PwLower: true, PwDigit: true, PwSymbol: true,
		PwHistory: 5, PwMaxAgeDays: 90, LockAttempts: 5, LockMinutes: 15,
		IdleMinutes: 30, SessionHours: 12, MaxSessions: 3, MFARoles: "admin",
		Grade5: 85, Grade4: 70, Grade3: 50, RatingNames: false,
		AllowedExt:     "pdf docx doc pptx ppt xlsx xls odt odp ods txt md csv png jpg jpeg gif webp mp4 webm mp3 ogg zip 7z html go py c cpp h java js ts sql json xml ipynb",
		MaxUploadMB:    200,
		Banner:         "Доступ разрешён только зарегистрированным пользователям. Действия в системе регистрируются.",
		TeacherCourses: true,
	}
}
