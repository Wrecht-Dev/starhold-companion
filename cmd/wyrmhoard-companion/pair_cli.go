package main

import (
	"fmt"
	"os"
	"strings"
)

// runPairCLI handles the terminal fallback:
//
//	wyrmhoard-companion pair CODE --server https://server
//
// It returns true when the CLI path was selected (including failure), so main
// does not fall through to native-messaging stdin handling.
func runPairCLI() bool {
	if len(os.Args) <= 1 || os.Args[1] != "pair" {
		return false
	}
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: wyrmhoard-companion pair CODE --server https://your-server")
		os.Exit(2)
	}
	code := strings.TrimSpace(os.Args[2])
	server := os.Getenv("WYRMHOARD_SERVER")
	for i := 3; i < len(os.Args)-1; i++ {
		if os.Args[i] == "--server" {
			server = os.Args[i+1]
		}
	}
	r := pair(request{Type: "pair", Code: code, Server: server})
	if !r.OK {
		fmt.Fprintln(os.Stderr, "pairing failed:", r.Error)
		fmt.Fprintln(os.Stderr, "usage: wyrmhoard-companion pair CODE --server https://your-server")
		os.Exit(1)
	}
	fmt.Printf("paired device %s with %s\n", r.Device, r.Server)
	return true
}
