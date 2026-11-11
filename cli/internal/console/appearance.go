package console

// Appearance is the palette a window that draws its own picks (the desktop
// app): the system's, unless the person picks dark or light, which the next
// window opens on as well (DataSource.SaveAppearance).
// The terminal console draws in its terminal's colors and picks none.
type Appearance string

// The appearances, in the order the theme question offers them.
const (
	// AppearanceSystem follows the system's light or dark.
	AppearanceSystem Appearance = "system"
	// AppearanceDark is the dark palette whatever the system's.
	AppearanceDark Appearance = "dark"
	// AppearanceLight is the light palette whatever the system's.
	AppearanceLight Appearance = "light"
)

// Appearances is every appearance, in the order the theme question offers
// them.
var Appearances = []Appearance{AppearanceSystem, AppearanceDark, AppearanceLight}

// ParseAppearance is an appearance as it was kept or answered; anything it
// does not name, the empty string included, is the system's, which is where a
// window opens without a choice.
func ParseAppearance(s string) Appearance {
	switch a := Appearance(s); a {
	case AppearanceDark, AppearanceLight:
		return a
	}
	return AppearanceSystem
}

// ThemeUnavailable is why the theme command does nothing in a window that
// draws in its terminal's colors.
const ThemeUnavailable = "the terminal console draws in your terminal's colors"

// ThemeQuestion is the theme command's question: which appearance the window
// draws in, the one it has now offered beside the others but not to be chosen
// again, so the question opens on a change.
func ThemeQuestion(now Appearance) Question {
	q := Question{Title: "Theme", Body: "Draw the window dark, light, or as the system does."}
	for _, a := range Appearances {
		c := Choice{Key: string(a), Label: appearanceLabels[a], Hint: appearanceHints[a]}
		if a == ParseAppearance(string(now)) {
			c.Why = "the window is drawn this way now"
		}
		q.Choices = append(q.Choices, c)
	}
	return q
}

var appearanceLabels = map[Appearance]string{
	AppearanceSystem: "System",
	AppearanceDark:   "Dark",
	AppearanceLight:  "Light",
}

var appearanceHints = map[Appearance]string{
	AppearanceSystem: "follow the system's light or dark appearance",
	AppearanceDark:   "dark, whatever the system's",
	AppearanceLight:  "light, whatever the system's",
}
