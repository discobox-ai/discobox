#!/bin/sh
# discobox-prompt-answer: print the one JSON document in a wrapper's output.
#
# `discobox-prompt --output-schema` promises exactly one JSON document on
# stdout and nothing else (see harness/DESIGN.md): its caller decodes the answer
# strictly, and anything around it is an answer that cannot be read. Not every
# agent CLI can be told to say only that — one narrates its run, another wraps
# its answer in a code fence — so the wrappers for those pipe through this,
# which is where that normalizing lives once instead of in each of them.
#
# What it prints is the last document that starts a line: a model answering as
# it was asked to puts its JSON on a line of its own, and prose that mentions a
# brace — "the payload is { ... }", an unclosed example, a stray quote — does
# not. Reading every brace in the output as structure would let one unbalanced
# character earlier in a transcript swallow the answer, which fails closed and
# is undiagnosable.
#
# Braces and quotes inside a string are text, not structure. Nothing here
# validates JSON: the caller does that and refuses what it cannot read, so this
# cannot turn prose into a verdict by passing it through.
#
# Exit 1 with nothing on stdout when the output holds no such document, which
# reads to the caller as a wrapper that did not answer.
set -eu

exec awk '
# Lines are kept as they arrive; nothing is accumulated character by character,
# because string building inside a scan is quadratic in the awk Debian ships.
{ line[NR] = $0 }

# opens returns the column at which this line starts a document, or 0.
function opens(s,   i, c) {
	for (i = 1; i <= length(s); i++) {
		c = substr(s, i, 1)
		if (c == " " || c == "\t") { continue }
		return c == "{" ? i : 0
	}
	return 0
}

# closes scans from line "from", column "col", and records in endLine/endCol
# where the document it opens is closed. It returns 1 when one closes.
function closes(from, col,   row, i, c, depth, inString, esc, text) {
	depth = 0; inString = 0; esc = 0
	for (row = from; row <= NR; row++) {
		text = line[row]
		for (i = (row == from ? col : 1); i <= length(text); i++) {
			c = substr(text, i, 1)
			if (inString) {
				if (esc) { esc = 0; continue }
				if (c == "\\") { esc = 1; continue }
				if (c == "\"") { inString = 0 }
				continue
			}
			if (c == "\"") { inString = 1; continue }
			if (c == "{") { depth++; continue }
			if (c == "}") {
				depth--
				if (depth == 0) { endLine = row; endCol = i; return 1 }
			}
		}
	}
	return 0
}

END {
	# The answer comes last, after whatever the CLI said on the way, so the
	# candidates are tried from the end. Only so many: a transcript of nothing
	# but open braces would otherwise be rescanned once per line.
	tried = 0
	for (start = NR; start >= 1; start--) {
		col = opens(line[start])
		if (col == 0) { continue }
		if (++tried > 200) { break }
		if (!closes(start, col)) { continue }
		if (start == endLine) {
			print substr(line[start], col, endCol - col + 1)
			exit 0
		}
		printf "%s\n", substr(line[start], col)
		for (row = start + 1; row < endLine; row++) { print line[row] }
		print substr(line[endLine], 1, endCol)
		exit 0
	}
	exit 1
}
'
