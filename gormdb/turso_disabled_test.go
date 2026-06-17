//go:build !turso

package gormdb_test

import (
	"strings"
	"testing"

	"github.com/obot-platform/discobox/gormdb"
)

func TestOpenTursoWithoutBuildTagReturnsHelpfulError(t *testing.T) {
	_, err := gormdb.Open(gormdb.Config{Driver: gormdb.DriverTurso, DSN: ":memory:"})
	if err == nil {
		t.Fatal("expected turso build tag error")
	}
	if !strings.Contains(err.Error(), "requires building with -tags turso") {
		t.Fatalf("error = %v, want build tag guidance", err)
	}
}
