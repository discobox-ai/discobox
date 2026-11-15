package secrets_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// postgresSchemaService is a secrets service on a schema of its own in the
// Postgres DISCOBOX_TEST_POSTGRES_DSN names, dropped when the test ends, and
// a handle that reads pg_stat_activity.
func postgresSchemaService(t *testing.T, name string) (*resourcesecrets.Service, *store.Store, *database.DB) {
	t.Helper()
	dsn := os.Getenv("DISCOBOX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DISCOBOX_TEST_POSTGRES_DSN for real row-lock races")
	}
	admin, err := database.New(database.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("%s_%d", name, time.Now().UnixNano())
	if err := admin.Write.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Write.Exec("DROP SCHEMA " + schema + " CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	svc, st := newAgentCredentialService(t, dsn+sep+"search_path="+schema)
	return svc, st, admin
}

// waitForLockWaiter reports whether a query containing text is seen waiting
// on a lock within five seconds.
func waitForLockWaiter(t *testing.T, admin *database.DB, text string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int64
		if err := admin.Read.Raw("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE ?", "%"+text+"%").Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
