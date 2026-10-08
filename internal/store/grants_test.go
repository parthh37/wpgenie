package store

import (
	"context"
	"errors"
	"testing"
)

func TestSiteGrants(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if err := s.CreatePlan(ctx, &Plan{ID: "p", Name: "P", Overage: "notify"}); err != nil {
			t.Fatal(err)
		}
		acct := func(name, user string) (*Account, *User) {
			a, u, _, err := s.CreateAccount(ctx, &Account{Name: name, Kind: AccountCustomer, PlanID: "p"}, "",
				&NewUser{Username: user, PasswordHash: "h", Role: "customer"})
			if err != nil {
				t.Fatal(err)
			}
			return a, u
		}
		a, _ := acct("A", "ann")
		b, bob := acct("B", "bob")
		_, cat := acct("C", "cat")
		for i, id := range []string{"s1", "s2", "s3"} {
			if err := s.CreateSite(ctx, &Site{ID: id, Name: id, PrimaryDomain: id + ".test", PHPVersion: "8.3",
				FPMPort: 19500 + i, DBName: "wp_" + id, Status: StatusActive, ShieldMode: "standard"}); err != nil {
				t.Fatal(err)
			}
			if err := s.AssignSite(ctx, id, a.ID); err != nil {
				t.Fatal(err)
			}
		}
		grant := func(site string, u *User, access string) error {
			return s.CreateSiteGrant(ctx, &SiteGrant{SiteID: site, UserID: u.ID, Access: access, GrantedBy: "ann"}, 2)
		}
		for _, g := range []struct {
			site string
			u    *User
		}{{"s1", bob}, {"s1", cat}, {"s2", bob}, {"s3", bob}} {
			if err := grant(g.site, g.u, "viewer"); err != nil {
				t.Fatal(err)
			}
		}
		if err := grant("s1", bob, "manager"); !errors.Is(err, ErrExists) {
			t.Fatalf("shared twice: %v", err)
		}
		if err := s.CreateSiteGrant(ctx, &SiteGrant{SiteID: "s1", UserID: 999, Access: "viewer"}, 9); err == nil {
			t.Fatal("shared with a user that doesn't exist")
		}
		// At most max per site.
		if _, dan := acct("D", "dan"); !errors.Is(grant("s1", dan, "viewer"), ErrConflict) {
			t.Fatal("more grants than the limit")
		}

		g, err := s.SiteGrant(ctx, "s1", bob.ID)
		if err != nil || g.Username != "bob" || g.Access != "viewer" || g.GrantedBy != "ann" || g.CreatedAt.IsZero() {
			t.Fatalf("grant %+v %v", g, err)
		}
		if err := s.SetSiteGrantAccess(ctx, "s1", bob.ID, "developer"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSiteGrantAccess(ctx, "s9", bob.ID, "developer"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("changed a grant that doesn't exist: %v", err)
		}
		list, err := s.SiteGrants(ctx, "s1")
		if err != nil || len(list) != 2 || list[0].Username != "bob" || list[0].Access != "developer" || list[1].Username != "cat" {
			t.Fatalf("s1's grants %+v %v", list, err)
		}
		mine, err := s.UserSiteGrants(ctx, bob.ID)
		if err != nil || len(mine) != 3 || mine[0].SiteID != "s1" || mine[0].AccountID != a.ID {
			t.Fatalf("bob's grants %+v %v", mine, err)
		}
		if n, err := s.SiteGrantCounts(ctx); err != nil || len(n) != 3 || n["s1"] != 2 || n["s2"] != 1 || n["s3"] != 1 {
			t.Fatalf("grant counts %v %v", n, err)
		}

		// Reassigning to the same account keeps grants; to another clears them.
		if err := s.AssignSite(ctx, "s1", a.ID); err != nil {
			t.Fatal(err)
		}
		if l, _ := s.SiteGrants(ctx, "s1"); len(l) != 2 {
			t.Fatalf("same owner dropped grants: %+v", l)
		}
		if err := s.AssignSite(ctx, "s1", b.ID); err != nil {
			t.Fatal(err)
		}
		if l, _ := s.SiteGrants(ctx, "s1"); len(l) != 0 {
			t.Fatalf("a site given to another account kept its grants: %+v", l)
		}
		// Made staff-only, or deleted: gone too.
		if err := s.UnassignSite(ctx, "s2"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSite(ctx, "s3"); err != nil {
			t.Fatal(err)
		}
		if mine, _ := s.UserSiteGrants(ctx, bob.ID); len(mine) != 0 {
			t.Fatalf("grants outlived their sites' ownership: %+v", mine)
		}

		// Deleting the user deletes theirs.
		if err := grant("s1", cat, "viewer"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteUser(ctx, cat.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SiteGrant(ctx, "s1", cat.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a deleted user's grant: %v", err)
		}
		if err := s.DeleteSiteGrant(ctx, "s1", cat.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleting a missing grant: %v", err)
		}
	})
}
