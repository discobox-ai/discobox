package tui

import (
	"charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
)

// markdownStyle is how a comment's markdown is drawn: the window's own
// palette, not the renderer's. Its stock theme is a document's — a body color
// of its own, headings in a blue found nowhere else here, inline code as a red
// chip on a painted field — and a comment in the issue tab is not a document
// beside the window, it is part of it. So the text is the terminal's own, a
// heading is what a title is everywhere else (the accent, bold), code in a
// sentence is the blue a port is in the header, with no field behind it, a
// link is that blue too, and everything secondary is the dim the window
// already uses.
//
// A block of code is highlighted, in discobox-review's colors rather than the
// renderer's: a short list that leaves names, punctuation and whatever no
// lexer claims in the terminal's own text, since a line where everything is
// colored emphasizes nothing. It has no field behind it either.
//
// Without color it is the renderer's plain style, which marks structure with
// characters.
func markdownStyle(st *styles) ansi.StyleConfig {
	if !st.color {
		style := glamourstyles.ASCIIStyleConfig
		return flushDocument(style)
	}
	str := func(s string) *string { return &s }
	yes, no := true, false

	style := glamourstyles.DarkStyleConfig
	style.Document.Color = nil

	heading := ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: str(colGold), Bold: &yes}}
	style.Heading = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{BlockSuffix: "\n", Color: str(colGold), Bold: &yes}}
	// The levels are told apart by how they sit in the text, not by a row of
	// hashes: a reader of the page never sees those.
	style.H1, style.H2, style.H3 = heading, heading, heading
	minor := ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: str(colGold), Bold: &no}}
	style.H4, style.H5, style.H6 = minor, minor, minor

	style.Code = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: str(colInfo)}}
	style.CodeBlock.Color = nil
	fg := func(c string) ansi.StylePrimitive { return ansi.StylePrimitive{Color: str(c)} }
	style.CodeBlock.Chroma = &ansi.Chroma{
		Keyword:         fg(colSynKeyword),
		KeywordType:     fg(colSynType),
		NameBuiltin:     fg(colSynType),
		NameClass:       fg(colSynType),
		NameFunction:    fg(colSynFunc),
		NameTag:         fg(colSynFunc),
		NameAttribute:   fg(colSynNumber),
		LiteralNumber:   fg(colSynNumber),
		LiteralString:   fg(colSynString),
		Comment:         fg(colSynComment),
		CommentPreproc:  fg(colSynComment),
		GenericDeleted:  fg(colSynDel),
		GenericInserted: fg(colSynAdd),
		GenericEmph:     ansi.StylePrimitive{Italic: &yes},
		GenericStrong:   ansi.StylePrimitive{Bold: &yes},
	}

	style.Link = ansi.StylePrimitive{Color: str(colInfo)}
	style.LinkText = ansi.StylePrimitive{Color: str(colInfo)}
	style.Image = ansi.StylePrimitive{Color: str(colInfo)}
	style.ImageText = ansi.StylePrimitive{Color: str(colDim), Format: "image: {{.text}}"}

	style.BlockQuote.Color = str(colGrey)
	style.BlockQuote.IndentToken = str("│ ")
	style.HorizontalRule.Color = str(colInactive)
	style.Table.Color = nil
	style.Table.CenterSeparator, style.Table.ColumnSeparator, style.Table.RowSeparator = str("┼"), str("│"), str("─")
	return flushDocument(style)
}

// flushDocument takes the margin and the blank lines the renderer puts round a
// whole document: here the comment's own header already separates it.
func flushDocument(style ansi.StyleConfig) ansi.StyleConfig {
	zero := uint(0)
	style.Document.Margin = &zero
	style.Document.BlockPrefix, style.Document.BlockSuffix = "", ""
	return style
}
