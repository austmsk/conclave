package gate

import (
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

func TestCheckToolCalls(t *testing.T) {
	cases := []struct {
		name       string
		rejections int
		at         int
		flagged    bool
	}{
		{"none", 0, 0, false},
		{"below default", 2, 0, false},
		{"at default", 3, 0, true},
		{"above default", 10, 0, true},
		{"below configured", 4, 5, false},
		{"at configured", 5, 5, true},
		{"configured one", 1, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkToolCalls(tc.rejections, Policy{FlagRejectionsAt: tc.at})
			if v.Gate != contracts.GateToolCalls || !v.Passed || v.Flagged != tc.flagged {
				t.Fatalf("got %+v, want passed and flagged=%v", v, tc.flagged)
			}
		})
	}
}
