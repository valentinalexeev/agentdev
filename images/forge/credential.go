package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// credentialHelper is git's credential helper (`forge credential get`), run by the sidecar's own git only. It
// answers with the token from the sidecar's environment, and only for exactly the forge's scheme and host, so a
// redirect or another remote never receives it.
func credentialHelper(args []string, in io.Reader, out io.Writer) int {
	if len(args) == 0 || args[0] != "get" {
		return 0
	}
	scheme, host, err := forgeOrigin(strings.TrimRight(os.Getenv("FORGE_URL"), "/"))
	if err != nil {
		return 0
	}
	var proto, h string
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "protocol":
			proto = v
		case "host":
			h = v
		}
	}
	// the sidecar's git child gets the driver's credential (FORGE_GIT_*, a short-lived token on GitHub); a helper
	// started without it falls back to the Gitea token of the sidecar's environment
	user, pass := os.Getenv("FORGE_GIT_USER"), os.Getenv("FORGE_GIT_PASSWORD")
	if pass == "" {
		user, pass = os.Getenv("FORGE_USER"), os.Getenv("FORGE_TOKEN")
	}
	if proto != scheme || h != host || pass == "" || strings.ContainsAny(user+pass, "\n\r\x00") {
		return 0
	}
	fmt.Fprintf(out, "username=%s\npassword=%s\n", user, pass)
	return 0
}
