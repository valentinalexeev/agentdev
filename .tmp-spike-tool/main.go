package main

import (
	"fmt"
	"os"
	"regexp"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	re := regexp.MustCompile(os.Args[2])
	seen := map[string]bool{}
	for _, m := range re.FindAll(b, 60) {
		s := string(m)
		if !seen[s] {
			seen[s] = true
			fmt.Printf("%q\n", s)
		}
	}
}
