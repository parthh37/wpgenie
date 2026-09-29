package site

import (
	"context"
	"errors"
	"testing"

	"github.com/parthh37/wpgenie/internal/proxy"
	"github.com/parthh37/wpgenie/internal/shield"
)

func ptr[T any](v T) *T { return &v }

func TestSetShieldSettingsReachShieldAndProxy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, err := h.svc.SetShield(ctx, "s1", ShieldInput{Mode: shield.ModeStandard, BlockAIBots: true,
		DenyIPs: ptr([]string{"198.51.100.7", "2001:db8::/48"}), XMLRPC: ptr(true),
		RateRPS: ptr(20.0), RateBurst: ptr(100), LoginPerMin: ptr(3.0), ChallengeBits: ptr(18),
		Reputation: ptr("block"), CountryMode: ptr("allow"), Countries: ptr([]string{"de", " FR", "DE"}),
		CountryAction: ptr("challenge"), BodyWAF: ptr("detect")})
	if err != nil {
		t.Fatal(err)
	}
	if st.RateRPS != 20 || st.ChallengeBits != 18 || len(st.DenyIPs) != 2 || st.DenyIPs[0] != "198.51.100.7/32" ||
		len(st.Countries) != 2 || st.Countries[0] != "DE" || !st.XMLRPC || st.BodyWAF != "detect" {
		t.Fatalf("stored %+v", st)
	}
	ss, ok := h.svc.ShieldLookup("s1")
	if !ok || ss.RequestsPerSecond != 20 || ss.Burst != 100 || ss.LoginPerMinute != 3 || ss.Difficulty != 18 ||
		ss.Reputation != shield.Block || ss.CountryAction != shield.Challenge || !ss.CountryAllow ||
		!ss.Countries["FR"] || len(ss.Deny) != 2 {
		t.Fatalf("shield settings %+v", ss)
	}
	if !h.svc.CountryRulesInUse() {
		t.Error("country rules in use not reported")
	}
	last := h.proxy.last[0]
	if last.BlockXMLRPC || last.BodyWAF != proxy.WAFDetect {
		t.Errorf("proxy site %+v", last)
	}
	// Older clients that only send mode and AI blocking keep everything else.
	st, err = h.svc.SetShield(ctx, "s1", ShieldInput{Mode: shield.ModeUnderAttack, BlockAIBots: false})
	if err != nil || st.RateRPS != 20 || st.BodyWAF != "detect" || st.CountryMode != "allow" || !st.XMLRPC {
		t.Fatalf("partial update: %+v %v", st, err)
	}
	// 0 restores the defaults; turning country rules off keeps the list.
	st, _ = h.svc.SetShield(ctx, "s1", ShieldInput{Mode: shield.ModeStandard, RateRPS: ptr(0.0), CountryMode: ptr("off")})
	if ss, _ := h.svc.ShieldLookup("s1"); st.RateRPS != 0 || ss.CountryAction != shield.Allow || len(st.Countries) != 2 {
		t.Errorf("reset: %+v %+v", st, ss)
	}
}

func TestSetShieldValidation(t *testing.T) {
	h := newHarness(t)
	for name, in := range map[string]ShieldInput{
		"rate too high":        {RateRPS: ptr(5000.0)},
		"negative burst":       {RateBurst: ptr(-1)},
		"login too low":        {LoginPerMin: ptr(0.1)},
		"difficulty too high":  {ChallengeBits: ptr(30)},
		"difficulty too low":   {ChallengeBits: ptr(4)},
		"reputation":           {Reputation: ptr("maybe")},
		"body waf":             {BodyWAF: ptr("on")},
		"country mode":         {CountryMode: ptr("only")},
		"country action":       {CountryAction: ptr("allow")},
		"country code":         {Countries: ptr([]string{"Germany"})},
		"rules without a list": {CountryMode: ptr("block")},
		"deny everything":      {DenyIPs: ptr([]string{"0.0.0.0/0"})},
		"deny garbage":         {DenyIPs: ptr([]string{"not-an-ip"})},
	} {
		in.Mode = shield.ModeStandard
		if _, err := h.svc.SetShield(context.Background(), "s1", in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGlobalLists(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g, err := h.svc.SetGlobalLists(ctx, GlobalLists{Allow: []string{"192.0.2.10"}, Deny: []string{"203.0.113.0/24", "203.0.113.0/24"}})
	if err != nil || len(g.Deny) != 1 || g.Allow[0] != "192.0.2.10/32" {
		t.Fatalf("%+v %v", g, err)
	}
	got, _ := h.svc.GlobalLists(ctx)
	if len(got.Allow) != 1 || len(got.Deny) != 1 || len(got.Shield().Deny) != 1 {
		t.Errorf("reloaded %+v", got)
	}
	if _, err := h.svc.SetGlobalLists(ctx, GlobalLists{Deny: []string{"::/0"}}); !errors.Is(err, ErrInvalidInput) {
		t.Error("a deny list covering everything was accepted")
	}
}

func TestCreateRejectsMultiLineNames(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.svc.Create(context.Background(), CreateInput{Domain: "new.test", AdminEmail: "a@b.co", Name: "x\n}\nevil {"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("multi-line name: %v", err)
	}
}
