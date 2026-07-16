package bot

import (
	"strings"
	"testing"
)

func TestBuildProfileFacts(t *testing.T) {
	tests := []struct {
		name     string
		facts    string
		contains []string
		absent   []string
	}{
		{
			name:     "полный профиль",
			facts:    buildProfileFacts("Иван", "Петров", "ivan", "Люблю бег", true, 3),
			contains: []string{"Имя: Иван Петров.", "Ник: @ivan.", "О себе: Люблю бег", "Фото профиля: 3."},
		},
		{
			name:     "пустой профиль (приватность) — нейтральные формулировки",
			facts:    buildProfileFacts("小美", "", "", "", true, 0),
			contains: []string{"Имя: 小美.", "Ник: не задан.", "О себе: пусто.", "Фото профиля: 0."},
		},
		{
			name:     "bio недоступно (getChat упал) и фото неизвестно",
			facts:    buildProfileFacts("X", "", "a83nfk29", "", false, -1),
			contains: []string{"Ник: @a83nfk29.", "О себе: недоступно."},
			absent:   []string{"Фото профиля"},
		},
		{
			name:     "без имени",
			facts:    buildProfileFacts("", "", "", "", true, 1),
			contains: []string{"Имя: (без имени)."},
		},
		{
			name:     "длинное bio обрезается",
			facts:    buildProfileFacts("A", "", "", strings.Repeat("х", 600), true, 1),
			contains: []string{"…"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range tc.contains {
				if !strings.Contains(tc.facts, want) {
					t.Errorf("facts must contain %q:\n%s", want, tc.facts)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(tc.facts, bad) {
					t.Errorf("facts must NOT contain %q:\n%s", bad, tc.facts)
				}
			}
		})
	}
}

func TestProfileVoteText(t *testing.T) {
	got := profileVoteText("<a href=\"tg://user?id=1\">Иван</a>", 2, 1, 3)
	for _, want := range []string{"Иван", "перевес в 3 голоса", "Забанить: <b>2</b>", "Оставить: <b>1</b>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
