package app

import (
	"fmt"
	"time"

	"kafedra/internal/secure"
)

// SeedDemo наполняет пустую систему демонстрационными данными и возвращает
// созданные учётные записи с временными паролями (логин → пароль).
func (a *App) SeedDemo() ([][2]string, error) {
	var creds [][2]string
	mk := func(login, name, role, group string) (User, error) {
		pw := secure.GeneratePassword(14)
		u, err := a.CreateUser(login, name, login+"@example.edu", role, group, pw)
		if err == nil {
			creds = append(creds, [2]string{login, pw})
		}
		return u, err
	}
	g1, g2 := Group{ID: secure.NewID(), Name: "ПИ-21", Note: "Программная инженерия, 2 курс"}, Group{ID: secure.NewID(), Name: "ИБ-22", Note: "Информационная безопасность, 2 курс"}
	a.Groups.Put(g1.ID, g1)
	a.Groups.Put(g2.ID, g2)
	teacher, err := mk("teacher", "Смирнова Анна Викторовна", RoleTeacher, "")
	if err != nil {
		return nil, err
	}
	names := []string{"Иванов Пётр Сергеевич", "Кузнецова Мария Олеговна", "Васильев Дмитрий Андреевич", "Соколова Елена Игоревна"}
	var students []User
	for i, n := range names {
		g := g1.ID
		if i == 3 {
			g = g2.ID
		}
		s, err := mk(fmt.Sprintf("student%d", i+1), n, RoleStudent, g)
		if err != nil {
			return nil, err
		}
		students = append(students, s)
	}
	now := time.Now()
	s1, s2 := Section{ID: secure.NewID(), Title: "Модуль 1. Основы языка"}, Section{ID: secure.NewID(), Title: "Модуль 2. Конкурентность"}
	co := Course{ID: secure.NewID(), Title: "Программирование на Go", Created: now, TeacherIDs: []string{teacher.ID}, GroupIDs: []string{g1.ID, g2.ID},
		Sections: []Section{s1, s2}, Description: "Курс знакомит с языком **Go**: типы, функции, интерфейсы, горутины и каналы.\n\nИтоговая оценка складывается из тестов, практических заданий и работы на семинарах."}
	a.Courses.Put(co.ID, co)
	lesson := Item{ID: secure.NewID(), CourseID: co.ID, SectionID: s1.ID, Kind: KindLesson, Title: "Лекция 1. Структура программы", Order: 1, Visible: true, Created: now, Updated: now,
		Body: "# Структура программы\n\nПрограмма на Go состоит из *пакетов*. Точка входа — функция `main` пакета `main`.\n\n```go\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n    fmt.Println(\"Здравствуй, мир!\")\n}\n```\n\n## Основные команды\n\n| Команда | Назначение |\n| --- | --- |\n| `go build` | сборка исполняемого файла |\n| `go test` | запуск тестов |\n| `go vet` | статический анализ |\n\n> Совет: форматируйте код командой `gofmt` перед каждой фиксацией изменений.\n\n1. Установите инструментарий.\n2. Создайте модуль: `go mod init пример`.\n3. Запустите программу.\n"}
	a.Items.Put(lesson.ID, lesson)
	qs, _ := ParseQuestions("? Какая функция является точкой входа в программу на Go?\n+ main\n- init\n- start\n- run\n\n? Отметьте встроенные типы Go\n+ int\n+ string\n- char\n+ bool\n* 2\n\n? Какой командой запускаются тесты? (введите команду целиком)\n= go test\n")
	test := Item{ID: secure.NewID(), CourseID: co.ID, SectionID: s1.ID, Kind: KindTest, Title: "Тест 1. Основы", Order: 2, Visible: true, Created: now, Updated: now,
		MaxPoints: 10, TimeLimit: 15, MaxAttempts: 2, Shuffle: true, PassPercent: 50, ShowAnswers: true, Questions: qs, CloseAt: now.AddDate(0, 0, 14)}
	a.Items.Put(test.ID, test)
	task := Item{ID: secure.NewID(), CourseID: co.ID, SectionID: s2.ID, Kind: KindTask, Title: "Практическая работа 1. Конвейер на каналах", Order: 3, Visible: true, Created: now, Updated: now,
		MaxPoints: 20, CloseAt: now.AddDate(0, 0, 10), Body: "Реализуйте конвейер из трёх стадий, связанных каналами:\n\n- генерация чисел;\n- возведение в квадрат;\n- суммирование.\n\nПриложите архив с исходным кодом и краткий отчёт."}
	a.Items.Put(task.ID, task)
	col := Column{ID: secure.NewID(), CourseID: co.ID, Title: "Семинар 1", MaxPoints: 5, Date: now}
	a.Columns.Put(col.ID, col)
	for i, s := range students[:3] {
		id := col.ID + ":" + s.ID
		a.Grades.Put(id, Grade{ID: id, ColumnID: col.ID, CourseID: co.ID, UserID: s.ID, Points: float64(5 - i), By: teacher.ID, At: now})
	}
	day := a.weekStart(now).AddDate(0, 0, 7) // понедельник следующей недели
	for w := 0; w < 4; w++ {
		for i, k := range []string{"lec", "prac"} {
			st := day.AddDate(0, 0, 7*w+i*2).Add(9 * time.Hour)
			id := secure.NewID()
			a.Events.Put(id, Event{ID: id, CourseID: co.ID, GroupID: g1.ID, TeacherID: teacher.ID, Kind: k, Start: st, End: st.Add(90 * time.Minute), Room: "305"})
		}
	}
	nid := secure.NewID()
	a.NewsC.Put(nid, News{ID: nid, CourseID: co.ID, AuthorID: teacher.ID, Title: "Добро пожаловать на курс", Body: "Материалы первого модуля опубликованы. Тест 1 открыт в течение двух недель.", Created: now})
	return creds, nil
}
