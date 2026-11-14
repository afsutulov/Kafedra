package app

import "time"

// One request-local index: progress, analytics and certificate eligibility use
// the same criteria. No persistent cache to invalidate after grading or edits.
type completionState struct {
	items map[string]Item
	done  map[string]map[string]bool
}

func (s completionState) complete(userID string) bool {
	return len(s.items) > 0 && len(s.done[userID]) == len(s.items)
}

func (a *App) courseCompletion(courseID, userID string, from, to time.Time) completionState {
	s := completionState{items: map[string]Item{}, done: map[string]map[string]bool{}}
	for _, it := range a.courseItems(courseID, true) {
		s.items[it.ID] = it
	}
	add := func(user, item string) {
		if s.done[user] == nil {
			s.done[user] = map[string]bool{}
		}
		s.done[user][item] = true
	}
	for _, at := range a.Attempts.Find(func(at *Attempt) bool {
		return at.CourseID == courseID && (userID == "" || at.UserID == userID) && at.Done && inPeriod(at.Finished, from, to)
	}) {
		if it, ok := s.items[at.ItemID]; ok && it.Kind == KindTest && at.passed(&it) {
			add(at.UserID, it.ID)
		}
	}
	for _, sub := range a.Subs.Find(func(sub *Submission) bool {
		return sub.CourseID == courseID && (userID == "" || sub.UserID == userID) && sub.Graded && inPeriod(sub.GradedAt, from, to)
	}) {
		if it, ok := s.items[sub.ItemID]; ok && it.Kind == KindTask && sub.ID == it.ID+":"+sub.UserID {
			add(sub.UserID, it.ID)
		}
	}
	for _, p := range a.Progress.Find(func(p *Progress) bool {
		return p.CourseID == courseID && (userID == "" || p.UserID == userID) && inPeriod(p.At, from, to)
	}) {
		if it, ok := s.items[p.ItemID]; ok && (it.Kind == KindLesson || it.Kind == KindFile) && p.ID == p.UserID+":"+it.ID {
			add(p.UserID, it.ID)
		}
	}
	return s
}
