package mission

import "testing"

func TestOutcomeTruthTable(t *testing.T) {
	g := func(s string) CriterionResult { return CriterionResult{Status: s} }                     // guard
	e := func(s string) CriterionResult { return CriterionResult{Status: s, ProvesChange: true} } // evidence
	cases := []struct {
		name string
		in   []CriterionResult
		want string
	}{
		{"all pass", []CriterionResult{g(ResultPass), e(ResultPass)}, StatusSucceeded},
		{"only guards pass (nothing done)", []CriterionResult{g(ResultPass), e(ResultFail)}, StatusFailed},
		{"some evidence passes", []CriterionResult{e(ResultPass), e(ResultFail)}, StatusPartial},
		{"any error blocks", []CriterionResult{e(ResultPass), g(ResultError)}, StatusBlocked},
		{"no evidence criteria", []CriterionResult{g(ResultPass)}, StatusBlocked},
		{"empty", nil, StatusBlocked},
	}
	for _, c := range cases {
		if got := Outcome(c.in); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestContractRequiresChangeEvidence(t *testing.T) {
	raw := `{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},
	  "acceptance":[{"id":"tests","kind":"command","command":["go","test","./..."]}]}`
	if _, err := ParseContract([]byte(raw)); err == nil {
		t.Fatal("contract with only guard checks must be rejected")
	}
	raw = `{"version":1,"title":"t","objective":"o","agent":"a","workspace":{"source_dir":"x"},
	  "acceptance":[{"id":"report","kind":"file_contains","path":"REPORT.md","text":"Findings","must_change":true}]}`
	if _, err := ParseContract([]byte(raw)); err != nil {
		t.Fatalf("file_contains+must_change proves change: %v", err)
	}
}
