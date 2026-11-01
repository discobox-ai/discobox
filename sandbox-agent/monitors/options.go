package monitors

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// optionStart matches where an option begins inside a comma-joined `-o`
// value: a name and an `=`. A comma not followed by one is part of the value
// before it, so `-o pr=412,413` is one option and `-o repo=a/b,pr=4` two.
var optionStart = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*=`)

// ParseOptions reads `-o` values: each `key=value`, or several joined by
// commas. A key given twice is refused rather than one silently winning.
func ParseOptions(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, value := range values {
		for _, part := range splitOptions(value) {
			key, val, ok := strings.Cut(part, "=")
			key = strings.TrimSpace(key)
			if !ok || key == "" {
				return nil, fmt.Errorf("option %q is not key=value", part)
			}
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("option %q is given twice", key)
			}
			out[key] = strings.TrimSpace(val)
		}
	}
	return out, nil
}

func splitOptions(value string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] == ',' && optionStart.MatchString(value[i+1:]) {
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

// CheckOptions refuses options the monitor does not declare and required ones
// that are missing, naming each, before anything is enabled.
func CheckOptions(def Definition, opts map[string]string) error {
	var unknown, missing []string
	for key := range opts {
		if _, ok := def.Option(key); !ok {
			unknown = append(unknown, key)
		}
	}
	for _, opt := range def.Options {
		if opt.Required && strings.TrimSpace(opts[opt.Name]) == "" {
			missing = append(missing, opt.Name)
		}
	}
	sort.Strings(unknown)
	var problems []string
	if len(unknown) > 0 {
		declared := make([]string, 0, len(def.Options))
		for _, opt := range def.Options {
			declared = append(declared, opt.Name)
		}
		known := "it takes none"
		if len(declared) > 0 {
			known = "it takes " + strings.Join(declared, ", ")
		}
		problems = append(problems, fmt.Sprintf("%s does not take %s (%s)", def.ID, strings.Join(unknown, ", "), known))
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("%s requires %s", def.ID, strings.Join(missing, ", ")))
	}
	if _, err := def.IntervalFor(opts); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}
