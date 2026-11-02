package database

import (
	"fmt"

	"gorm.io/gorm"
)

// splitSecretRequestParts converts every secret request from one purpose and
// one route list to its parts (ADR 26-11-02-022 §6): a request that asked to
// delegate becomes one with only a delegate part, any other one with only a
// use part. The part takes the request's routes and the binding approving it
// made, and its outcome is the request's answer: approved or denied, or none
// while it is pending, so it is answered with one decision for its one part.
// Bindings are not touched; a binding carrying both flags stays as it is
// until an approval splits it (§5).
//
// It runs after the retirements and the route upgrade, which still write and
// read the old columns, and drops them once moved. The moves are idempotent,
// so an upgrade stopped between them and the drops is finished by the next
// start.
func splitSecretRequestParts(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("secret_requests") || !migrator.HasColumn("secret_requests", "purpose") {
		return nil
	}
	const outcome = `CASE status WHEN 'approved' THEN 'approved' WHEN 'denied' THEN 'denied' ELSE '' END`
	// The moves read routes and binding_id, which are dropped before purpose:
	// a start that finds purpose but not them was stopped mid-drop, after
	// the moves had committed, and only has the drops to finish.
	moved := !migrator.HasColumn("secret_requests", "routes") || !migrator.HasColumn("secret_requests", "binding_id")
	err := db.Transaction(func(tx *gorm.DB) error {
		if moved {
			return nil
		}
		for _, part := range []struct{ prefix, where string }{
			{"delegate_", "purpose = 'delegate'"},
			{"use_", "purpose <> 'delegate'"},
		} {
			update := fmt.Sprintf(`UPDATE secret_requests SET %[1]sasked = true, %[1]sroutes = routes, %[1]sbinding_id = binding_id, %[1]soutcome = %[2]s WHERE %[3]s`,
				part.prefix, outcome, part.where)
			if err := tx.Exec(update).Error; err != nil {
				return fmt.Errorf("move secret requests to their %s part: %w", part.prefix[:len(part.prefix)-1], err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// purpose last: it is what marks the step as not yet finished.
	for _, column := range []string{"routes", "binding_id", "purpose"} {
		if err := dropRetiredColumn(db, "secret_requests", "secret_requests", column); err != nil {
			return err
		}
	}
	return nil
}

// prepareSecretRequestParts gives a secret_requests table from before parts
// the columns of the shape it is upgraded from — one purpose, one route list,
// one binding — when it lacks any of them, before AutoMigrate. The upgrade
// steps between AutoMigrate and splitSecretRequestParts still write that
// shape (insertUpgradeRequest, the judge's retirement, moveRetiredRequests),
// and the model no longer declares it, so a database old enough to predate
// any of them would otherwise refuse those writes. A table already split has
// use_asked, and is left alone.
func prepareSecretRequestParts(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("secret_requests") || migrator.HasColumn("secret_requests", "use_asked") {
		return nil
	}
	for _, column := range []struct{ name, definition string }{
		{"purpose", "text NOT NULL DEFAULT 'use'"},
		{"routes", "text"},
		{"binding_id", "text NOT NULL DEFAULT ''"},
	} {
		if migrator.HasColumn("secret_requests", column.name) {
			continue
		}
		if err := db.Exec(fmt.Sprintf("ALTER TABLE secret_requests ADD COLUMN %s %s", column.name, column.definition)).Error; err != nil {
			return fmt.Errorf("prepare secret_requests.%s for its split into parts: %w", column.name, err)
		}
	}
	return nil
}
