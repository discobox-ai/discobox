package database

import (
	"bytes"
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/x/gormdb"
)

// TestNewLogsNoBoundValues checks that both handles New opens log a
// statement without its bound values, whatever the statement writes.
func TestNewLogsNoBoundValues(t *testing.T) {
	db := openTestDB(t)
	for name, handle := range map[string]*gorm.DB{"write": db.Write, "read": db.Read} {
		filter, ok := handle.Logger.(gorm.ParamsFilter)
		if !ok {
			t.Fatalf("%s handle's logger %T does not filter bound values", name, handle.Logger)
		}
		_, vars := filter.ParamsFilter(context.Background(), "INSERT INTO t (v) VALUES (?)", "secret")
		if len(vars) != 0 {
			t.Fatalf("%s handle's logger keeps bound values %v; a slow or failed statement would log them", name, vars)
		}
	}
}

// TestSQLLogCarriesNoKeyMaterial writes, as slow statements and as a failed
// one, the rows that hold key material with no encryption provider set, and
// checks the log names the statements but none of the values.
func TestSQLLogCarriesNoKeyMaterial(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var out bytes.Buffer
	// Every statement is slow: each one reaches the log.
	tx := db.Write.Session(&gorm.Session{Logger: newSQLLogger(&out, time.Nanosecond)})

	privateKey := []byte("PRIVATE-KEY-MATERIAL-0123456789")
	secrets := []string{
		string(privateKey),
		// Without an encryption provider a sealed value is the plaintext
		// in base64; that is the form the issue found in the log.
		base64.StdEncoding.EncodeToString(privateKey),
		"PUBLIC-KEY-0123456789",
		"ROTATED-PRIVATE-KEY-9876543210",
	}

	state := model.ServerState{
		Key:   "worker_agent_request_issuer",
		Value: model.RawJSON(`{"publicKey":"PUBLIC-KEY-0123456789","encryptedPrivateKey":"` + base64.StdEncoding.EncodeToString(privateKey) + `"}`),
	}
	if err := tx.Create(&state).Error; err != nil {
		t.Fatalf("insert server state: %v", err)
	}
	if err := tx.Model(&model.ServerState{}).Where("key = ?", state.Key).
		Update("value", model.RawJSON(`{"encryptedPrivateKey":"ROTATED-PRIVATE-KEY-9876543210"}`)).Error; err != nil {
		t.Fatalf("update server state: %v", err)
	}
	// The same key again fails: an error is logged with its statement too.
	if err := tx.Create(&state).Error; err == nil {
		t.Fatalf("second insert of server state %q succeeded, want a conflict", state.Key)
	}
	// Scan logs through GORM's recorder, not the logger's own filter.
	var key string
	if err := tx.Raw("SELECT key FROM server_state WHERE value <> ?", "PUBLIC-KEY-0123456789").Scan(&key).Error; err != nil {
		t.Fatalf("scan server state: %v", err)
	}
	if err := tx.Raw("SELECT nope FROM server_state WHERE value = ?", "ROTATED-PRIVATE-KEY-9876543210").Scan(&key).Error; err == nil {
		t.Fatalf("scan of a missing column succeeded, want an error")
	}

	logged := out.String()
	if !strings.Contains(logged, "SLOW SQL") || !strings.Contains(logged, "INSERT INTO `server_state`") ||
		!strings.Contains(logged, "SELECT key FROM server_state WHERE value <> ?") {
		t.Fatalf("log does not record the slow statements:\n%s", logged)
	}
	for _, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Fatalf("log carries %q:\n%s", secret, logged)
		}
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(Config{
		Driver: gormdb.DriverSQLite,
		DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})
	return db
}
