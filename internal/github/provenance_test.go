package github_test

import (
	"testing"

	"github.com/austmsk/conclave/internal/github"
)

const (
	provenanceOwner = int64(42)
	provenanceApp   = int64(7)
)

var classifyCases = []struct {
	name    string
	a       github.Author
	ownerID int64
	want    github.Source
}{
	{"owner user", github.Author{ID: 42, Type: "User"}, provenanceOwner, github.SourceOwner},
	{"owner id typed Bot", github.Author{ID: 42, Type: "Bot"}, provenanceOwner, github.SourceBot},
	{"owner acting through another App", github.Author{ID: 42, Type: "User", AppID: 99}, provenanceOwner, github.SourceBot},
	{"owner acting through this App", github.Author{ID: 42, Type: "User", AppID: 7}, provenanceOwner, github.SourcePlatform},
	{"this App's bot", github.Author{ID: 1001, Type: "Bot", AppID: 7}, provenanceOwner, github.SourcePlatform},
	{"another App's bot", github.Author{ID: 1002, Type: "Bot", AppID: 99}, provenanceOwner, github.SourceBot},
	{"bot account without an App", github.Author{ID: 1003, Type: "Bot"}, provenanceOwner, github.SourceBot},
	{"stranger user", github.Author{ID: 500, Type: "User"}, provenanceOwner, github.SourceStranger},
	{"organization", github.Author{ID: 600, Type: "Organization"}, provenanceOwner, github.SourceStranger},
	{"mannequin", github.Author{ID: 700, Type: "Mannequin"}, provenanceOwner, github.SourceStranger},
	{"owner id with empty type", github.Author{ID: 42}, provenanceOwner, github.SourceStranger},
	{"owner id with lowercase type", github.Author{ID: 42, Type: "user"}, provenanceOwner, github.SourceStranger},
	{"unset owner never matches id 0", github.Author{ID: 0, Type: "User"}, 0, github.SourceStranger},
	{"negative owner never matches", github.Author{ID: -1, Type: "User"}, -1, github.SourceStranger},
}

func TestClassifyProvenance(t *testing.T) {
	for _, tc := range classifyCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := github.Classify(tc.a, tc.ownerID, provenanceApp); got != tc.want {
				t.Errorf("Classify(%+v, owner %d, app %d) = %s, want %s", tc.a, tc.ownerID, provenanceApp, got, tc.want)
			}
		})
	}
}

func TestClassifyUnsetAppIDNeverPlatform(t *testing.T) {
	a := github.Author{ID: 1001, Type: "Bot"}
	if got := github.Classify(a, provenanceOwner, 0); got != github.SourceBot {
		t.Errorf("Classify with app ID 0 = %s, want bot", got)
	}
}
