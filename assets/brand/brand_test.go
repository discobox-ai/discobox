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

func TestDimensionsReadTheShapeTokens(t *testing.T) {
	want := map[string]float64{"radius-frame": 12, "radius-key": 4, "stroke-focus": 1.5, "space-1": 4}
	got := map[string]float64{}
	for _, d := range Dimensions() {
		got[d.Name] = d.Points
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %v, want %v", name, got[name], v)
		}
	}
	if _, ok := got["step-0"]; ok {
		t.Error("the type scale is rem on a page, not a length in points")
	}
}

func TestParseDimensionsRefusesALengthNotInPx(t *testing.T) {
	for name, doc := range map[string]string{
		"rem":      `{"space":{"a":{"$type":"dimension","$value":"1rem"}},"radius":{"b":{"$type":"dimension","$value":"4px"}},"stroke":{"c":{"$type":"dimension","$value":"1px"}}}`,
		"no group": `{"space":{"a":{"$type":"dimension","$value":"4px"}}}`,
		"negative": `{"space":{"a":{"$type":"dimension","$value":"-4px"}},"radius":{"b":{"$type":"dimension","$value":"4px"}},"stroke":{"c":{"$type":"dimension","$value":"1px"}}}`,
	} {
		if _, err := parseDimensions([]byte(doc)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
