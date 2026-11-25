package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	ctx, _ := strconv.Atoi(os.Args[3])
	needle := []byte(os.Args[2])
	seen := map[string]bool{}
	n := 0
	for off := 0; n < 15; {
		i := bytes.Index(b[off:], needle)
		if i < 0 {
			break
		}
		i += off
		lo, hi := i-ctx, i+len(needle)+ctx
		if lo < 0 {
			lo = 0
		}
		if hi > len(b) {
			hi = len(b)
		}
		s := string(b[lo:hi])
		if !seen[s] {
			seen[s] = true
			n++
			fmt.Printf("%q\n\n", s)
		}
		off = i + len(needle)
	}
}
