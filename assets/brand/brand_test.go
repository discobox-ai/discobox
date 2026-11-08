package brand

import "testing"

func TestColorsReadTheTokens(t *testing.T) {
	gold, ok := LookupColor("gold")
	if !ok {
		t.Fatal("no gold")
	}
	if gold.Dark != "#ffd700" || gold.Light == "" || gold.XTerm != 220 {
		t.Errorf("gold = %+v", gold)
	}
	text, ok := LookupColor("text")
	if !ok || text.XTerm != 0 {
		t.Errorf("text = %+v, %v; the TUI draws it in the terminal's foreground, by no index", text, ok)
	}
	all := Colors()
	for i := 1; i < len(all); i++ {
		if all[i-1].Name >= all[i].Name {
			t.Errorf("colors are not ordered by name: %s before %s", all[i-1].Name, all[i].Name)
		}
	}
}

func TestParseColorsRefusesAMalformedRole(t *testing.T) {
	for name, doc := range map[string]string{
		"no light":    `{"color":{"x":{"$type":"color","$value":"#000000"}}}`,
		"short value": `{"color":{"x":{"$type":"color","$value":"#000","$extensions":{"ai.discobox":{"light":"#ffffff"}}}}}`,
		"low xterm":   `{"color":{"x":{"$type":"color","$value":"#000000","$extensions":{"ai.discobox":{"light":"#ffffff","xterm":13}}}}}`,
	} {
		if _, err := parseColors([]byte(doc)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
