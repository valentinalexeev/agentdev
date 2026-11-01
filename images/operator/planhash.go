package main

// Plan file helpers (#69). The operator image runs them inside executor Jobs: hash-plan after the tool wrote the plan
// (the hash the person approves is computed by this code, not by the tool), verify-plan before an apply (any other
// file never reaches the tool). No shell, no network, read-only mount.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// planHelper handles "hash-plan <file>" and "verify-plan <file> <sha256>". ok=false: not a helper invocation.
func planHelper(args []string, out io.Writer) (code int, ok bool) {
	if len(args) < 1 {
		return 0, false
	}
	switch args[0] {
	case "hash-plan":
		if len(args) != 2 {
			fmt.Fprintln(out, "usage: hash-plan <file>")
			return 2, true
		}
		h, err := fileSHA256(args[1])
		if err != nil {
			fmt.Fprintln(out, "hash-plan:", err)
			return 1, true
		}
		fmt.Fprintf(out, "%s%s\n", planHashPrefix, h)
		return 0, true
	case "verify-plan":
		if len(args) != 3 || !hashRE.MatchString(args[2]) {
			fmt.Fprintln(out, "usage: verify-plan <file> <sha256>")
			return 2, true
		}
		h, err := fileSHA256(args[1])
		if err != nil {
			fmt.Fprintln(out, "verify-plan:", err)
			return 1, true
		}
		if h != args[2] {
			fmt.Fprintf(out, "verify-plan: plan file sha256 %s is not the approved %s\n", h, args[2])
			return 1, true
		}
		fmt.Fprintf(out, "verify-plan: plan file matches the approved sha256 %s\n", h)
		return 0, true
	}
	return 0, false
}
