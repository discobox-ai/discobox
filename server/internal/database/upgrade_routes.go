package database

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// legacyRoute is a route as it was stored before routes named host patterns
// (ADR 26-10-27-880 §8): a host covering itself and every host beneath it,
// or `*`, and a path prefix on whole segments. Only the upgrade reads it: the
// grant retirement, which runs over a database of that era, and
// upgradeRouteShapes, which converts what is left of it.
type legacyRoute struct {
	Host    string `json:"host"`
	Path    string `json:"path,omitempty"`
	Blocked bool   `json:"blocked,omitempty"`
}

// decodeLegacyRoutes reads a stored route list of the legacy shape. A route
// already in the new shape is an error, never read as one naming no host: a
// step that reads legacy routes runs before upgradeRouteShapes converts them.
func decodeLegacyRoutes(text string) ([]legacyRoute, error) {
	if text = strings.TrimSpace(text); text == "" || text == "null" {
		return nil, nil
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, err
	}
	for _, route := range raw {
		if _, ok := route["hosts"]; ok {
			return nil, fmt.Errorf("route list %s is already in the shape routes have now", text)
		}
	}
	var routes []legacyRoute
	if err := json.Unmarshal([]byte(text), &routes); err != nil {
		return nil, err
	}
	return routes, nil
}

// normalizeLegacyRoute is the reading every legacy route was written through
// (secretroute.Normalize as it was): the host normalized as hostscope does,
// not empty, and a host name or `*`; the path from "/", without a trailing
// "/", and "/" alone no path.
func normalizeLegacyRoute(route legacyRoute) (legacyRoute, error) {
	host := hostscope.Normalize(route.Host)
	switch {
	case host == "":
		return legacyRoute{}, fmt.Errorf("a route must name a host")
	case host != secretroute.AnyHost && (strings.ContainsAny(host, "/?#@:= \t*") || hasControlRune(host)):
		return legacyRoute{}, fmt.Errorf("route host %q is not a host name", host)
	}
	routePath := strings.TrimSpace(route.Path)
	if routePath != "" {
		if !strings.HasPrefix(routePath, "/") {
			return legacyRoute{}, fmt.Errorf("route path %q must start with /", routePath)
		}
		if strings.ContainsAny(routePath, "?# \t") || hasControlRune(routePath) {
			return legacyRoute{}, fmt.Errorf("route path %q must be a plain path", routePath)
		}
		routePath = strings.TrimRight(routePath, "/")
		if strings.Contains(routePath, "//") {
			return legacyRoute{}, fmt.Errorf("route path %q has an empty segment", routePath)
		}
		for _, segment := range strings.Split(routePath, "/") {
			if segment == "." || segment == ".." {
				return legacyRoute{}, fmt.Errorf("route path %q has a %q segment", routePath, segment)
			}
		}
	}
	return legacyRoute{Host: host, Path: routePath, Blocked: route.Blocked}, nil
}

func hasControlRune(text string) bool {
	return strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) })
}

// legacyPathCovers reports whether a legacy route's path prefix covers a
// request path, on whole segments, ignoring case, as legacy routes were
// matched. An empty prefix covers every path.
func legacyPathCovers(prefix, requestPath string) bool {
	if prefix == "" {
		return true
	}
	if len(requestPath) < len(prefix) || !strings.EqualFold(requestPath[:len(prefix)], prefix) {
		return false
	}
	return len(requestPath) == len(prefix) || requestPath[len(prefix)] == '/'
}

