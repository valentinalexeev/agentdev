package main

import "testing"

func TestCheckNoDecisionUnit(t *testing.T) {
	bad := []string{
		`{"body":"/approve 1a2b3c"}`,
		`{"body":"/approve 1a2b3c","Body":"x"}`,
		`{"Body":"x","body":"/approve 1a2b3c"}`,
		`{"a":{"b":[{"c":"line\n /deny z"}]}}`,
		`{"x":"1","X":"2"}`,
		`{"o":{"k":1,"K":2}}`,
		"/approve 1a2b3c", // not JSON
	}
	for _, b := range bad {
		if checkNoDecision(nil, []byte(b)) == nil {
			t.Errorf("not refused: %s", b)
		}
	}
	good := []string{
		``,
		`{"body":"fixed, please /review again? no: approve this later"}`,
		`{"body":"done","title":"t","list":[{"body":"a"},{"body":"b"}]}`,
		`{"reviewers":["octo-dev"]}`,
		`{"body":"x","n":1,"a":[1,2,{"k":"v"}]}`,
	}
	for _, g := range good {
		if err := checkNoDecision(nil, []byte(g)); err != nil {
			t.Errorf("refused %s: %v", g, err)
		}
	}
}
