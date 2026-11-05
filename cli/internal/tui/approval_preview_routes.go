//go:build prototype

package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

type previewRouteRow struct{ method, path, options string }

// Expand host/path lists so every displayed path has its own sortable row.
// Preserve method lists in one cell and the canonical GraphQL/MCP options.
func previewRouteCells(routes []secretroute.Route) []previewRouteRow {
	var rows []previewRouteRow
	for _, route := range routes {
		methods := strings.Join(route.Methods, ",")
		if methods == "" {
			methods = "*"
		}
		optionsRoute := route
		optionsRoute.Methods = nil
		formatted := secretroute.Format(optionsRoute)
		_, options, _ := strings.Cut(formatted, " ")
		paths := route.Paths
		if len(paths) == 0 {
			paths = []string{""}
		}
		for _, host := range route.Hosts {
			for _, path := range paths {
				rows = append(rows, previewRouteRow{methods, host + path, options})
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b previewRouteRow) int {
		if order := cmp.Compare(a.path, b.path); order != 0 {
			return order
		}
		if order := cmp.Compare(a.method, b.method); order != 0 {
			return order
		}
		return cmp.Compare(a.options, b.options)
	})
	return rows
}

func previewRouteSection(label string, part *RequestPart, all []secretroute.Route) section {
	sec := section{label: label}
	if part == nil {
		sec.lines = []line{{text: "not requested", tone: toneDim}}
		return sec
	}
	methodWidth, pathWidth := len("METHOD"), len("PATH")
	for _, row := range previewRouteCells(all) {
		methodWidth = max(methodWidth, len(row.method))
		pathWidth = max(pathWidth, len(row.path))
	}
	rowText := func(method, path, options string) string {
		if options == "" {
			return fmt.Sprintf("%-*s  %s", methodWidth, method, path)
		}
		return fmt.Sprintf("%-*s  %-*s  %s", methodWidth, method, pathWidth, path, options)
	}
	sec.lines = append(sec.lines, line{text: rowText("METHOD", "PATH", "OPTIONS"), tone: toneDim})
	for _, row := range previewRouteCells(part.Routes) {
		sec.lines = append(sec.lines, line{text: rowText(row.method, row.path, row.options), tone: toneAccent})
	}
	return sec
}
