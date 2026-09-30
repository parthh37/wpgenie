package site

import (
	"context"
	"errors"
	"testing"

	"github.com/parthh37/wpgenie/internal/shield"
)

func TestProtectionLevels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// New sites' defaults are the recommended level.
	if got := LevelOf(newSite("n.test", "n")); got != LevelRecommended {
		t.Fatalf("new site: %s", got)
	}
	for _, l := range ProtectionLevels {
		st, err := h.svc.SetShield(ctx, "s1", ShieldInput{Mode: shield.ModeAuto, BlockAIBots: true, Level: l.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got := LevelOf(st); got != l.ID {
			t.Errorf("after applying %s: %s (%+v)", l.ID, got, st.ShieldSettings())
		}
	}
	// A level keeps what isn't about strictness, and explicit settings win.
	allow := []string{"203.0.113.7"}
	on := true
	if _, err := h.svc.SetShield(ctx, "s1", ShieldInput{Mode: shield.ModeAuto, TrustedIPs: &allow, XMLRPC: &on}); err != nil {
		t.Fatal(err)
	}
	bits := 20
	st, err := h.svc.SetShield(ctx, "s1", ShieldInput{Mode: shield.ModeStandard, Level: LevelBasic, ChallengeBits: &bits})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TrustedIPs) != 1 || !st.XMLRPC || st.ChallengeBits != 20 || st.Reputation != ReputationOff ||
		LevelOf(st) != LevelCustom || st.ShieldMode != "standard" {
		t.Fatalf("%+v", st.ShieldSettings())
	}
	if _, err := h.svc.SetShield(ctx, "s1", ShieldInput{Mode: shield.ModeAuto, Level: "paranoid"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown level: %v", err)
	}
}
