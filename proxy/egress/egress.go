// Package egress is a sandbox's egress policy (ADR 26-10-01-032): the allow
// list and deny-by-default flag each level carries, the rule that merges the
// levels into one sandbox's effective policy, and the matching the pool proxy
// enforces it with.
//
// It is shared because the control plane validates and merges what people set,
// the pool agent records the result, and the proxy enforces it, and an entry
// that one of them reads differently is either a host nobody allowed or one
// that stops working for no visible reason.
package egress

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
)

// Policy is one level's egress policy, or the effective policy of a sandbox.
//
// Allow entries are domain patterns ("github.com", "*.googleapis.com",
// "registry.*", "*"), IPs, or CIDRs: host only, never a port. A pattern
// matches as matchDomain reads it, so "github.com" is that host alone
// and "*.github.com" every name beneath it.
//
// On a level, DenyByDefault turns restriction on and Allow adds the hosts a
// restricted sandbox may reach; neither narrows another level. On an effective
// policy, DenyByDefault is whether the sandbox is restricted, and Allow is
// then all it may reach.
//
// Passthrough names the hosts whose CONNECT the pool proxy splices to the
// origin unread instead of intercepting (ADR 26-10-31-819), in Allow's
// vocabulary. It decides how an allowed tunnel is carried, never whether it is
// allowed, and applies whether or not the sandbox is restricted.
type Policy struct {
	Allow         []string `json:"allow,omitempty"`
	DenyByDefault bool     `json:"denyByDefault,omitempty"`
	Passthrough   []string `json:"passthrough,omitempty"`
}

// Effective is a sandbox's effective policy (ADR 26-10-01-032 §2): own is its
// own level and inherited its project's, pool's and harness config's. A
// sandbox a person created passes through the union of every level's hosts;
// one a discobox created inherits none of them and passes through its own
// level's alone (ADR 26-10-31-819 §2), so passthrough is never copied onto a
// box a person did not decide it for.
func Effective(own Policy, createdByDiscobox bool, inherited ...Policy) Policy {
	if createdByDiscobox {
		inherited = withoutPassthrough(inherited)
	}
	return Merge(append(inherited, own)...)
}

// Provision is the policy a sandbox's machine-scoped layers are provisioned
// under, on no sandbox's behalf: inherited levels merged, passing nothing
// through, since provisioning is no box a person decided passthrough for.
func Provision(inherited ...Policy) Policy {
	return Merge(withoutPassthrough(inherited)...)
}

func withoutPassthrough(levels []Policy) []Policy {
	out := make([]Policy, len(levels))
	for i, level := range levels {
		level.Passthrough = nil
		out[i] = level
	}
	return out
}

// Merge is the union of the levels given: restricted when any level says so,
// and then reaching the union of their hosts, and passing through the union
// of their passthrough hosts. Effective and Provision decide which levels'
// passthrough a policy takes. The unions are normalized, so two merges of the
// same levels are equal whatever order the levels or their entries came in.
func Merge(levels ...Policy) Policy {
	var merged Policy
	for _, level := range levels {
		merged.DenyByDefault = merged.DenyByDefault || level.DenyByDefault
		merged.Allow = append(merged.Allow, level.Allow...)
		merged.Passthrough = append(merged.Passthrough, level.Passthrough...)
	}
	merged.Allow = Normalize(merged.Allow)
	merged.Passthrough = Normalize(merged.Passthrough)
	return merged
}

