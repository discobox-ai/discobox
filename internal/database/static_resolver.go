package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// StaticResolver adapts a single DB to the tenant resolver interface. It is
// useful for tests and single-tenant embedded wiring.
type StaticResolver struct {
	DB *DB
}

func (r StaticResolver) ResolveTenantDB(context.Context, string) (write, read *gorm.DB, err error) {
	if r.DB == nil {
		return nil, nil, fmt.Errorf("static database is nil")
	}
	read = r.DB.Read
	if read == nil {
		read = r.DB.Write
	}
	return r.DB.Write, read, nil
}
