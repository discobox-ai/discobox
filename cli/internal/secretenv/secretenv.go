// Package secretenv names the environment variable a secret is bound to when
// nobody names one: the secret's name, upper-snaked, so github is GITHUB and
// npm-token is NPM_TOKEN. `discobox run --bind` and the console's binding form
// default to it, and spelling it once keeps the two from disagreeing about
// which variable a secret lands in.
package secretenv

import "strings"

// Name is secret's name as an environment variable: letters upper-cased,
// digits kept, and every run of anything else one underscore. A name that
// would start with a digit gains a leading underscore, which a variable needs.
func Name(secret string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.TrimSpace(secret) {
		switch {
		case r >= 'a' && r <= 'z':
			r -= 'a' - 'A'
			fallthrough
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
		default:
			gap = true
		}
	}
	name := b.String()
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	return name
}
