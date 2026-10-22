// Command fakediscobox stands in for a released discobox CLI in the installer
// tests. It answers the two things an installer asks of the binary it has just
// put in place: what version it is, and — for --stage — staging the server.
package main

import (
	"fmt"
	"os"
	"strings"
)

// version is set with -ldflags -X, as a release sets the real one.
var version = ""

func main() {
	switch strings.Join(os.Args[1:], " ") {
	case "--version":
		fmt.Printf("discobox version %s\n", version)
	case "admin server stage":
		// What a cold mirror or a full disk does to the download the real
		// command makes.
		if os.Getenv("DISCOBOX_FAKE_STAGE_FAILS") != "" {
			fmt.Fprintln(os.Stderr, "staging the server failed")
			os.Exit(1)
		}
		fmt.Println("staged the server for " + version)
	default:
		os.Exit(2)
	}
}
