//go:build !linux

package main

import "fmt"

func hostCommand(_ []string) error {
	return fmt.Errorf("host command requires a Linux guest")
}
