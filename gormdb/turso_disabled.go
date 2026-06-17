//go:build !turso

package gormdb

import "fmt"

func openTurso(string, Config) (*Pools, error) {
	return nil, fmt.Errorf("turso database support requires building with -tags turso")
}
