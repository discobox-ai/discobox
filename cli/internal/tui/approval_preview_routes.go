//go:build prototype

package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

type previewRouteRow struct {
	protocol                             int
	host, method, path, operation, names string
	blocked                              bool
}

func previewRouteCells(routes []secretroute.Route) []previewRouteRow {
	var rows []previewRouteRow
	for _, route := range routes {
		base := previewRouteRow{method: strings.Join(route.Methods, ","), blocked: route.Blocked}
		if base.method == "" {
			base.method = "*"
		}
		operations := []string{""}
		switch {
		case route.GraphQL != nil:
			base.protocol = 1
			operations = route.GraphQL.Operations
			if len(operations) == 0 {
				operations = []string{"*"}
			}
			base.names = strings.Join(route.GraphQL.Fields, ",")
			if base.names == "" {
				base.names = "*"
			}
		case route.MCP != nil:
			base.protocol = 2
			base.names = strings.Join(route.MCP.Tools, ",")
			if base.names == "" {
				base.names = "*"
			}
		}
		paths := route.Paths
		if len(paths) == 0 {
			paths = []string{"*"}
		}
		for _, host := range route.Hosts {
			for _, path := range paths {
				for _, op := range operations {
					row := base
					row.host = host
					row.path = path
					row.operation = op
					rows = append(rows, row)
				}
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b previewRouteRow) int {
		if c := cmp.Compare(a.protocol, b.protocol); c != 0 {
			return c
		}
		for _, pair := range [][2]string{{a.host, b.host}, {a.path, b.path}, {a.method, b.method}, {a.operation, b.operation}, {a.names, b.names}} {
			if c := cmp.Compare(pair[0], pair[1]); c != 0 {
				return c
			}
		}
		return cmp.Compare(fmt.Sprint(a.blocked), fmt.Sprint(b.blocked))
	})
	return slices.Compact(rows)
}

// Keep use/delegation boundaries, then protocol order and host groups inside
// each. GraphQL/MCP methods are shown only when at least one is restricted.
func previewRouteSection(label string, part *RequestPart) section {
	sec := section{label: label}
	if part == nil {
		sec.lines = []line{{text: "not requested", tone: toneDim}}
		return sec
	}
	rows := previewRouteCells(part.Routes)
	protocols := []string{"HTTP", "GraphQL", "MCP"}
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].protocol == rows[start].protocol && rows[end].host == rows[start].host {
			end++
		}
		group := rows[start:end]
		protocol := group[0].protocol
		if len(sec.lines) > 0 {
			sec.lines = append(sec.lines, line{})
		}
		sec.lines = append(sec.lines, line{text: protocols[protocol] + " " + group[0].host, tone: toneAccent})

		showMethod := protocol == 0 || slices.ContainsFunc(group, func(r previewRouteRow) bool { return r.method != "*" })
		showEffect := slices.ContainsFunc(group, func(r previewRouteRow) bool { return r.blocked })
		headers := []string{}
		if showEffect {
			headers = append(headers, "ACCESS")
		}
		if showMethod {
			headers = append(headers, "METHOD")
		}
		headers = append(headers, "PATH")
		switch protocol {
		case 1:
			headers = append(headers, "OPERATION", "FIELDS")
		case 2:
			headers = append(headers, "TOOLS")
		}
		table := [][]string{headers}
		for _, row := range group {
			cells := []string{}
			if showEffect {
				effect := "ALLOW"
				if row.blocked {
					effect = "DENY"
				}
				cells = append(cells, effect)
			}
			if showMethod {
				cells = append(cells, row.method)
			}
			cells = append(cells, row.path)
			switch protocol {
			case 1:
				cells = append(cells, row.operation, row.names)
			case 2:
				cells = append(cells, row.names)
			}
			table = append(table, cells)
		}
		widths := make([]int, len(headers))
		for _, cells := range table {
			for i, cell := range cells {
				widths[i] = max(widths[i], len(cell))
			}
		}
		for i, cells := range table {
			var text strings.Builder
			text.WriteString("  ")
			for col, cell := range cells {
				if col == len(cells)-1 {
					text.WriteString(cell)
				} else {
					fmt.Fprintf(&text, "%-*s  ", widths[col], cell)
				}
			}
			tone := toneAccent
			if i == 0 {
				tone = toneDim
			} else if group[i-1].blocked {
				tone = toneAlert
			}
			sec.lines = append(sec.lines, line{text: text.String(), tone: tone})
		}
		start = end
	}
	return sec
}

// This is the binding's own restriction, not a flattening of the credential
// or parent ceilings. Hosts scope intentionally drops request-level blocks.
func previewGrantSection(scope string, part *RequestPart, hosts []string) section {
	if scope == "routes" {
		return previewRouteSection("Grant restriction · Requested routes", part)
	}
	sec := section{label: "Grant restriction · Requested hosts"}
	if scope == "anywhere" {
		sec.label = "Grant restriction · Anywhere"
		sec.lines = []line{{text: "Any protocol · any host", tone: toneAccent}, {text: "  METHOD  PATH", tone: toneDim}, {text: "  *       *", tone: toneAccent}, {}, {text: "No grant restriction. Still limited by credential scope and any parent delegation.", tone: toneDim}}
		return sec
	}
	for _, host := range hosts {
		if len(sec.lines) > 0 {
			sec.lines = append(sec.lines, line{})
		}
		sec.lines = append(sec.lines, line{text: "Any protocol · " + host, tone: toneAccent}, line{text: "  METHOD  PATH", tone: toneDim}, line{text: "  *       *", tone: toneAccent})
	}
	if len(hosts) == 0 {
		sec.lines = append(sec.lines, line{text: "No open requested hosts. This scope cannot be accepted.", tone: toneAlert})
	}
	sec.lines = append(sec.lines, line{}, line{text: "Any method, path and protocol on these hosts. Request-level blocks are dropped.", tone: toneDim})
	return sec
}
