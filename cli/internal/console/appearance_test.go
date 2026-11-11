package console

import (
	"slices"
	"testing"
)

// The theme question offers every appearance in order, the one the window is
// drawn in now shown but not to be chosen again; anything kept that names
// none is the system's.
func TestThemeQuestionOffersAChange(t *testing.T) {
	for _, now := range []Appearance{AppearanceSystem, AppearanceDark, AppearanceLight, "", "sepia"} {
		q := ThemeQuestion(now)
		var keys, closed []string
		for _, c := range q.Choices {
			keys = append(keys, c.Key)
			if c.Why != "" {
				closed = append(closed, c.Key)
			}
		}
		if !slices.Equal(keys, []string{"system", "dark", "light"}) {
			t.Errorf("%q: the choices %v", now, keys)
		}
		if want := []string{string(ParseAppearance(string(now)))}; !slices.Equal(closed, want) {
			t.Errorf("%q: closed %v, want %v", now, closed, want)
		}
	}
}

// Only a window that draws its own palette takes the theme command; the
// terminal console says why it has none.
func TestThemeIsAThemedWindowsOnly(t *testing.T) {
	r := NewRegistry(servers{})
	e, _ := r.Entry(Context{}, "theme", nil)
	if e.Enabled || e.Why != ThemeUnavailable {
		t.Errorf("the terminal console's theme: %+v", e)
	}
	if got := Dispatch(r, Frontend[string](&recorder{}), Context{}, "theme", nil); got != "unavailable: "+ThemeUnavailable {
		t.Errorf("the terminal console's theme dispatched to %q", got)
	}
	if e, _ := r.Entry(Context{Themed: true}, "theme", nil); !e.Enabled {
		t.Errorf("the desktop's theme: %+v", e)
	}
	if got := Dispatch(r, Frontend[string](&recorder{}), Context{Themed: true}, "theme", nil); got != "theme" {
		t.Errorf("the desktop's theme dispatched to %q", got)
	}
}
