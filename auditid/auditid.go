// Package auditid owns how an audit record's ID is written on an API.
//
// Three of the four audit trails (ADR 0130) number their records with an
// `x/id` value, which carries its own prefix and is unique everywhere. The
// pool proxy's trail does not: its ID is the audit database's row number,
// because that number is the write order, which is what a follower reads along
// (ADR 0130 §5). A bare integer beside `cvd_…` and `evt_…` reads as a different
// kind of thing and cannot be routed by a command given one ID, so the proxy's
// trail writes its row number as `http_<number>` on every API above the
// database that holds it.
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
func (id ExchangeID) String() string {
	if id == 0 {
		return ""
	}
	return ExchangePrefix + strconv.FormatUint(uint64(id), 10)
}

func (id ExchangeID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.String())
}

func (id *ExchangeID) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("audit exchange ID: want a string such as %s12, got %s", ExchangePrefix, data)
	}
	if text == "" {
		*id = 0
		return nil
	}
	parsed, err := ParseExchange(text)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// ParseExchange reads the form String writes. The prefix is required: a bare
// number is the database's own spelling and is not what an API carries.
func ParseExchange(value string) (ExchangeID, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(value), ExchangePrefix)
	if !ok || rest == "" {
		return 0, fmt.Errorf("audit exchange ID %q: want %s followed by the row number", value, ExchangePrefix)
	}
	number, err := strconv.ParseUint(rest, 10, 64)
	if err != nil || number == 0 {
		return 0, fmt.Errorf("audit exchange ID %q: %q is not a row number", value, rest)
	}
	return ExchangeID(number), nil
}

// IsExchange reports whether value is spelled as an audited exchange's ID. It
// is how a command handed one ID decides which trail to read, alongside the
// prefixes `x/id` writes for the other trails.
func IsExchange(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), ExchangePrefix)
}
