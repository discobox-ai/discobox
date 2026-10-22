package auditid

import "testing"

func TestExchangeIDRoundTrips(t *testing.T) {
	if got := ExchangeID(10219).String(); got != "http_10219" {
		t.Fatalf("String() = %q", got)
	}
	if got := ExchangeID(0).String(); got != "" {
		t.Fatalf("zero String() = %q, want empty", got)
	}
	id, err := ParseExchange("http_10219")
	if err != nil || id != 10219 {
		t.Fatalf("ParseExchange() = %d, %v", id, err)
	}
	for _, bad := range []string{"10219", "http_", "http_0", "http_x", "", "cvd_abc", "http_-1"} {
		if _, err := ParseExchange(bad); err == nil {
			t.Fatalf("ParseExchange(%q) was accepted", bad)
		}
	}
	if !IsExchange("http_1") || IsExchange("cvd_1") || IsExchange("evt_1") {
		t.Fatal("IsExchange does not separate the trails")
	}
}

func TestExchangeIDJSONIsTheAPISpelling(t *testing.T) {
	data, err := ExchangeID(7).MarshalJSON()
	if err != nil || string(data) != `"http_7"` {
		t.Fatalf("MarshalJSON() = %s, %v", data, err)
	}
	var id ExchangeID
	if err := id.UnmarshalJSON([]byte(`"http_7"`)); err != nil || id != 7 {
		t.Fatalf("UnmarshalJSON() = %d, %v", id, err)
	}
	// A bare number is the database's spelling, not the API's.
	if err := id.UnmarshalJSON([]byte(`7`)); err == nil {
		t.Fatal("a bare number was accepted as an exchange ID")
	}
}

// A DNS query's ID is spelled like an exchange's with its own prefix, and
// neither trail accepts the other's: a cursor handed to the wrong trail would
// otherwise read an unrelated table from an unrelated row.
func TestDNSQueryIDIsItsOwnTrail(t *testing.T) {
	if got := DNSQueryID(42).String(); got != "dns_42" {
		t.Fatalf("String() = %q", got)
	}
	id, err := ParseDNSQuery("dns_42")
	if err != nil || id != 42 {
		t.Fatalf("ParseDNSQuery() = %d, %v", id, err)
	}
	if _, err := ParseDNSQuery("http_42"); err == nil {
		t.Fatal("an exchange ID was accepted as a DNS query ID")
	}
	if _, err := ParseExchange("dns_42"); err == nil {
		t.Fatal("a DNS query ID was accepted as an exchange ID")
	}
	if !IsDNSQuery("dns_1") || IsDNSQuery("http_1") || IsExchange("dns_1") {
		t.Fatal("IsDNSQuery does not separate the trails")
	}
	data, err := DNSQueryID(7).MarshalJSON()
	if err != nil || string(data) != `"dns_7"` {
		t.Fatalf("MarshalJSON() = %s, %v", data, err)
	}
	var decoded DNSQueryID
	if err := decoded.UnmarshalJSON([]byte(`"dns_7"`)); err != nil || decoded != 7 {
		t.Fatalf("UnmarshalJSON() = %d, %v", decoded, err)
	}
}
