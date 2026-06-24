package rules

import (
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// HeaderRule is the internal deterministic rewrite rule shape.
type HeaderRule struct {
	ID          string
	Pattern     string
	Methods     []string
	PathRegexes []string
	ClientIDs   []string
	Conditions  []HeaderCondition
	Set         map[string]string
	Append      map[string]string
}

type compiledHeaderRule struct {
	HeaderRule
	methods      map[string]struct{}
	pathPatterns []*regexp.Regexp
	clientIDs    map[string]struct{}
}

// HeaderCondition requires an incoming request header value to match exactly.
type HeaderCondition struct {
	Header string
	Equals string
}

// MatchResult describes a deterministic header rewrite match.
type MatchResult struct {
	Matched bool
	RuleID  string
	Pattern string
	Host    string
	Headers []string
}

// Rewriter applies deterministic header rules.
type Rewriter struct {
	rules []compiledHeaderRule
}

// NewRewriter creates a deterministic rewriter.
func NewRewriter(headerRules []HeaderRule) *Rewriter {
	rules := make([]compiledHeaderRule, 0, len(headerRules))
	for _, rule := range headerRules {
		methods := normalizeMethods(rule.Methods)
		clientIDs := normalizeStrings(rule.ClientIDs)
		compiled := compiledHeaderRule{
			HeaderRule: HeaderRule{
				ID:          rule.ID,
				Pattern:     strings.ToLower(rule.Pattern),
				Methods:     methods,
				PathRegexes: slices.Clone(rule.PathRegexes),
				ClientIDs:   clientIDs,
				Conditions:  slices.Clone(rule.Conditions),
				Set:         copyMap(rule.Set),
				Append:      copyMap(rule.Append),
			},
			methods:   setFromStrings(methods),
			clientIDs: setFromStrings(clientIDs),
		}
		for _, pattern := range rule.PathRegexes {
			re, err := regexp.Compile(pattern)
			if err != nil {
				continue
			}
			compiled.pathPatterns = append(compiled.pathPatterns, re)
		}
		rules = append(rules, compiled)
	}
	slices.SortFunc(rules, compareRules)
	return &Rewriter{rules: rules}
}

// Apply mutates req when a matching rule applies.
func (r *Rewriter) Apply(req *http.Request, clientID string) MatchResult {
	host := extractHost(req.Host)
	for _, rule := range r.rules {
		if !MatchDomain(rule.Pattern, host) ||
			!methodMatches(req.Method, rule.methods) ||
			!pathMatches(req.URL.Path, rule.pathPatterns) ||
			!clientMatches(clientID, rule.clientIDs) ||
			!conditionsMatch(req, rule.Conditions) {
			continue
		}
		var headers []string
		for key, value := range rule.Set {
			req.Header.Set(key, value)
			headers = append(headers, http.CanonicalHeaderKey(key))
		}
		for key, value := range rule.Append {
			req.Header.Add(key, value)
			headers = append(headers, http.CanonicalHeaderKey(key))
		}
		slices.Sort(headers)
		return MatchResult{
			Matched: true,
			RuleID:  rule.ID,
			Pattern: rule.Pattern,
			Host:    host,
			Headers: headers,
		}
	}
	return MatchResult{Host: host}
}

// MatchDomain checks whether host matches pattern.
func MatchDomain(pattern, host string) bool {
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

func compareRules(a, b compiledHeaderRule) int {
	as, bs := specificity(a.Pattern)+constraintSpecificity(a), specificity(b.Pattern)+constraintSpecificity(b)
	if as != bs {
		return bs - as
	}
	return strings.Compare(a.Pattern, b.Pattern)
}

func specificity(pattern string) int {
	switch {
	case pattern == "*":
		return 0
	case strings.HasPrefix(pattern, "*."):
		return 10 + len(pattern)
	case strings.HasSuffix(pattern, ".*"):
		return 20 + len(pattern)
	default:
		return 100 + len(pattern)
	}
}

func conditionsMatch(req *http.Request, conditions []HeaderCondition) bool {
	for _, condition := range conditions {
		if req.Header.Get(condition.Header) != condition.Equals {
			return false
		}
	}
	return true
}

func methodMatches(method string, methods map[string]struct{}) bool {
	if len(methods) == 0 {
		return true
	}
	_, ok := methods[strings.ToUpper(method)]
	return ok
}

func pathMatches(path string, patterns []*regexp.Regexp) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if pattern.MatchString(path) {
			return true
		}
	}
	return false
}

func clientMatches(clientID string, clientIDs map[string]struct{}) bool {
	if len(clientIDs) == 0 {
		return true
	}
	_, ok := clientIDs[clientID]
	return ok
}

func constraintSpecificity(rule compiledHeaderRule) int {
	return len(rule.methods)*3 + len(rule.pathPatterns)*5 + len(rule.clientIDs)*7 + len(rule.Conditions)*2
}

func normalizeMethods(methods []string) []string {
	out := make([]string, 0, len(methods))
	for _, method := range methods {
		out = append(out, strings.ToUpper(strings.TrimSpace(method)))
	}
	return out
}

func normalizeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

func setFromStrings(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func extractHost(hostPort string) string {
	host, _, err := net.SplitHostPort(hostPort)
	if err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(hostPort)
}

func copyMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	maps.Copy(out, values)
	return out
}
