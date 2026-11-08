package main

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The schema is internal interface (ADR 0006, #177): during an update the API pod of the previous release still serves
// next to the new one, against the schema the new release applied. So a release only expands the schema: new tables,
// new columns that are nullable or have a default, new indexes. A drop, a rename, a type change or a new NOT NULL
// column without a default comes one MINOR release after the code that stops using it, and then the baseline below is
// edited in the same PR (which the review sees).
//
// testdata/schema-baseline.txt lists the table.column pairs the previous release had; each must still be created.

var (
	createTableRe = regexp.MustCompile(`(?i)^CREATE TABLE IF NOT EXISTS ([a-z_]+) \(`)
	columnRe      = regexp.MustCompile(`^\s+([a-z_]+)\s+[a-z]`)
	addColumnRe   = regexp.MustCompile(`(?i)^ALTER TABLE ([a-z_]+) ADD COLUMN IF NOT EXISTS ([a-z_]+) (.*);`)
	contractRe    = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|INDEX|VIEW)|RENAME\s+(TO|COLUMN)|ALTER\s+COLUMN\s+[a-z_]+\s+(TYPE|SET\s+NOT\s+NULL)|TRUNCATE)\b`)
)

func schemaColumns(t *testing.T) map[string]bool {
	t.Helper()
	cols := map[string]bool{}
	table := ""
	for _, line := range strings.Split(schemaSQL, "\n") {
		if m := createTableRe.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		if table != "" && strings.HasPrefix(strings.TrimSpace(line), ")") {
			table = ""
			continue
		}
		if table != "" {
			if m := columnRe.FindStringSubmatch(line); m != nil {
				cols[table+"."+m[1]] = true
			}
		}
		if m := addColumnRe.FindStringSubmatch(line); m != nil {
			cols[m[1]+"."+m[2]] = true
			def := strings.ToUpper(m[3])
			if strings.Contains(def, "NOT NULL") && !strings.Contains(def, "DEFAULT") {
				t.Errorf("added column %s.%s is NOT NULL without a default: the previous API cannot insert rows", m[1], m[2])
			}
		}
	}
	return cols
}

func TestSchemaIsExpandOnly(t *testing.T) {
	for i, line := range strings.Split(schemaSQL, "\n") {
		code, _, _ := strings.Cut(line, "--")
		if contractRe.MatchString(code) {
			t.Errorf("schema.sql:%d contracts the schema (%q): only one MINOR release after the code stops using it (ADR 0006)", i+1, strings.TrimSpace(code))
		}
	}
	cols := schemaColumns(t)
	f, err := os.Open("testdata/schema-baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		c := strings.TrimSpace(sc.Text())
		if c == "" || strings.HasPrefix(c, "#") {
			continue
		}
		if !cols[c] {
			t.Errorf("column %s of the previous release is gone: the previous API pod still uses it during an update", c)
		}
	}
}
