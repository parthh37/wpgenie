package api

import (
	"fmt"
	"strings"
	"testing"
)

func TestBurstAPI(t *testing.T) {
	e := newTenancyEnv(t)
	var out map[string]any
	// Burst is a plan feature (Basic doesn't have it), within the plan.
	if c := e.as("alice", "PUT", "/api/v1/sites/sa/burst", `{"mode":"auto"}`, &out); c != 403 || !strings.Contains(fmt.Sprint(out["error"]), "burst") {
		t.Errorf("burst without the feature: %d %v", c, out)
	}
	if c := e.as("bob", "PUT", "/api/v1/sites/sb/burst", `{"mode":"auto","base":5}`, &out); c != 403 || !strings.Contains(fmt.Sprint(out["error"]), "plan") {
		t.Errorf("a normal size beyond the plan: %d %v", c, out)
	}
	if c := e.as("bob", "PUT", "/api/v1/sites/sb/burst", `{"mode":"turbo"}`, &out); c != 400 {
		t.Errorf("unknown mode: %d %v", c, out)
	}

	// Anyone who sees the site sees its burst and what its account has left.
	var view struct {
		Mode    string `json:"mode"`
		Base    int    `json:"base"`
		Account *struct {
			Unlimited bool  `json:"unlimited"`
			Credit    int64 `json:"credit"`
			Allowed   bool  `json:"allowed"`
		} `json:"account"`
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sa/burst", "", &view); c != 200 || view.Mode != "off" || view.Base != 1 ||
		view.Account == nil || view.Account.Allowed || !view.Account.Unlimited {
		t.Fatalf("alice's burst: %d %+v %+v", c, view, view.Account)
	}
	if c := e.as("tok", "GET", "/api/v1/sites/sx/burst", "", &view); c != 200 || view.Account != nil {
		t.Fatalf("a staff site has no account: %d %+v", c, view.Account)
	}

	// Minutes are bought through the provisioning API only.
	path := fmt.Sprintf("/api/v1/accounts/%d/burst-credit", e.acct["B"].ID)
	if c := e.as("bob", "POST", path, `{"minutes":100}`, nil); c != 403 {
		t.Errorf("a customer adding minutes: %d", c)
	}
	var bal struct {
		Credit    int64 `json:"credit"`
		Remaining int64 `json:"remaining"`
	}
	if c := e.as("tok", "POST", path, `{"minutes":100}`, &bal); c != 200 || bal.Credit != 100 {
		t.Fatalf("adding minutes: %d %+v", c, bal)
	}
	if c := e.as("tok", "POST", path, `{"minutes":0}`, nil); c != 400 {
		t.Errorf("no minutes: %d", c)
	}
	if c := e.as("bob", "GET", fmt.Sprintf("/api/v1/accounts/%d/burst", e.acct["B"].ID), "", &bal); c != 200 || bal.Credit != 100 {
		t.Fatalf("bob's balance: %d %+v", c, bal)
	}
	if c := e.as("alice", "GET", fmt.Sprintf("/api/v1/accounts/%d/burst", e.acct["B"].ID), "", nil); c != 404 {
		t.Errorf("someone else's balance: %d", c)
	}

	// Protection levels and automatic Under attack periods.
	var levels []map[string]any
	if c := e.as("alice", "GET", "/api/v1/security/levels", "", &levels); c != 200 || len(levels) != 3 || levels[1]["id"] != "recommended" {
		t.Fatalf("levels: %d %v", c, levels)
	}
	if c := e.as("alice", "GET", "/api/v1/sites/sa/attack", "", &out); c != 200 || out["active"] != false {
		t.Fatalf("attack: %d %v", c, out)
	}
	if c := e.as("alice", "DELETE", "/api/v1/sites/sa/attack", "", nil); c != 404 {
		t.Errorf("ending no attack: %d", c)
	}
	if c := e.as("alice", "PUT", "/api/v1/sites/sa/shield", `{"mode":"auto","level":"paranoid"}`, &out); c != 400 {
		t.Fatalf("unknown level: %d %v", c, out)
	}
}
