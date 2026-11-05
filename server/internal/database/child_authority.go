package database

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// Notifications commit with the resource mutation, survive deletion, and are
// independent of a server process. Egress versions are separate: reads retain
// authority when a policy changes, while writes must be reauthorized.
func migrateChildAuthority(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS child_authority_versions (resource TEXT PRIMARY KEY, version BIGINT NOT NULL)`).Error; err != nil {
		return err
	}
	for _, table := range []struct{ name, access, egress string }{
		{"sandboxes", "parent_sandbox_id,discobox_api,desired_state,pool_id,harness_config_id,project_id", "egress"},
		{"projects", "", "egress"}, {"pools", "", "egress"}, {"harness_configs", "", "egress,image_egress_hosts"},
	} {
		obsolete := "child_authority_" + table.name + "_update"
		drop := "DROP TRIGGER IF EXISTS " + obsolete
		if db.Name() == "postgres" {
			drop += " ON " + table.name
		}
		if err := db.Exec(drop).Error; err != nil {
			return err
		}
		for _, kind := range []string{"access", "egress", "delete"} {
			columns, prefix, op := table.access, "", "UPDATE"
			if kind == "egress" {
				columns = table.egress
				prefix = "egress/"
			}
			if kind == "delete" {
				op = "DELETE"
			}
			if op == "UPDATE" && columns == "" {
				continue
			}
			name := "child_authority_" + table.name + "_" + kind
			key := "'" + prefix + table.name + "/' || OLD.id"
			update := "INSERT INTO child_authority_versions(resource,version) VALUES (" + key + ",1) ON CONFLICT(resource) DO UPDATE SET version=child_authority_versions.version+1;"
			var conditions []string
			for _, column := range strings.Split(columns, ",") {
				if column == "" {
					continue
				}
				if db.Name() == "postgres" {
					conditions = append(conditions, "OLD."+column+" IS DISTINCT FROM NEW."+column)
				} else {
					conditions = append(conditions, "OLD."+column+" IS NOT NEW."+column)
				}
			}
			condition := strings.Join(conditions, " OR ")
			if op == "DELETE" {
				condition = ""
			}
			if db.Name() == "postgres" {
				action := update
				if condition != "" {
					action = "IF " + condition + " THEN " + update + " END IF;"
				}
				if err := db.Exec("CREATE OR REPLACE FUNCTION " + name + "_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN " + action + " RETURN OLD; END $$").Error; err != nil {
					return err
				}
				if err := db.Exec("DROP TRIGGER IF EXISTS " + name + " ON " + table.name).Error; err != nil {
					return err
				}
				if err := db.Exec("CREATE TRIGGER " + name + " AFTER " + op + " ON " + table.name + " FOR EACH ROW EXECUTE FUNCTION " + name + "_fn()").Error; err != nil {
					return err
				}
			} else {
				if condition != "" {
					condition = " WHEN " + condition
				}
				sql := fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS %s AFTER %s ON %s FOR EACH ROW%s BEGIN %s END", name, op, table.name, condition, update)
				if err := db.Exec(sql).Error; err != nil {
					return err
				}
			}
		}
	}
	return nil
}
