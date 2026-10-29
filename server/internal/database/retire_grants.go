package database

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/x/id"
)

// The retired grant model's words, which only these migrations still read.
const (
	retiredGrantScopeSandbox       = "sandbox"
	retiredGrantScopeHarnessConfig = "harnessConfig"
	retiredGrantScopeProject       = "project"
	retiredGrantPurposeDelegate    = "delegate"
	retiredRefreshReason           = "refresh"
	// bindingExpiryResourceType and bindingExpiryID are the binding expiry
	// sweep's mark (store.BindingExpiryResourceType), spelled here because
	// this package sits below the store's binding writers.
	bindingExpiryResourceType = "secretBindingExpiry"
	bindingExpiryID           = "all"
	// retiredOpenRefreshIndex is the partial index that held one open refresh
	// request per secret while refreshes were secret requests.
	retiredOpenRefreshIndex = "idx_secret_request_open_refresh"
)

// renameSecretBindingTTLColumn carries a secret's limit from
// max_grant_ttl_seconds to max_binding_ttl_seconds: the same ceiling, on what
// replaced grants.
func renameSecretBindingTTLColumn(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("secrets") || !migrator.HasColumn("secrets", "max_grant_ttl_seconds") {
		return nil
	}
	if migrator.HasColumn("secrets", "max_binding_ttl_seconds") {
		return nil
	}
	return db.Exec(`ALTER TABLE "secrets" RENAME COLUMN "max_grant_ttl_seconds" TO "max_binding_ttl_seconds"`).Error
}

