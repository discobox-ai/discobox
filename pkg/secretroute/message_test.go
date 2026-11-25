package secretroute

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// paddedDocument is a document of about size bytes: prefix, then item
// repeated, then suffix.
func paddedDocument(size int, prefix, item, suffix string) []byte {
	var b bytes.Buffer
	b.WriteString(prefix)
	for b.Len() < size-len(item)-len(suffix) {
		b.WriteString(item)
	}
	b.WriteString(suffix)
	return b.Bytes()
}

// arrayHeavyDocument is a 1 MiB body the proxy may read for a body place: one
// sentinel string and an array of a single-digit number per two bytes, the
// shape that costs a tree reader a heap node per byte pair.
func arrayHeavyDocument() []byte {
	return paddedDocument(1<<20, `{"api_key":"sentinel","a":[0`, ",0", "]}")
}

func BenchmarkParseDocumentArrayHeavy(b *testing.B) {
	data := arrayHeavyDocument()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseDocument(data); err != nil {
			b.Fatal(err)
		}
	}
}

// parseAllocates is how many bytes one ParseDocument of data allocates, and
// its error. No test in this package runs in parallel, so the process's
// allocations between the two reads are the parse's.
func parseAllocates(data []byte) (uint64, error) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := ParseDocument(data)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, err
}

// assertParseAllocates fails t when ParseDocument of data errs, or allocates
// more than perByte times its length.
func assertParseAllocates(t *testing.T, what string, data []byte, perByte int) {
	t.Helper()
	got, err := parseAllocates(data)
	if err != nil {
		t.Fatalf("ParseDocument of %s: %v", what, err)
	}
	if limit := uint64(perByte * len(data)); got > limit {
		t.Errorf("ParseDocument of a %d-byte document of %s allocated %d bytes, want at most %d", len(data), what, got, limit)
	}
}

// TestParseDocumentAllocatesASmallMultipleOfItsInput holds the strict reader
// to the cost of what it keeps (the strings, their pointers, the top-level
// scalars), not a heap node per value: a 1 MiB array-heavy body is read in a
// small multiple of its size.
func TestParseDocumentAllocatesASmallMultipleOfItsInput(t *testing.T) {
	assertParseAllocates(t, "a sentinel and an array of numbers", arrayHeavyDocument(), 4)
}

// TestParseDocumentKeepsAPointerStepPerString holds what the reader keeps of
// each string to a record and one pointer step, not a copy of the string's
// whole pointer: a long member name, or a deep path of long names, over many
// short strings is kept once. The long-name documents are small, so that a
// reader copying each pointer (megabytes for these, gigabytes at 1 MiB) fails
// here rather than exhausting the machine running it.
func TestParseDocumentKeepsAPointerStepPerString(t *testing.T) {
	const small = 16 << 10
	deep := strings.Repeat(`{"`+strings.Repeat("d", 128)+`":`, MaxMessageDepth-1)
	for name, data := range map[string][]byte{
		"empty strings":             paddedDocument(1<<20, `[""`, `,""`, `]`),
		"objects of one member":     paddedDocument(1<<20, `[{}`, `,{"a":""}`, `]`),
		"a long name over them":     paddedDocument(small, `{"`+strings.Repeat("n", small/2)+`":[""`, `,""`, `]}`),
		"long names deep over them": paddedDocument(small, deep+`[""`, `,""`, `]`+strings.Repeat("}", MaxMessageDepth-1)),
	} {
		// A string's record and its step are 68 bytes, and the densest
		// document holds a string per three bytes.
		assertParseAllocates(t, name, data, 32)
	}
}

// TestParseDocumentKeepsNoRoomForStringsItDoesNotHold holds the reader to the
// strings a document turns out to hold, not the quotes it is written with:
// one string of escaped quotes keeps one string, and a run of quotes that is
// refused at its third byte keeps nothing.
func TestParseDocumentKeepsNoRoomForStringsItDoesNotHold(t *testing.T) {
	assertParseAllocates(t, "one string of escaped quotes", paddedDocument(1<<20, `"`, `\"`, `"`), 4)
	quotes := bytes.Repeat([]byte{'"'}, 1<<20)
	got, err := parseAllocates(quotes)
	if !errors.Is(err, ErrNotMessage) {
		t.Fatalf("ParseDocument of a run of quotes = %v, want ErrNotMessage", err)
	}
	if got > 4<<10 {
		t.Errorf("ParseDocument of a %d-byte run of quotes, refused, allocated %d bytes, want at most %d", len(quotes), got, 4<<10)
	}
}

// TestParseDocumentPointers pins every string's pointer, name flag and bytes
// where pointer steps are shared and reset: siblings after a sibling that held
// a string, an element holding none between two that do, a member renamed
// within its object, empty names, containers holding no string, a root string
// and array, and escaped names.
func TestParseDocumentPointers(t *testing.T) {
	type str struct {
		at, value string
		name      bool
		raw       string
	}
	for _, tc := range []struct {
		doc  string
		want []str
	}{
		{
			doc: `{"a":[{"b":"x"},1,{"c~/d":["y",{}],"e":"z"}],"":{"":""},"f":[[],[["w"]]]}`,
			want: []str{
				{"/a", "a", true, `"a"`},
				{"/a/0/b", "b", true, `"b"`},
				{"/a/0/b", "x", false, `"x"`},
				{"/a/2/c~0~1d", "c~/d", true, `"c~/d"`},
				{"/a/2/c~0~1d/0", "y", false, `"y"`},
				{"/a/2/e", "e", true, `"e"`},
				{"/a/2/e", "z", false, `"z"`},
				{"/", "", true, `""`},
				{"//", "", true, `""`},
				{"//", "", false, `""`},
				{"/f", "f", true, `"f"`},
				{"/f/1/0/0", "w", false, `"w"`},
			},
		},
		{
			doc:  ` "sé" `,
			want: []str{{"", "sé", false, `"sé"`}},
		},
		{
			doc: `[1,{"q":[true,null,"r"]},"p",[{}],{"t\/u":"v"}]`,
			want: []str{
				{"/1/q", "q", true, `"q"`},
				{"/1/q/2", "r", false, `"r"`},
				{"/2", "p", false, `"p"`},
				{"/4/t~1u", "t/u", true, `"t\/u"`},
				{"/4/t~1u", "v", false, `"v"`},
			},
		},
	} {
		m, err := ParseDocument([]byte(tc.doc))
		if err != nil {
			t.Fatalf("ParseDocument(%s): %v", tc.doc, err)
		}
		var got []str
		for _, s := range m.Strings() {
			got = append(got, str{s.At(), s.Value, s.Name, tc.doc[s.Start:s.End]})
		}
		if len(got) != len(tc.want) {
			t.Fatalf("ParseDocument(%s) strings = %+v, want %+v", tc.doc, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ParseDocument(%s) string %d = %+v, want %+v", tc.doc, i, got[i], tc.want[i])
			}
		}
	}
}
