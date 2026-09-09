package main

import (
	"testing"

	progresspkg "github.com/igor-zatochniy/tts-reader/internal/progress"
)

func TestProgressFuzzFixtureRestoresRealBookPositions(t *testing.T) {
	for _, text := range []string{"", "Hello", "Аудіо 😀."} {
		t.Run(text, func(t *testing.T) {
			app, b := newProgressLoadFixture(t, []byte(text))
			for position := range text {
				assertProgressRestore(t, app, b, int64(position))
			}
			assertProgressRestore(t, app, b, b.Size)
		})
	}
}

func TestProgressFuzzFixtureRejectsInvalidProgress(t *testing.T) {
	app, b := newProgressLoadFixture(t, []byte("Аудіо 😀."))
	cases := []struct {
		name   string
		mutate func(*progresspkg.Progress)
	}{
		{"inside_cyrillic_rune", func(p *progresspkg.Progress) { p.LastPosition = 1 }},
		{"inside_emoji", func(p *progresspkg.Progress) { p.LastPosition = 12 }},
		{"negative", func(p *progresspkg.Progress) { p.LastPosition = -1 }},
		{"past_eof", func(p *progresspkg.Progress) { p.LastPosition = b.Size + 1 }},
		{"wrong_fingerprint", func(p *progresspkg.Progress) { p.BookFingerprint = "wrong" }},
		{"wrong_mtime", func(p *progresspkg.Progress) { p.BookModifiedAtUnixNano++ }},
		{"wrong_size", func(p *progresspkg.Progress) { p.BookSize++ }},
		{"wrong_version", func(p *progresspkg.Progress) { p.Version-- }},
		{"wrong_unit", func(p *progresspkg.Progress) { p.PositionUnit = "runes" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Спершу доводимо, що fixture придатний до відновлення без пошкодження.
			assertProgressRestore(t, app, b, 2)
			p := progresspkg.ProgressForBook(b, 2)
			tc.mutate(&p)
			pos, hasSave, err := loadProgressData(t, app, mustProgressJSON(t, p))
			if err == nil || pos != 0 || hasSave {
				t.Fatalf("некоректний progress прийнято: position=%d, hasSave=%v, err=%v", pos, hasSave, err)
			}
		})
	}
}
