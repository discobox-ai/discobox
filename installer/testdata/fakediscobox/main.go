// Command fakediscobox stands in for a released discobox CLI in the installer
// tests. It answers --version the way the real one does, which is the one
// thing an installer asks of the binary it has just put in place.
package main

import (
	"fmt"
	"os"
)

// version is set with -ldflags -X, as a release sets the real one.
var version = ""

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("discobox version %s\n", version)
		return
	}
	os.Exit(2)
}
