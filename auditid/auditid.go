// Package auditid owns how an audit record's ID is written on an API.
//
// Most audit trails (ADR 0130) number their records with an `x/id` value,
// which carries its own prefix and is unique everywhere. The pool proxy's two
// trails do not: their IDs are the audit database's row numbers, because that
// number is the write order, which is what a follower reads along (ADR 0130
// §5). A bare integer beside `cvd_…` and `evt_…` reads as a different kind of
// thing and cannot be routed by a command given one ID, so each proxy trail
// writes its row number with its own prefix on every API above the database
// that holds it: `http_<number>` for an exchange, `dns_<number>` for a DNS
// query (ADR 0148).
//
// The package is deliberately tiny and dependency-free: the component that owns
// the row, the two that relay it, and the CLI that prints it all have to agree
// on this spelling, and the CLI cannot import the proxy to learn it.
package auditid

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ExchangePrefix marks an ID as an audited HTTP exchange's.
const ExchangePrefix = "http_"

// ExchangeID is an audited HTTP exchange's ID: the pool-local row number of the
// proxy's audit database, written as http_<number>.
//
// It is an integer in Go and in the database, where its order is the write
// order a cursor reads along, and a string on every API: MarshalJSON writes the
// prefixed form and UnmarshalJSON accepts only that form, so a caller cannot
// send a bare number and have it mean something else later.
type ExchangeID uint64

// String is the ID as an API writes it. The zero ID is written as the empty
// string: no row has it, and "http_0" would read as one that does.
func (id ExchangeID) String() string { return format(ExchangePrefix, uint64(id)) }

func (id ExchangeID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.String())
}

func (id *ExchangeID) UnmarshalJSON(data []byte) error {
	number, err := unmarshal(data, ExchangePrefix, "audit exchange ID")
	*id = ExchangeID(number)
	return err
}

// ParseExchange reads the form String writes. The prefix is required: a bare
// number is the database's own spelling and is not what an API carries.
func ParseExchange(value string) (ExchangeID, error) {
	number, err := parse(value, ExchangePrefix, "audit exchange ID")
	return ExchangeID(number), err
}

// IsExchange reports whether value is spelled as an audited exchange's ID. It
// is how a command handed one ID decides which trail to read, alongside the
// prefixes `x/id` writes for the other trails.
func IsExchange(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), ExchangePrefix)
}

// DNSPrefix marks an ID as an audited DNS query's.
const DNSPrefix = "dns_"

// DNSQueryID is an audited DNS query's ID: the pool-local row number of the
// proxy's audit database, written as dns_<number>. It is spelled and parsed
// exactly as ExchangeID is, with its own prefix, so neither trail's cursor can
// be read as the other's.
type DNSQueryID uint64

// String is the ID as an API writes it; the zero ID is the empty string.
func (id DNSQueryID) String() string { return format(DNSPrefix, uint64(id)) }

func (id DNSQueryID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.String())
}

func (id *DNSQueryID) UnmarshalJSON(data []byte) error {
	number, err := unmarshal(data, DNSPrefix, "audit DNS query ID")
	*id = DNSQueryID(number)
	return err
}

// ParseDNSQuery reads the form DNSQueryID.String writes; the prefix is
// required.
func ParseDNSQuery(value string) (DNSQueryID, error) {
	number, err := parse(value, DNSPrefix, "audit DNS query ID")
	return DNSQueryID(number), err
}

// IsDNSQuery reports whether value is spelled as an audited DNS query's ID.
func IsDNSQuery(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), DNSPrefix)
}

func format(prefix string, number uint64) string {
	if number == 0 {
		return ""
	}
	return prefix + strconv.FormatUint(number, 10)
}

func unmarshal(data []byte, prefix, kind string) (uint64, error) {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return 0, fmt.Errorf("%s: want a string such as %s12, got %s", kind, prefix, data)
	}
	if text == "" {
		return 0, nil
	}
	return parse(text, prefix, kind)
}

func parse(value, prefix, kind string) (uint64, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(value), prefix)
	if !ok || rest == "" {
		return 0, fmt.Errorf("%s %q: want %s followed by the row number", kind, value, prefix)
	}
	number, err := strconv.ParseUint(rest, 10, 64)
	if err != nil || number == 0 {
		return 0, fmt.Errorf("%s %q: %q is not a row number", kind, value, rest)
	}
	return number, nil
}
