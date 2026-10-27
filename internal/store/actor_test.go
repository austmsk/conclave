package store_test

import (
	"testing"

	"github.com/austmsk/conclave/internal/store"
)

func TestActorStrings(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"workflow", store.ActorWorkflow("feature-austmsk/election-tally-12"), "workflow:feature-austmsk/election-tally-12"},
		{"github user", store.ActorGitHub(583231), "github:583231"},
		{"worker", store.ActorWorker, "worker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}
