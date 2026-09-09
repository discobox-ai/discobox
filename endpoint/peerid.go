package endpoint

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// A peer ID is how an [IrohID] is written down: `d1-` and lowercase Crockford
// base32 with check symbols, in groups of seven (ADR 0097 §1).
//
//	d1-dtztd73-fe72z95-54a1b3e-nfj0xh3-dw4rebf-6ep2hhn-ebaqm88-eewbp0v
//
// It is the only textual form of a peer identity: the address after
// `discobox://`, the CLI, `authorized_ids`, the API, and the database all use
// this and reject anything else (ADR 0097 §§5-6).
const (
	// PeerIDVersion says how to read what follows. A later "d2" can carry a
	// different encoding or a different transport without invalidating an
	// address somebody already wrote down, which is the whole reason the
	// prefix is here.
	//
	// It is exported because every peer ID starts with it, which makes it the
	// one prefix that identifies nothing: anything matching a peer ID by
	// prefix has to require more than this.
	PeerIDVersion = "d1"

	// CrockfordAlphabet omits I, L, O and U: the first three because they are
	// confusable with 1 and 0, the last because it turns typos into words.
	// Decoding maps the confusable ones back, so a human who types "I" for "1"
	// still names the right peer.
	CrockfordAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

	// peerIDBodyLen is 52 symbols for 256 bits, plus one check symbol for each
	// of the four 13-symbol groups.
	peerIDDataLen  = 52
	peerIDGroupLen = 13
	peerIDBodyLen  = 56

	// peerIDDisplayGroup is how the body is grouped for reading. The dashes
	// are not part of the value: Crockford ignores them, so a peer ID survives
	// being pasted with them, without them, or wrapped by a terminal.
	peerIDDisplayGroup = 7
)

// String renders the peer ID: the form a server prints, a CLI shows, an
// operator writes in authorized_ids, and the API returns.
func (id IrohID) String() string {
	body := encodePeerIDBody(id[:])
	var out strings.Builder
	out.WriteString(PeerIDVersion)
	for i := 0; i < len(body); i += peerIDDisplayGroup {
		out.WriteByte('-')
		out.WriteString(body[i:min(i+peerIDDisplayGroup, len(body))])
	}
	return out.String()
}

// Key is the form stored in the database and compared against.
//
// It is the same value with the dashes removed, so a prefix match is a prefix
// of one stable string rather than of whatever hyphenation somebody pasted
// (ADR 0097 §5).
func (id IrohID) Key() string {
	return PeerIDVersion + encodePeerIDBody(id[:])
}

// IrohEndpointID renders this identity the way iroh itself writes one: 64
// lowercase hex characters, which is what iroh's own tracing, tickets and
// tooling show. iroh abbreviates it to the first five bytes in a log line —
// `endpoint{id=4afa25be01}` — so a reader with both logs open has two
// identifiers for one machine and nothing to line them up with. This is that
// something (ADR 0098 §5).
//
// Output only. Nothing here reads it back: [ParseIrohID] rejects hex, and so
// do authorized_ids and the API, because a peer ID has exactly one written
// form (ADR 0097 §6).
func (id IrohID) IrohEndpointID() string {
	return hex.EncodeToString(id[:])
}

// Short renders the version and the first group, for a log line where the
// whole ID is noise. It is never a valid peer ID: [ParseIrohID] rejects it, so
// a short form cannot be pasted somewhere that expects the real thing.
func (id IrohID) Short() string {
	return PeerIDVersion + "-" + encodePeerIDBody(id[:])[:peerIDDisplayGroup]
}

// ParseIrohID decodes a peer ID.
//
// Dashes and case are ignored, because they are how the value is made readable
// rather than part of it. Hex is not accepted: there is one form and it is
// enforced (ADR 0097 §6).
func ParseIrohID(value string) (IrohID, error) {
	compact := NormalizePeerID(value)
	if compact == "" {
		return IrohID{}, fmt.Errorf("peer ID is required")
	}
	body, ok := strings.CutPrefix(compact, PeerIDVersion)
	if !ok {
		return IrohID{}, fmt.Errorf("peer ID %q does not start with %q; a peer ID looks like %s",
			value, PeerIDVersion, ExamplePeerID)
	}
	if len(body) != peerIDBodyLen {
		return IrohID{}, fmt.Errorf("peer ID %q is %d symbols after %q, want %d; a peer ID looks like %s",
			value, len(body), PeerIDVersion, peerIDBodyLen, ExamplePeerID)
	}
	data := make([]byte, 0, peerIDDataLen)
	for group := 0; group < peerIDBodyLen; group += peerIDGroupLen + 1 {
		symbols := body[group : group+peerIDGroupLen]
		check := body[group+peerIDGroupLen]
		for i := range symbols {
			if crockfordValue(symbols[i]) < 0 {
				return IrohID{}, fmt.Errorf("peer ID %q contains %q, which is not one of %s",
					value, string(symbols[i]), CrockfordAlphabet)
			}
		}
		if crockfordValue(check) < 0 {
			return IrohID{}, fmt.Errorf("peer ID %q contains %q, which is not one of %s",
				value, string(check), CrockfordAlphabet)
		}
		// The check symbol is what turns a mistyped peer ID into an error here
		// rather than into a connection that never completes.
		if want := luhnCheck(symbols); check != want {
			return IrohID{}, fmt.Errorf("peer ID %q has a bad check symbol in group %d: it is not a valid peer ID, most likely mistyped",
				value, group/(peerIDGroupLen+1)+1)
		}
		data = append(data, symbols...)
	}
	return decodePeerIDBody(string(data), value)
}