// deduplicateSecretNames renames every secret that shares its name with an
// older one in its project, so the name can be the whole uniqueness domain:
// a request, a binding and a person name a secret by it. The older keeps its
// name; each later one gets the first free "-2", "-3", …, logged.
func deduplicateSecretNames(db *gorm.DB) error {
	if !db.Migrator().HasTable("secrets") {
		return nil
	}
	var rows []struct {
		ID        string
		ProjectID string
		Name      string
	}
	if err := db.Table("secrets").Select("id", "project_id", "name").Order("created_at ASC, id ASC").Scan(&rows).Error; err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, row := range rows {
		taken[row.ProjectID+"\x00"+row.Name] = true
	}
	seen := map[string]bool{}
	for _, row := range rows {
		key := row.ProjectID + "\x00" + row.Name
		if !seen[key] {
			seen[key] = true
			continue
		}
		var name string
		for n := 2; ; n++ {
			if name = fmt.Sprintf("%s-%d", row.Name, n); !taken[row.ProjectID+"\x00"+name] {
				break
			}
		}
		taken[row.ProjectID+"\x00"+name] = true
		seen[row.ProjectID+"\x00"+name] = true
		log.Printf("renamed secret %s of project %s from %q to %q: a secret's name is unique in its project now", row.ID, row.ProjectID, row.Name, name)
		if err := db.Exec("UPDATE secrets SET name = ? WHERE id = ?", name, row.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

// retireSecretGrants turns the grant model into bindings (ADR 26-10-27-880 §8,
// ADR 26-10-27-939 §3), widening nothing:
//
//   - a secret's host and routes become its routes alone: its host, as a
//     route, when it had no routes; its routes narrowed to the host when it
//     had both; `*` when it had neither, since it went anywhere;
//   - a harness config's binding covered by a live grant becomes a use
//     binding on the harness config, with the grant's hosts as its routes
//     (`*` for a grant with none) and its expiry; one no grant covered was
//     never sent, and goes;
//   - a sandbox's own sentinel covered by a live grant becomes a use binding
//     on the sandbox, the same way; one no grant covered is asked for again
//     as a pending request from the sandbox, for a person to answer;
//   - a delegation grant becomes a delegate binding on its sandbox, and a
//     grant handed on under one gets that binding as its parent;
//   - a refresh request moves to its own record; a request the proxy opened
//     on its own is closed, as nothing opens one any more; an agent's
//     request keeps what it asked, its hosts as routes;
//   - the grant and harness-binding tables go, with the columns only they
//     and the request's old shape read.
//
// Each binding made is recorded as created by the system, caused by the
// upgrade. A binding's ID is taken from the row it replaces, so a start
// interrupted part way finds what it already made and carries on. Every
// sandbox holding a sentinel is marked for delivery, so its pool is sent the
// route lists every sentinel now carries.
func retireSecretGrants(db *gorm.DB) error {
	migrator := db.Migrator()
	if err := moveSecretHostsToRoutes(db); err != nil {
		return err
	}
	// What each grant became, for the requests approved with one.
	grantBindings := map[string]string{}
	if migrator.HasTable("secret_grants") || migrator.HasTable("harness_config_secret_bindings") {
		now := time.Now().UTC()
		grants, err := readRetiredGrants(db, now)
		if err != nil {
			return err
		}
		if err := bindDelegationGrants(db, grants, grantBindings); err != nil {
			return err
		}
		unsent, err := bindHarnessConfigSecrets(db, grants)
		if err != nil {
			return err
		}
		if err := bindSandboxSecrets(db, grants, grantBindings, unsent); err != nil {
			return err
		}
		if err := markEverySentinelHolder(db); err != nil {
			return err
		}
		for _, table := range []string{"secret_grants", "harness_config_secret_bindings"} {
			if migrator.HasTable(table) {
				if err := migrator.DropTable(table); err != nil {
					return err
				}
			}
		}
	}
	if err := moveRetiredRequests(db, grantBindings); err != nil {
		return err
	}
	return dropRetiredSecretColumns(db)
}

// moveSecretHostsToRoutes makes a secret's routes the whole of where it may
// go, from its host and its routes, while the host column is there to read.
func moveSecretHostsToRoutes(db *gorm.DB) error {
	if !db.Migrator().HasTable("secrets") || !db.Migrator().HasColumn("secrets", "host") {
		return nil
	}
	var rows []struct {
		ID        string
		ProjectID string
		Host      string
		Routes    string
	}
	if err := db.Table("secrets").Select("id", "project_id", "host", "COALESCE(routes, '') AS routes").Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		routes, err := decodeLegacyRoutes(row.Routes)
		if err != nil {
			return fmt.Errorf("secret %s routes: %w", row.ID, err)
		}
		next, changed := routesFromHost(hostscope.Normalize(row.Host), routes)
		if !changed {
			continue
		}
		next = upgradeRoutes(fmt.Sprintf("secret %s of project %s", row.ID, row.ProjectID), next)
		if len(next) == 1 && next[0].Host == secretroute.AnyHost && row.Host == "" {
			log.Printf("secret %s of project %s had no host and no routes, and went anywhere: it has the route * now, which a person may narrow (ADR 26-10-27-939)", row.ID, row.ProjectID)
		} else {
			log.Printf("secret %s of project %s had host %q and routes %+v: it has routes %+v now, narrowed to the host and never unblocked (ADR 26-10-27-880 §8)", row.ID, row.ProjectID, row.Host, routes, next)
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := db.Exec("UPDATE secrets SET routes = ? WHERE id = ?", string(encoded), row.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

// routesFromHost is where a secret with host and routes may go, as routes
// alone: the host when it had no routes; its routes within the host when it
// had both — one above the host is narrowed to it, one beside it went nowhere
// and goes; `*` when it had neither. changed reports whether routes moved.
func routesFromHost(host string, routes []legacyRoute) ([]legacyRoute, bool) {
	switch {
	case host == "" && len(routes) == 0:
		return []legacyRoute{{Host: secretroute.AnyHost}}, true
	case host == "":
		return routes, false
	case len(routes) == 0:
		return []legacyRoute{{Host: host}}, true
	}
	var out []legacyRoute
	for _, route := range routes {
		routeHost := hostscope.Normalize(route.Host)
		switch {
		case hostscope.Covers(host, routeHost):
			out = append(out, route)
		case routeHost == secretroute.AnyHost || hostscope.Covers(routeHost, host):
			// Brought down to the host, an open route gains the host's
			// labels, and may then outrank a blocked route it used to lose
			// to. It goes rather than unblock what that route refused.
			if !route.Blocked && outranksOnNarrowing(route, routeHost, host, routes) {
				continue
			}
			route.Host = host
			out = append(out, route)
		}
	}
	if len(out) == len(routes) {
		same := true
		for i := range out {
			same = same && out[i] == routes[i]
		}
		if same {
			return routes, false
		}
	}
	return out, true
}

// outranksOnNarrowing reports whether narrowing an open route from routeHost
// down to host would let it win somewhere a blocked route of the same list
// used to: a blocked route covering host with more labels than routeHost. On
// host itself, the narrowed route wins only where its path is the longer one,
// so it outranks the block only where its path lies within the block's; above
// host, the narrowed route has more labels and wins wherever their paths meet.
func outranksOnNarrowing(route legacyRoute, routeHost, host string, routes []legacyRoute) bool {
	for _, other := range routes {
		otherHost := hostscope.Normalize(other.Host)
		if !other.Blocked || otherHost == secretroute.AnyHost || !hostscope.Covers(otherHost, host) {
			continue
		}
		if hostLabels(otherHost) <= hostLabels(routeHost) {
			continue
		}
		within := legacyPathCovers(other.Path, route.Path)
		if otherHost == host {
			if within {
				return true
			}
			continue
		}
		if within || legacyPathCovers(route.Path, other.Path) {
			return true
		}
	}
	return false
}

// hostLabels is how specific a route's host is: its label count, none for
// `*`.
func hostLabels(host string) int {
	if host == secretroute.AnyHost || host == "" {
		return 0
	}
	return strings.Count(host, ".") + 1
}

// retiredGrant is one grant as the conversion reads it.
type retiredGrant struct {
	ID           string
	ProjectID    string
	SecretID     string
	Scope        string
	ScopeKey     string
	Purpose      string
	Hosts        []string
	ExpiresAt    *time.Time
	DelegationID string
}

// readRetiredGrants reads every live grant. A lapsed one authorized nothing,
// and goes with the table.
func readRetiredGrants(db *gorm.DB, now time.Time) ([]retiredGrant, error) {
	migrator := db.Migrator()
	if !migrator.HasTable("secret_grants") {
		return nil, nil
	}
	columns := []string{"id", "project_id", "secret_id", "scope", "scope_key", "COALESCE(hosts, '') AS hosts", "expires_at"}
	hasPurpose := migrator.HasColumn("secret_grants", "purpose")
	if hasPurpose {
		columns = append(columns, "purpose")
	}
	hasDelegation := migrator.HasColumn("secret_grants", "delegation_id")
	if hasDelegation {
		columns = append(columns, "delegation_id")
	}
	var rows []map[string]any
	if err := db.Table("secret_grants").Select(columns).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	var out []retiredGrant
	for _, row := range rows {
		grant := retiredGrant{
			ID:           columnText(row["id"]),
			ProjectID:    columnText(row["project_id"]),
			SecretID:     columnText(row["secret_id"]),
			Scope:        columnText(row["scope"]),
			ScopeKey:     columnText(row["scope_key"]),
			Purpose:      columnText(row["purpose"]),
			DelegationID: columnText(row["delegation_id"]),
		}
		if text := columnText(row["hosts"]); text != "" && text != "null" {
			if err := json.Unmarshal([]byte(text), &grant.Hosts); err != nil {
				return nil, fmt.Errorf("grant %s hosts: %w", grant.ID, err)
			}
		}
		// A host no route can be was never one the proxy matched: it goes,
		// and a grant left with no host it named authorized nothing, and is
		// not read as one naming none.
		if named := len(grant.Hosts); named > 0 {
			var hosts []legacyRoute
			for _, host := range grant.Hosts {
				hosts = append(hosts, legacyRoute{Host: host})
			}
			grant.Hosts = nil
			for _, route := range upgradeRoutes("grant "+grant.ID, hosts) {
				grant.Hosts = append(grant.Hosts, route.Host)
			}
			if len(grant.Hosts) == 0 {
				continue
			}
		}
		if expires := columnTime(row["expires_at"]); expires != nil {
			if !now.Before(*expires) {
				continue
			}
			grant.ExpiresAt = expires
		}
		out = append(out, grant)
	}
	return out, nil
}

// columnTime reads a time column the way a driver hands it back.
func columnTime(value any) *time.Time {
	switch v := value.(type) {
	case time.Time:
		t := v.UTC()
		return &t
	case *time.Time:
		if v == nil {
			return nil
		}
		t := v.UTC()
		return &t
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
			if t, err := time.Parse(layout, v); err == nil {
				t = t.UTC()
				return &t
			}
		}
	}
	return nil
}

// upgradeRoutes are legacy routes the upgrade reads, read as every legacy
// route was written (normalizeLegacyRoute): one that no route could be — a
// host that is no host name, a control character — is dropped and logged
// rather than carried on, and a list left empty sends nowhere, which widens
// nothing.
func upgradeRoutes(what string, routes []legacyRoute) []legacyRoute {
	out := make([]legacyRoute, 0, len(routes))
	for _, route := range routes {
		next, err := normalizeLegacyRoute(route)
		if err != nil {
			log.Printf("dropped route %q%s of %s: %v", route.Host, route.Path, what, err)
			continue
		}
		if !slices.Contains(out, next) {
			out = append(out, next)
		}
	}
	return out
}

// grantRoutes are the routes a binding made from a grant carries: each host
// it named as it covered it, the host and every host beneath it
// (upgradeLegacyRoute), and `*` for a grant that named none (ADR 26-10-27-939
// §3).
func grantRoutes(grant retiredGrant) []model.SecretRoute {
	if len(grant.Hosts) == 0 {
		return []model.SecretRoute{{Hosts: []string{secretroute.AnyHost}}}
	}
	var legacy []legacyRoute
	for _, host := range hostscope.List(grant.Hosts...) {
		legacy = append(legacy, legacyRoute{Host: host})
	}
	return upgradeLegacyRoutes("grant "+grant.ID, legacy, nil)
}

// coveringGrant is what the live use grants of secretID covering a target,
// at any of scopes, authorized together, or nil when none does. Old resolves
// accepted any of them, so one binding takes them all: every grant's hosts —
// none, so `*`, when any grant had none — the earliest expiry, and the
// delegation they were all handed on under, when they share one. members are
// the grants it stands for; more than one is logged, naming what for.
func coveringGrant(grants []retiredGrant, projectID, secretID string, scopes [][2]string, what string) (merged *retiredGrant, members []retiredGrant) {
	for _, scope := range scopes {
		for _, grant := range grants {
			if grant.ProjectID == projectID && grant.SecretID == secretID && grant.Purpose != retiredGrantPurposeDelegate &&
				grant.Scope == scope[0] && grant.ScopeKey == scope[1] {
				members = append(members, grant)
			}
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	return mergeUpgradeGrants(members, what), members
}

func mergeUpgradeGrants(members []retiredGrant, what string) *retiredGrant {
	out := members[0]
	if len(members) == 1 {
		return &out
	}
	ids := make([]string, 0, len(members))
	anyHost := false
	var hosts []string
	for _, grant := range members {
		ids = append(ids, grant.ID)
		if len(grant.Hosts) == 0 {
			anyHost = true
		}
		hosts = append(hosts, grant.Hosts...)
		if grant.ExpiresAt != nil && (out.ExpiresAt == nil || grant.ExpiresAt.Before(*out.ExpiresAt)) {
			out.ExpiresAt = grant.ExpiresAt
		}
		if grant.DelegationID != out.DelegationID {
			out.DelegationID = ""
		}
	}
	out.Hosts = hostscope.List(hosts...)
	if anyHost {
		out.Hosts = nil
	}
	log.Printf("%s was covered by %d grants (%s); its binding takes all their hosts and the earliest expiry (ADR 26-10-27-880 §8)",
		what, len(members), strings.Join(ids, ", "))
	return &out
}

// bindingIDFor is the binding ID a row converted into a binding gets: its own
// ID's random part under the binding prefix, so a re-run finds it.
func bindingIDFor(rowID string) string {
	_, suffix, ok := strings.Cut(rowID, "_")
	if !ok {
		suffix = rowID
	}
	return model.PrefixSecretBinding + "_" + suffix
}

// createUpgradeBinding stores binding unless a re-run already did, with its
// created event.
func createUpgradeBinding(tx *gorm.DB, binding *model.SecretBinding) error {
	var existing int64
	if err := tx.Model(&model.SecretBinding{}).Where("id = ?", binding.ID).Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	binding.CreatedBy = model.SecretBindingActorSystem
	if err := tx.Create(binding).Error; err != nil {
		return err
	}
	// Armed as every binding written with an expiry is (store's
	// recordBindingEventTx), named here because this package sits below it.
	if binding.ExpiresAt != nil {
		if err := reconcile.MarkAtTx(context.Background(), tx, bindingExpiryResourceType, bindingExpiryID, *binding.ExpiresAt); err != nil {
			return err
		}
	}
	return tx.Create(&model.SecretBindingEvent{
		ProjectID: binding.ProjectID,
		BindingID: binding.ID,
		SecretID:  binding.SecretID,
		Event:     model.SecretBindingEventCreated,
		Actor:     model.SecretBindingActorSystem,
		Cause:     "upgrade",
		Binding:   *binding,
	}).Error
}

// bindDelegationGrants makes a delegate binding on its sandbox of every live
// delegation grant, recording grant ID to binding ID in grantBindings.
func bindDelegationGrants(db *gorm.DB, grants []retiredGrant, grantBindings map[string]string) error {
	groups := map[[3]string][]retiredGrant{}
	var keys [][3]string
	for _, grant := range grants {
		if grant.Purpose != retiredGrantPurposeDelegate || grant.Scope != retiredGrantScopeSandbox {
			continue
		}
		key := [3]string{grant.ProjectID, grant.ScopeKey, grant.SecretID}
		if len(groups[key]) == 0 {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], grant)
	}
	for _, key := range keys {
		members := groups[key]
		grant := mergeUpgradeGrants(members, fmt.Sprintf("sandbox %s's delegation of %s", key[1], key[2]))
		binding := &model.SecretBinding{
			ID: bindingIDFor(grant.ID), ProjectID: grant.ProjectID, SecretID: grant.SecretID,
			TargetKind: model.SecretBindingTargetSandbox, TargetID: grant.ScopeKey,
			Delegate: true, Routes: grantRoutes(*grant), ExpiresAt: grant.ExpiresAt,
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			// A failed older migration may already have committed one member.
			// Keep its ID so existing children and requests still refer to it.
			var rows []model.SecretBinding
			if err := tx.Where("target_kind = ? AND target_id = ? AND secret_id = ?", binding.TargetKind, binding.TargetID, binding.SecretID).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return createUpgradeBinding(tx, binding)
			}
			existing := rows[0]
			if !existing.Delegate || existing.Use || existing.ProjectID != binding.ProjectID {
				return fmt.Errorf("cannot migrate delegation for sandbox %s: conflicting binding %s", binding.TargetID, existing.ID)
			}
			routes, expires := binding.Routes, binding.ExpiresAt
			*binding = existing
			if reflect.DeepEqual(existing.Routes, routes) && reflect.DeepEqual(existing.ExpiresAt, expires) {
				return nil
			}
			binding.Routes, binding.ExpiresAt = routes, expires
			if err := tx.Save(binding).Error; err != nil {
				return err
			}
			if expires != nil {
				if err := reconcile.MarkAtTx(context.Background(), tx, bindingExpiryResourceType, bindingExpiryID, *expires); err != nil {
					return err
				}
			}
			return tx.Create(&model.SecretBindingEvent{ProjectID: binding.ProjectID, BindingID: binding.ID,
				SecretID: binding.SecretID, Event: model.SecretBindingEventUpdated,
				Actor: model.SecretBindingActorSystem, Cause: "upgrade", Binding: *binding}).Error
		}); err != nil {
			return err
		}
		for _, member := range members {
			grantBindings[member.ID] = binding.ID
		}
		log.Printf("delegation grants of project %s are delegate binding %s on sandbox %s (ADR 26-10-27-880 §8)", grant.ProjectID, binding.ID, grant.ScopeKey)
	}
	return nil
}

// bindHarnessConfigSecrets makes a use binding on its harness config of every
// harness config binding a live grant covers, and points the sentinels it
// delivered at it. One no grant covered is dropped, and the sentinels it
// delivered are returned: each is left to a grant of its own sandbox, and
// goes when there is none, since it was never sent.
func bindHarnessConfigSecrets(db *gorm.DB, grants []retiredGrant) (map[string]bool, error) {
	unsent := map[string]bool{}
	migrator := db.Migrator()
	if !migrator.HasTable("harness_config_secret_bindings") {
		return unsent, nil
	}
	var rows []struct {
		ID              string
		ProjectID       string
		HarnessConfigID string
		EnvName         string
		SecretID        string
	}
	if err := db.Table("harness_config_secret_bindings").Select("id", "project_id", "harness_config_id", "env_name", "secret_id").Order("id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	hasSource := migrator.HasColumn("sandbox_secrets", "source_harness_config_id")
	for _, row := range rows {
		grant, _ := coveringGrant(grants, row.ProjectID, row.SecretID, [][2]string{
			{retiredGrantScopeHarnessConfig, row.HarnessConfigID}, {retiredGrantScopeProject, row.ProjectID}},
			fmt.Sprintf("harness config %s's %s in project %s", row.HarnessConfigID, row.EnvName, row.ProjectID))
		if err := db.Transaction(func(tx *gorm.DB) error {
			delivered := tx.Table("sandbox_secrets").Where("secret_id = ? AND env_name = ? AND sandbox_id IN (?)",
				row.SecretID, row.EnvName, tx.Table("sandboxes").Select("id").Where("harness_config_id = ?", row.HarnessConfigID))
			if hasSource {
				delivered = delivered.Where("source_harness_config_id = ?", row.HarnessConfigID)
			}
			if grant == nil {
				log.Printf("dropped harness config %s's binding of %s to secret %s in project %s: no grant covered it, so it was never sent (ADR 26-10-27-880 §8)",
					row.HarnessConfigID, row.EnvName, row.SecretID, row.ProjectID)
				var ids []string
				if err := delivered.Pluck("id", &ids).Error; err != nil {
					return err
				}
				for _, id := range ids {
					unsent[id] = true
				}
				return nil
			}
			binding := &model.SecretBinding{
				ID:         bindingIDFor(row.ID),
				ProjectID:  row.ProjectID,
				SecretID:   row.SecretID,
				TargetKind: model.SecretBindingTargetHarnessConfig,
				TargetID:   row.HarnessConfigID,
				Use:        true,
				EnvName:    row.EnvName,
				Routes:     grantRoutes(*grant),
				ExpiresAt:  grant.ExpiresAt,
			}
			if err := createUpgradeBinding(tx, binding); err != nil {
				return err
			}
			return delivered.Update("binding_id", binding.ID).Error
		}); err != nil {
			return nil, err
		}
	}
	return unsent, nil
}

// bindSandboxSecrets makes a use binding on its sandbox of every sentinel a
// sandbox was given itself that a live grant covers, and points the sentinel
// at it; one no grant covered is asked for again, and goes — unless it came
// from a harness config binding no grant covered (unsent), which nobody asked
// for and which just goes. A grant of the sandbox's own is recorded in
// grantBindings as the binding it became.
func bindSandboxSecrets(db *gorm.DB, grants []retiredGrant, grantBindings map[string]string, unsent map[string]bool) error {
	var rows []struct {
		ID              string
		ProjectID       string
		SandboxID       string
		SecretID        string
		EnvName         string
		HarnessConfigID *string
	}
	if err := db.Table("sandbox_secrets").
		Select("sandbox_secrets.id, sandbox_secrets.project_id, sandbox_secrets.sandbox_id, sandbox_secrets.secret_id, sandbox_secrets.env_name, sandboxes.harness_config_id").
		Joins("JOIN sandboxes ON sandboxes.id = sandbox_secrets.sandbox_id").
		Where("sandbox_secrets.binding_id = ''").Order("sandbox_secrets.id").Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		scopes := [][2]string{{retiredGrantScopeSandbox, row.SandboxID}}
		if row.HarnessConfigID != nil && *row.HarnessConfigID != "" {
			scopes = append(scopes, [2]string{retiredGrantScopeHarnessConfig, *row.HarnessConfigID})
		}
		scopes = append(scopes, [2]string{retiredGrantScopeProject, row.ProjectID})
		grant, members := coveringGrant(grants, row.ProjectID, row.SecretID, scopes,
			fmt.Sprintf("sandbox %s's %s in project %s", row.SandboxID, row.EnvName, row.ProjectID))
		if err := db.Transaction(func(tx *gorm.DB) error {
			switch {
			case grant == nil && unsent[row.ID]:
				log.Printf("removed sandbox %s's sentinel for %s in project %s: its harness config's binding was never sent", row.SandboxID, row.EnvName, row.ProjectID)
				return tx.Exec("DELETE FROM sandbox_secrets WHERE id = ?", row.ID).Error
			case grant == nil:
				return reaskForSentinel(tx, row.ID, row.ProjectID, row.SandboxID, row.SecretID, row.EnvName)
			}
			binding := &model.SecretBinding{
				ID:         bindingIDFor(row.ID),
				ProjectID:  row.ProjectID,
				SecretID:   row.SecretID,
				TargetKind: model.SecretBindingTargetSandbox,
				TargetID:   row.SandboxID,
				Use:        true,
				EnvName:    row.EnvName,
				Routes:     grantRoutes(*grant),
				ExpiresAt:  grant.ExpiresAt,
				ParentID:   grantBindings[grant.DelegationID],
			}
			if err := createUpgradeBinding(tx, binding); err != nil {
				return err
			}
			for _, member := range members {
				if member.Scope == retiredGrantScopeSandbox {
					grantBindings[member.ID] = binding.ID
				}
			}
			return tx.Exec("UPDATE sandbox_secrets SET binding_id = ? WHERE id = ?", binding.ID, row.ID).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// reaskForSentinel opens a pending request for a sentinel no grant covered,
// naming the secret and the variable, and removes the sentinel: it was never
// sent, and only a binding delivers one now.
func reaskForSentinel(tx *gorm.DB, rowID, projectID, sandboxID, secretID, envName string) error {
	var secrets []struct {
		Name   string
		Routes string
	}
	if err := tx.Table("secrets").Select("name", "COALESCE(routes, '') AS routes").Where("id = ?", secretID).Limit(1).Scan(&secrets).Error; err != nil {
		return err
	}
	if len(secrets) == 1 {
		routes, err := decodeLegacyRoutes(secrets[0].Routes)
		if err != nil {
			return err
		}
		requestID, err := id.New(id.PrefixSecretRequest)
		if err != nil {
			return err
		}
		if err := insertUpgradeRequest(tx, requestID, projectID, sandboxID, secrets[0].Name, envName, routes,
			"Asked again for a credential this discobox was given with no grant covering it, so it was never sent; only a binding delivers one now (ADR 26-10-27-880 §8)."); err != nil {
			return err
		}
		log.Printf("sandbox %s of project %s held %s for secret %s with no grant covering it; asked again as pending request %s (ADR 26-10-27-880 §8)",
			sandboxID, projectID, envName, secretID, requestID)
	}
	return tx.Exec("DELETE FROM sandbox_secrets WHERE id = ?", rowID).Error
}

// insertUpgradeRequest writes a pending request in whichever shape the table
// has at this point of the upgrade: the old one still carries a type and a
// list of hosts, which the request's routes become later on. Its routes are
// written in the shape routes have now.
func insertUpgradeRequest(tx *gorm.DB, requestID, projectID, sandboxID, name, envName string, routes []legacyRoute, justification string) error {
	migrator := tx.Migrator()
	columns := map[string]any{
		"id": requestID, "project_id": projectID, "requested_by": "agent:" + sandboxID, "sandbox_id": sandboxID,
		"name": name, "env_name": envName, "justification": justification, "purpose": model.SecretPurposeUse,
		"status": model.SecretRequestStatusPending, "created_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}
	encodedRoutes, err := json.Marshal(upgradeLegacyRoutes("request "+requestID, routes, nil))
	if err != nil {
		return err
	}
	columns["routes"] = string(encodedRoutes)
	if migrator.HasColumn("secret_requests", "type") {
		columns["type"] = model.SecretTypeToken
	}
	if migrator.HasColumn("secret_requests", "hosts") {
		var hosts []string
		for _, route := range routes {
			hosts = append(hosts, route.Host)
		}
		encoded, err := json.Marshal(hosts)
		if err != nil {
			return err
		}
		columns["hosts"] = string(encoded)
	}
	return tx.Table("secret_requests").Create(columns).Error
}

// markEverySentinelHolder marks the sentinel delivery of every sandbox with a
// sentinel, so its pool is sent the route lists each now carries.
func markEverySentinelHolder(db *gorm.DB) error {
	var holders []sandboxRef
	if err := db.Table("sandbox_secrets").Select("DISTINCT project_id, sandbox_id AS id").Scan(&holders).Error; err != nil {
		return err
	}
	return markDirty(db, sentinelsResourceType, holders)
}

// moveRetiredRequests reads the request table's old shape into its new one:
// a refresh to its own record, a request the proxy opened closed, and an
// agent's request with its hosts as routes, its lifetime and its decision
// under their new names, and the binding its grant became when one did.
func moveRetiredRequests(db *gorm.DB, grantBindings map[string]string) error {
	migrator := db.Migrator()
	if !migrator.HasTable("secret_requests") || !migrator.HasColumn("secret_requests", "reason") {
		return nil
	}
	var rows []map[string]any
	columns := []string{"id", "project_id", "secret_id", "sandbox_id", "status", "name", "reason",
		"COALESCE(refresh_cause, '') AS refresh_cause", "COALESCE(refresh_answer, '') AS refresh_answer",
		"COALESCE(hosts, '') AS hosts", "COALESCE(grant_ttl_seconds, 0) AS grant_ttl_seconds",
		"COALESCE(grant_id, '') AS grant_id", "closed_at", "created_at"}
	if err := db.Table("secret_requests").Select(columns).Order("id").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		requestID := columnText(row["id"])
		if err := db.Transaction(func(tx *gorm.DB) error {
			switch {
			case columnText(row["reason"]) == retiredRefreshReason:
				return moveRefreshRequest(tx, row)
			case columnText(row["name"]) == "":
				if columnText(row["status"]) == model.SecretRequestStatusPending {
					log.Printf("deleted request %s of project %s: the proxy opened it on meeting a sentinel with no grant, and nothing opens one now; a sentinel exists only for a binding (ADR 26-10-27-880 §1)",
						requestID, columnText(row["project_id"]))
				}
				return tx.Exec("DELETE FROM secret_requests WHERE id = ?", requestID).Error
			}
			var hosts []string
			if text := columnText(row["hosts"]); text != "" && text != "null" {
				if err := json.Unmarshal([]byte(text), &hosts); err != nil {
					return err
				}
			}
			var routes []legacyRoute
			for _, host := range hosts {
				routes = append(routes, legacyRoute{Host: host})
			}
			what := "request " + requestID
			encoded, err := json.Marshal(upgradeLegacyRoutes(what, upgradeRoutes(what, routes), nil))
			if err != nil {
				return err
			}
			updates := map[string]any{"routes": string(encoded), "ttl_seconds": row["grant_ttl_seconds"]}
			if closed := columnTime(row["closed_at"]); closed != nil {
				updates["decided_at"] = *closed
			}
			if bindingID := grantBindings[columnText(row["grant_id"])]; bindingID != "" {
				updates["binding_id"] = bindingID
			}
			return tx.Table("secret_requests").Where("id = ?", requestID).Updates(updates).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// moveRefreshRequest copies one refresh request to its own record, under the
// same ID, and removes it from the requests.
func moveRefreshRequest(tx *gorm.DB, row map[string]any) error {
	status := model.SecretRefreshStatusPending
	switch columnText(row["status"]) {
	case model.SecretRequestStatusApproved:
		status = model.SecretRefreshStatusAnswered
	case model.SecretRequestStatusDenied:
		status = model.SecretRefreshStatusDismissed
	}
	refresh := &model.SecretRefreshRequest{
		ID:        columnText(row["id"]),
		ProjectID: columnText(row["project_id"]),
		SecretID:  columnText(row["secret_id"]),
		SandboxID: columnText(row["sandbox_id"]),
		Cause:     columnText(row["refresh_cause"]),
		Status:    status,
		ClosedAt:  columnTime(row["closed_at"]),
	}
	if created := columnTime(row["created_at"]); created != nil {
		refresh.CreatedAt = *created
	}
	if refresh.Cause == "" {
		refresh.Cause = model.SecretRefreshCauseStale
	}
	if text := columnText(row["refresh_answer"]); text != "" && text != "null" {
		var answer model.SecretRefreshAnswer
		if err := json.Unmarshal([]byte(text), &answer); err != nil {
			return err
		}
		refresh.Answer = &answer
	}
	var existing int64
	if err := tx.Model(&model.SecretRefreshRequest{}).Where("id = ?", refresh.ID).Count(&existing).Error; err != nil {
		return err
	}
	if existing == 0 {
		if err := tx.Create(refresh).Error; err != nil {
			return err
		}
	}
	return tx.Exec("DELETE FROM secret_requests WHERE id = ?", refresh.ID).Error
}

// dropRetiredSecretColumns drops what only the grant model read: a secret's
// host and the key that kept anonymous ones out of the old uniqueness index;
// the request's old shape; and the mark that told a harness's sentinels from a
// sandbox's own. The indexes over them go with them (dropRetiredColumn).
func dropRetiredSecretColumns(db *gorm.DB) error {
	// The partial index that held one open refresh request per secret names
	// the reason only in its predicate, where no reading of its columns finds
	// it.
	if migrator := db.Migrator(); migrator.HasTable("secret_requests") && migrator.HasIndex("secret_requests", retiredOpenRefreshIndex) {
		if err := migrator.DropIndex("secret_requests", retiredOpenRefreshIndex); err != nil {
			return err
		}
	}
	for _, retired := range []struct {
		table   string
		columns []string
	}{
		{"secrets", []string{"host", "unique_key"}},
		{"secret_requests", []string{"type", "hosts", "grant_ttl_seconds", "grant_id", "reason", "refresh_cause", "refresh_answer", "closed_at"}},
		{"sandbox_secrets", []string{"source_harness_config_id"}},
	} {
		if !db.Migrator().HasTable(retired.table) {
			continue
		}
		for _, column := range retired.columns {
			if err := dropRetiredColumn(db, retired.table, retired.table, column); err != nil {
				return err
			}
		}
	}
	return nil
}