// upgradeLegacyRoutes is a route list in the shape routes have now: its
// legacy routes read by the one rule the pool reads its own old routes by
// (secretroute.FromLegacyList), beside the routes current already holds in
// the new shape. It widens nothing: a list it cannot convert, or one it
// empties, refuses every request rather than reading as no routes. Each route
// not converted exactly is logged with what the list belongs to.
func upgradeLegacyRoutes(what string, routes []legacyRoute, current []model.SecretRoute) []model.SecretRoute {
	legacy := make([]secretroute.LegacyRoute, 0, len(routes))
	for _, route := range routes {
		legacy = append(legacy, secretroute.LegacyRoute{Host: route.Host, Path: route.Path, Blocked: route.Blocked})
	}
	converted, notes := secretroute.FromLegacyList(legacy, current)
	for _, note := range notes {
		log.Printf("the routes of %s: %s (ADR 26-10-27-880 §8)", what, note)
	}
	return converted
}

// upgradeRouteList converts one stored route list to the shape routes have
// now, and reports whether it changed. A list whose every route is in the new
// shape is left as it is, so the step is idempotent; in a list holding any
// legacy route the legacy routes are converted (upgradeLegacyRoutes) and each
// new one kept. A legacy route that still carries a use is blocked, as every
// reader of one reads it (ADR 26-10-27-880 §8).
func upgradeRouteList(what string, raw json.RawMessage) (json.RawMessage, bool, error) {
	if text := strings.TrimSpace(string(raw)); text == "" || text == "null" {
		return raw, false, nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, false, err
	}
	type storedRoute struct {
		legacyRoute
		Hosts []string `json:"hosts"`
		Use   string   `json:"use"`
	}
	stored := make([]storedRoute, len(elements))
	hasLegacy := false
	for i, element := range elements {
		if err := json.Unmarshal(element, &stored[i]); err != nil {
			return nil, false, err
		}
		hasLegacy = hasLegacy || stored[i].Hosts == nil
	}
	if !hasLegacy {
		return raw, false, nil
	}
	// The legacy routes convert as one list with the routes already in the new
	// shape, so one that refuses the list refuses it whole, and one that only
	// drops leaves the new-shape routes standing.
	var legacy []legacyRoute
	var current []model.SecretRoute
	for i, element := range elements {
		if stored[i].Hosts != nil {
			var route model.SecretRoute
			if err := json.Unmarshal(element, &route); err != nil {
				return nil, false, err
			}
			current = append(current, route)
			continue
		}
		old := stored[i].legacyRoute
		old.Blocked = old.Blocked || strings.TrimSpace(stored[i].Use) != ""
		legacy = append(legacy, old)
	}
	encoded, err := json.Marshal(upgradeLegacyRoutes(what, legacy, current))
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

// upgradeNestedRoutes converts the route lists inside one stored JSON
// document: the "routes" member of every object path reaches. path names the
// members to descend through, each an object or a list of them.
func upgradeNestedRoutes(what string, raw json.RawMessage, path ...string) (json.RawMessage, bool, error) {
	text := bytes.TrimSpace(raw)
	if len(text) == 0 || string(text) == "null" {
		return raw, false, nil
	}
	if text[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(text, &items); err != nil {
			return nil, false, err
		}
		changed := false
		for i, item := range items {
			next, itemChanged, err := upgradeNestedRoutes(what, item, path...)
			if err != nil {
				return nil, false, err
			}
			if itemChanged {
				items[i], changed = next, true
			}
		}
		if !changed {
			return raw, false, nil
		}
		encoded, err := json.Marshal(items)
		return encoded, true, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(text, &object); err != nil {
		return nil, false, err
	}
	key := "routes"
	if len(path) > 0 {
		key = path[0]
	}
	member, ok := object[key]
	if !ok {
		return raw, false, nil
	}
	var next json.RawMessage
	var changed bool
	var err error
	if len(path) > 0 {
		next, changed, err = upgradeNestedRoutes(what, member, path[1:]...)
	} else {
		next, changed, err = upgradeRouteList(what, member)
	}
	if err != nil || !changed {
		return raw, false, err
	}
	object[key] = next
	encoded, err := json.Marshal(object)
	return encoded, true, err
}

// routeColumn is one stored column holding routes: where in it they are, and
// which discoboxes' pools hold them, whose sentinel delivery is marked when
// they change.
type routeColumn struct {
	table, column string
	// path descends from the column's document to the objects whose "routes"
	// member is a route list; none is the column being the list.
	path    []string
	holders func(tx *gorm.DB, id string) ([]sandboxRef, error)
}

// routeColumns are every stored column that holds routes.
var routeColumns = []routeColumn{
	{table: "secrets", column: "routes", holders: secretHolders},
	{table: "secret_bindings", column: "routes", holders: bindingHolders},
	{table: "secret_requests", column: "routes"},
	// The snapshot each binding event keeps of its binding.
	{table: "secret_binding_events", column: "binding", path: []string{}},
	// The credentials a harness image declares, each with its routes.
	{table: "harness_configs", column: "secrets", path: []string{}},
	// The save-back providers a harness bundle declares, each with its
	// routes.
	{table: "harness_configs", column: "save_back", path: []string{"providers"}},
}

// upgradeRouteShapes converts every stored route of the legacy shape to the
// shape routes have now (ADR 26-10-27-880 §4, §8), without widening:
// secretroute.FromLegacy keeps what each covered — a host h becomes h and
// *.h, a path p becomes p/** — and a route it refuses is dropped when open
// and kept refusing at least what it did when blocked (upgradeLegacyRoutes).
// It runs after the grant retirement, which reads the legacy shape, on every
// start; a row whose routes are all in the new shape is left alone, so after
// the first run it writes nothing. Each row converts in its own transaction,
// with the sentinel delivery of the discoboxes holding its routes marked, so
// their pools are sent the lists in the shape they are now held in.
func upgradeRouteShapes(db *gorm.DB) error {
	for _, column := range routeColumns {
		if err := upgradeRouteColumn(db, column); err != nil {
			return err
		}
	}
	return nil
}

func upgradeRouteColumn(db *gorm.DB, column routeColumn) error {
	migrator := db.Migrator()
	if !migrator.HasTable(column.table) || !migrator.HasColumn(column.table, column.column) {
		return nil
	}
	// Only a legacy route has a "host" member; a list of new ones has none.
	var rows []map[string]any
	if err := db.Table(column.table).Select("id", column.column).Where(column.column+" LIKE ?", `%"host"%`).Order("id").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		id := columnText(row["id"])
		what := fmt.Sprintf("%s %s", strings.TrimSuffix(column.table, "s"), id)
		raw := json.RawMessage(columnText(row[column.column]))
		var next json.RawMessage
		var changed bool
		var err error
		if column.path == nil {
			next, changed, err = upgradeRouteList(what, raw)
		} else {
			next, changed, err = upgradeNestedRoutes(what, raw, column.path...)
		}
		if err != nil {
			return fmt.Errorf("%s %s routes: %w", column.table, id, err)
		}
		if !changed {
			continue
		}
		log.Printf("converted the routes of %s to host patterns: a host covers only itself now, so each legacy host h is h and *.h, and each path p is p/** (ADR 26-10-27-880 §8)", what)
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Table(column.table).Where("id = ?", id).UpdateColumn(column.column, string(next)).Error; err != nil {
				return err
			}
			if column.holders == nil {
				return nil
			}
			held, err := column.holders(tx, id)
			if err != nil {
				return err
			}
			return markDirty(tx, sentinelsResourceType, held)
		}); err != nil {
			return err
		}
	}
	return nil
}

// bindingHolders are the discoboxes holding a sentinel a binding delivers, or
// one handed on under it: the ones whose pool holds its routes.
func bindingHolders(db *gorm.DB, bindingID string) ([]sandboxRef, error) {
	var out []sandboxRef
	err := db.Table("sandbox_secrets").Select("DISTINCT project_id, sandbox_id AS id").
		Where("binding_id = ? OR binding_id IN (?)", bindingID, db.Table("secret_bindings").Select("id").Where("parent_id = ?", bindingID)).
		Scan(&out).Error
	return out, err
}