// ExamplePeerID is what an error shows so a reader can see the shape they are
// missing rather than infer it from a symbol count.
const ExamplePeerID = "d1-dtztd73-fe72z95-54a1b3e-nfj0xh3-dw4rebf-6ep2hhn-ebaqm88-eewbp0v"

// encodePeerIDBody renders 32 bytes as 52 Crockford symbols with a check
// symbol after each group of 13.
func encodePeerIDBody(raw []byte) string {
	symbols := make([]byte, 0, peerIDDataLen)
	bits := len(raw) * 8
	for offset := 0; offset < bits; offset += 5 {
		symbols = append(symbols, CrockfordAlphabet[bitsAt(raw, offset)])
	}
	out := make([]byte, 0, peerIDBodyLen)
	for i := 0; i < len(symbols); i += peerIDGroupLen {
		group := symbols[i : i+peerIDGroupLen]
		out = append(out, group...)
		out = append(out, luhnCheck(string(group)))
	}
	return string(out)
}

// decodePeerIDBody turns 52 Crockford symbols back into the 32 bytes they
// encode.
func decodePeerIDBody(symbols, original string) (IrohID, error) {
	var id IrohID
	for i := 0; i < len(symbols); i++ {
		value := crockfordValue(symbols[i])
		offset := i * 5
		for bit := range 5 {
			at := offset + bit
			if at >= IrohIDSize*8 {
				// The final symbol carries four padding bits, which must be
				// zero: a peer ID with anything there did not come from an
				// endpoint key.
				if value&(1<<(4-bit)) != 0 {
					return IrohID{}, fmt.Errorf("peer ID %q has trailing bits set and names no endpoint", original)
				}
				continue
			}
			if value&(1<<(4-bit)) != 0 {
				id[at/8] |= 1 << (7 - at%8)
			}
		}
	}
	return id, nil
}

// bitsAt reads the five bits starting at offset, padding past the end with
// zeros.
func bitsAt(raw []byte, offset int) byte {
	var out byte
	for bit := range 5 {
		at := offset + bit
		if at >= len(raw)*8 {
			break
		}
		if raw[at/8]&(1<<(7-at%8)) != 0 {
			out |= 1 << (4 - bit)
		}
	}
	return out
}

// NormalizePeerID puts a peer ID, or a prefix of one, into the single form
// everything stores and compares: lowercase, no dashes, and Crockford's
// confusables folded — i and l are 1, o is 0.
//
// It is exported because it is not only [ParseIrohID]'s business. Anything
// that looks a peer ID up by prefix has to reach the same string a full ID
// would, or the two ways of naming one peer diverge on exactly the input the
// alphabet exists to absorb: enrolling `d1-dtztdl3-…` would work while
// revoking the same string would find nothing.
//
// The folding covers the "d1" version prefix too, because somebody who writes
// "dl" for "d1" has made the same mistake, and the prefix is no less typed
// than the rest.
func NormalizePeerID(value string) string {
	folded := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
	return strings.NewReplacer("i", "1", "l", "1", "o", "0").Replace(folded)
}

// crockfordValue decodes one symbol. Confusables are folded before this runs,
// but it maps them too so a caller cannot get a different answer by skipping
// that step.
func crockfordValue(symbol byte) int {
	switch symbol {
	case 'i', 'l':
		return 1
	case 'o':
		return 0
	}
	if index := strings.IndexByte(CrockfordAlphabet, symbol); index >= 0 {
		return index
	}
	return -1
}

// luhnCheck is the Luhn mod-N check symbol over the Crockford alphabet, the
// scheme Syncthing uses on device IDs and for the same reason: a mistyped or
// transposed character is caught where it was typed rather than becoming a
// connection that never completes.
//
// It catches every single-symbol error and every adjacent transposition except
// one: swapping "0" with "z" leaves the check symbol unchanged. That is the
// mod-32 analog of mod-10 Luhn's blind spot on 0 and 9, it is inherent to the
// algorithm rather than to this use of it, and it is recorded here so nobody
// has to rediscover it from a test.
func luhnCheck(symbols string) byte {
	// The factor starts at 2 because the check symbol itself occupies the
	// rightmost position, where the factor is 1. Starting at 1 here — the
	// orientation used to *validate* a string that already carries its check
	// symbol — leaves the last data symbol and the check symbol
	// interchangeable, which is the single most likely transposition of the
	// lot and was undetected in 97% of cases before this was corrected.
	factor, sum, n := 2, 0, len(CrockfordAlphabet)
	for i := len(symbols) - 1; i >= 0; i-- {
		addend := crockfordValue(symbols[i]) * factor
		if factor == 2 {
			factor = 1
		} else {
			factor = 2
		}
		sum += addend/n + addend%n
	}
	return CrockfordAlphabet[(n-sum%n)%n]
}