// Normalize lowercases and trims entries, drops empty ones, and returns the
// rest sorted without duplicates; nil when nothing is left.
func Normalize(entries []string) []string {
	var out []string
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry != "" {
			out = append(out, entry)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Validate refuses an entry the proxy could not enforce.
func (p Policy) Validate() error {
	for _, entry := range p.Allow {
		if err := ValidateEntry(entry); err != nil {
			return err
		}
	}
	for _, entry := range p.Passthrough {
		if err := ValidateEntry(entry); err != nil {
			return fmt.Errorf("passthrough: %w", err)
		}
	}
	return nil
}

// ValidateEntry refuses anything that is not an IP, a CIDR, or a domain
// pattern the proxy matches: a name, "*", "*.suffix", or "prefix.*".
func ValidateEntry(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return errors.New("egress entry is empty")
	}
	if ip := net.ParseIP(entry); ip != nil {
		return refuseMappedIPv4(entry, ip)
	}
	if ip, _, err := net.ParseCIDR(entry); err == nil {
		return refuseMappedIPv4(entry, ip)
	}
	if strings.Contains(entry, "://") {
		return fmt.Errorf("egress entry %q is a URL; entries are hosts only", entry)
	}
	if strings.Contains(entry, "/") {
		return fmt.Errorf("egress entry %q is not a valid CIDR", entry)
	}
	if entry == "*" {
		return nil
	}
	name := entry
	switch {
	case strings.HasPrefix(name, "*."):
		name = name[2:]
	case strings.HasSuffix(name, ".*"):
		name = strings.TrimSuffix(name, ".*")
	}
	if strings.Contains(name, ":") {
		return fmt.Errorf("egress entry %q names a port; entries are hosts only", entry)
	}
	if name == "" || strings.Contains(name, "*") {
		return fmt.Errorf("egress entry %q: a wildcard is only \"*\", a leading \"*.\", or a trailing \".*\"", entry)
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" {
			return fmt.Errorf("egress entry %q has an empty label", entry)
		}
		if strings.IndexFunc(label, notHostNameRune) >= 0 {
			return fmt.Errorf("egress entry %q is not a host name, IP, or CIDR", entry)
		}
	}
	return nil
}

func notHostNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		return false
	}
	return true
}

// refuseMappedIPv4 refuses an IPv4 address written in IPv6's mapped form
// (::ffff:a.b.c.d). Its prefix length counts the 96 bits of the mapping, so
// ::ffff:8.0.0.0/101 is 8.0.0.0/5: compared against IPv4 entries by length
// it reads as narrower than it is, which is how a child could be judged
// within a creator it is wider than (Covers). Written as IPv4, it is compared
// as what it is.
func refuseMappedIPv4(entry string, ip net.IP) error {
	if ip.To4() != nil && strings.Contains(entry, ":") {
		return fmt.Errorf("egress entry %q is an IPv4 address in IPv6 form; write it as IPv4", entry)
	}
	return nil
}

// Matcher answers whether a destination is in an allow list.
type Matcher struct {
	domains []string
	ips     []matchedIP
	cidrs   []matchedCIDR
}

type matchedIP struct {
	entry string
	ip    net.IP
}

type matchedCIDR struct {
	entry string
	cidr  *net.IPNet
}

// NewMatcher compiles allow entries. An entry that is not valid matches
// nothing; entries are validated where they are written.
func NewMatcher(allow []string) Matcher {
	var m Matcher
	for _, entry := range Normalize(allow) {
		if ValidateEntry(entry) != nil {
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			m.ips = append(m.ips, matchedIP{entry: entry, ip: ip})
		} else if _, cidr, err := net.ParseCIDR(entry); err == nil {
			m.cidrs = append(m.cidrs, matchedCIDR{entry: entry, cidr: cidr})
		} else if !strings.Contains(entry, "/") {
			m.domains = append(m.domains, entry)
		}
	}
	return m
}

// Allows reports whether host, with or without a port, is allowed. An IP
// literal is matched against IP and CIDR entries only, never a domain.
func (m Matcher) Allows(host string) bool {
	_, ok := m.Match(host)
	return ok
}

