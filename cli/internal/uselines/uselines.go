// Package uselines breaks a credential use into the lines a person reads it
// in. It is display only: a use is entered on one line, and is stored and
// judged as it was written.
//
// The breaks follow the shape the discobox-access skill teaches — "<job> in
// <target>. Read <what>; <each write>. Never <the lines>." — where that shape
// is there, and nothing is assumed where it is not. Every word of the use is
// kept, in order: the lines joined with single spaces are the use with its
// whitespace collapsed. So a use that does not fit the shape is never shown
// as saying something else, only broken less.
package uselines

import "strings"

// Line is one line of a use as it is shown.
type Line struct {
	Text string
	// Hang is a line that continues the sentence above it: an item of the
	// writes after "Read …;" or of a "Never …" sentence. It is drawn indented
	// under that sentence.
	Hang bool
}

// Split breaks use into lines: a line per sentence, and within a "Read …;"
// or "Never …" sentence a line per item of its list, each hanging under the
// sentence. A use with no such structure is one line.
func Split(use string) []Line {
	var out []Line
	for _, sentence := range sentences(strings.Join(strings.Fields(use), " ")) {
		switch {
		case strings.HasPrefix(sentence, "Read "):
			// What it reads stays whole; the writes after it are the list.
			semi := indexTop(sentence, "; ")
			if semi < 0 {
				out = append(out, Line{Text: sentence})
				continue
			}
			out = append(out, Line{Text: sentence[:semi+1]})
			for _, item := range items(sentence[semi+2:]) {
				out = append(out, Line{Text: item, Hang: true})
			}
		case strings.HasPrefix(sentence, "Never "):
			list := items(sentence)
			out = append(out, Line{Text: list[0]})
			for _, item := range list[1:] {
				out = append(out, Line{Text: item, Hang: true})
			}
		default:
			out = append(out, Line{Text: sentence})
		}
	}
	return out
}

// sentences splits text after each ". " that ends a sentence: one outside
// parentheses and followed by a capital letter, so "v1.2", "e.g. a" and a
// period inside "(…)" do not.
func sentences(text string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth = max(depth-1, 0)
		case '.':
			if depth == 0 && i+2 < len(text) && text[i+1] == ' ' && isUpper(text[i+2]) {
				out = append(out, text[start:i+1])
				start = i + 2
			}
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// items splits a list after each ", " outside parentheses, keeping the comma
// on the item it ends.
func items(list string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth = max(depth-1, 0)
		case ',':
			if depth == 0 && i+1 < len(list) && list[i+1] == ' ' {
				out = append(out, list[start:i+1])
				start = i + 2
			}
		}
	}
	return append(out, list[start:])
}

// indexTop is the index of the first sep in s outside parentheses, or -1.
func indexTop(s, sep string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth = max(depth-1, 0)
		default:
			if depth == 0 && strings.HasPrefix(s[i:], sep) {
				return i
			}
		}
	}
	return -1
}

func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }
