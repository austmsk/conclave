package gate

import (
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

func TestSelectMode(t *testing.T) {
	const (
		clean = contracts.GatingCleanRoom
		reuse = contracts.GatingResetInPlace
	)
	cases := []struct {
		name                  string
		tier                  contracts.SensitivityTier
		mode                  contracts.GatingMode
		inPlace, force, flags bool
		want                  contracts.GatingMode
		wantErr               bool
	}{
		{name: "public reuses", tier: contracts.TierPublic, inPlace: true, want: reuse},
		{name: "private reuses", tier: contracts.TierPrivate, inPlace: true, want: reuse},
		{name: "policy reset_in_place reuses", tier: contracts.TierPublic, mode: reuse, inPlace: true, want: reuse},
		{name: "no agent sandbox", tier: contracts.TierPublic, want: clean},
		{name: "forced", tier: contracts.TierPublic, inPlace: true, force: true, want: clean},
		{name: "policy clean_room", tier: contracts.TierPublic, mode: clean, inPlace: true, want: clean},
		{name: "sensitive", tier: contracts.TierSensitive, inPlace: true, want: clean},
		{name: "local_only", tier: contracts.TierLocalOnly, inPlace: true, want: clean},
		{name: "flagged", tier: contracts.TierPrivate, inPlace: true, flags: true, want: clean},
		{name: "empty tier", tier: "", inPlace: true, wantErr: true},
		{name: "unknown tier", tier: "secret", inPlace: true, wantErr: true},
		{name: "empty tier even when clean anyway", tier: "", force: true, wantErr: true},
		{name: "unknown policy mode", tier: contracts.TierPublic, mode: "yolo", inPlace: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectMode(tc.tier, Policy{Mode: tc.mode}, tc.inPlace, tc.force, tc.flags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestSelectMode_FlagForcesCleanRoom checks each flag-only gate's flag,
// produced by the gate itself, moves gating to clean_room for a tier and
// policy that would otherwise reuse the agent's sandbox.
func TestSelectMode_FlagForcesCleanRoom(t *testing.T) {
	p := testPolicy(t)
	surface := checkExecSurface([]FileChange{{Kind: ChangeModified, OldPath: ".claude/settings.json", NewPath: ".claude/settings.json", Added: []string{"{}"}}}, p)
	tools := checkToolCalls(3, p)
	for _, v := range []verdict{surface, tools} {
		t.Run(string(v.Gate), func(t *testing.T) {
			if !v.Passed || !v.Flagged {
				t.Fatalf("gate did not pass with a flag: %+v", v)
			}
			got, err := SelectMode(contracts.TierPublic, p, true, false, v.Flagged)
			if err != nil || got != contracts.GatingCleanRoom {
				t.Fatalf("got %q, %v; want clean_room", got, err)
			}
		})
	}
	got, err := SelectMode(contracts.TierPublic, p, true, false, false)
	if err != nil || got != contracts.GatingResetInPlace {
		t.Fatalf("unflagged: got %q, %v; want reset_in_place", got, err)
	}
}