// Match is Allows that also says which entry matched host, the first in the
// entries' normalized order.
func (m Matcher) Match(host string) (string, bool) {
	if only, _, err := net.SplitHostPort(host); err == nil {
		host = only
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if ip := net.ParseIP(host); ip != nil {
		return m.matchIP(ip)
	}
	for _, pattern := range m.domains {
		if matchDomain(pattern, host) {
			return pattern, true
		}
	}
	return "", false
}

// matchDomain checks whether host matches pattern: "*" matches every host,
// "*.suffix" every name beneath suffix, "prefix.*" every name beginning
// prefix and a dot, and anything else that host alone.
func matchDomain(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	host = strings.ToLower(host)
	if pattern == "*" || pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(host, pattern[1:])
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(host, strings.TrimSuffix(pattern, ".*")+".")
	}
	return false
}

func (m Matcher) allowsIP(ip net.IP) bool {
	_, ok := m.matchIP(ip)
	return ok
}

func (m Matcher) matchIP(ip net.IP) (string, bool) {
	for _, allowed := range m.ips {
		if allowed.ip.Equal(ip) {
			return allowed.entry, true
		}
	}
	for _, allowed := range m.cidrs {
		if allowed.cidr.Contains(ip) {
			return allowed.entry, true
		}
	}
	return "", false
}

// Covers reports whether child is no wider than outer: everything child may
// reach, outer may reach too, and everything child passes through unread,
// outer passes through too. For reach, an unrestricted outer covers anything,
// and a restricted one covers only a restricted child whose every entry one
// of outer's entries covers. Passthrough is held to outer's whether or not
// either is restricted (ADR 26-10-31-819 §5): an unrestricted sandbox reaches
// everything, but the proxy reads all of it. It is the check that keeps a
// discobox from creating, or acting on, one wider than itself (§4).
func Covers(outer, child Policy) bool {
	if !CoversEntries(outer.Passthrough, child.Passthrough) {
		return false
	}
	if !outer.DenyByDefault {
		return true
	}
	if !child.DenyByDefault {
		return false
	}
	return CoversEntries(outer.Allow, child.Allow)
}

// CoversEntries reports whether every entry of child is covered by one of
// outer's: every host the child entry matches, an outer entry matches too.
func CoversEntries(outer, child []string) bool {
	outer = Normalize(outer)
	for _, entry := range Normalize(child) {
		// An entry that is not valid matches nothing (NewMatcher), so it
		// reaches nothing the outer list could need to cover.
		if ValidateEntry(entry) != nil {
			continue
		}
		if !slices.ContainsFunc(outer, func(o string) bool { return entryCovers(o, entry) }) {
			return false
		}
	}
	return true
}

// entryCovers reports whether every destination entry matches, outer matches.
// It follows Matcher exactly: an IP or CIDR entry is covered only by an IP or
// CIDR, never by a domain pattern — "*" included, since "*" matches no IP
// literal.
func entryCovers(outer, entry string) bool {
	if outer == entry {
		return true
	}
	if ip := net.ParseIP(entry); ip != nil {
		return NewMatcher([]string{outer}).allowsIP(ip)
	}
	if _, inner, err := net.ParseCIDR(entry); err == nil {
		_, cidr, err := net.ParseCIDR(outer)
		if err != nil {
			return false
		}
		innerOnes, _ := inner.Mask.Size()
		outerOnes, _ := cidr.Mask.Size()
		return outerOnes <= innerOnes && cidr.Contains(inner.IP)
	}
	switch {
	case outer == "*":
		return true
	case strings.HasPrefix(outer, "*."):
		// Every name an exact entry, a "*." entry, or a ".*" entry beneath the
		// suffix can match ends with it — except a ".*" entry, whose matches
		// end anywhere.
		return strings.HasSuffix(entry, outer[1:]) && !strings.HasSuffix(entry, ".*")
	case strings.HasSuffix(outer, ".*"):
		prefix := strings.TrimSuffix(outer, "*")
		return strings.HasPrefix(entry, prefix) && !strings.HasPrefix(entry, "*")
	}
	return false
}
