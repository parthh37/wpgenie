package cdn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRulesets is Cloudflare's rulesets API for one zone's cache phase.
type fakeRulesets struct {
	mu    sync.Mutex
	rs    *ruleset
	calls []string
	next  int
}

func (f *fakeRulesets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	ok := func(v any) {
		b, _ := json.Marshal(v)
		w.Write([]byte(`{"success":true,"errors":[],"result":` + string(b) + `}`))
	}
	var in rule
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, cachePhase):
		if f.rs == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"success":false,"errors":[{"code":10003,"message":"not found"}]}`))
			return
		}
		ok(f.rs)
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, cachePhase):
		var set struct{ Rules []rule }
		json.Unmarshal(body, &set)
		f.rs = &ruleset{ID: "rs1"}
		for _, x := range set.Rules {
			f.next++
			x.ID = "r" + string(rune('0'+f.next))
			f.rs.Rules = append(f.rs.Rules, x)
		}
		ok(f.rs)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rulesets/rs1/rules"):
		json.Unmarshal(body, &in)
		f.next++
		in.ID = "r" + string(rune('0'+f.next))
		f.rs.Rules = append(f.rs.Rules, in)
		ok(f.rs)
	case r.Method == http.MethodPatch || r.Method == http.MethodDelete:
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		json.Unmarshal(body, &in)
		for i, x := range f.rs.Rules {
			if x.ID == id {
				if r.Method == http.MethodDelete {
					f.rs.Rules = append(f.rs.Rules[:i], f.rs.Rules[i+1:]...)
				} else {
					in.ID = id
					f.rs.Rules[i] = in
				}
			}
		}
		ok(f.rs)
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"success":false,"errors":[{"code":1,"message":"unexpected"}]}`))
	}
}

func TestEdgeRuleLifecycle(t *testing.T) {
	f := &fakeRulesets{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	cf := &Cloudflare{BaseURL: srv.URL}
	ctx := context.Background()
	const tok = "abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	r := EdgeRule{Ref: "wpgenie_s1", Description: "WPGenie s1", Expression: EdgeExpression([]string{"a.test"}, []string{"wordpress_logged_in_"})}

	// A zone without cache rules gets the phase ruleset created.
	if err := cf.SetEdgeRule(ctx, tok, "z1", r); err != nil {
		t.Fatal(err)
	}
	if len(f.rs.Rules) != 1 || f.rs.Rules[0].ActionParameters["edge_ttl"].(map[string]any)["mode"] != "bypass_by_default" {
		t.Fatalf("rules %+v", f.rs.Rules)
	}
	// The owner's own rules stay; ours is added, then updated in place.
	f.rs.Rules = append([]rule{{ID: "mine", Description: "owner's rule", Expression: "true", Action: "set_cache_settings"}}, f.rs.Rules...)
	f.calls = nil
	if err := cf.SetEdgeRule(ctx, tok, "z1", r); err != nil || len(f.calls) != 1 {
		t.Fatalf("unchanged rule: %v, calls %v", err, f.calls)
	}
	r.Expression = EdgeExpression([]string{"a.test", "www.a.test"}, nil)
	if err := cf.SetEdgeRule(ctx, tok, "z1", r); err != nil {
		t.Fatal(err)
	}
	if len(f.rs.Rules) != 2 || f.rs.Rules[0].ID != "mine" || !strings.Contains(f.rs.Rules[1].Expression, "www.a.test") ||
		!strings.HasPrefix(f.calls[len(f.calls)-1], "PATCH") {
		t.Fatalf("after update: %+v, calls %v", f.rs.Rules, f.calls)
	}
	if err := cf.DeleteEdgeRule(ctx, tok, "z1", r.Ref, r.Description); err != nil {
		t.Fatal(err)
	}
	if len(f.rs.Rules) != 1 || f.rs.Rules[0].ID != "mine" {
		t.Fatalf("delete touched other rules: %+v", f.rs.Rules)
	}
	if err := cf.DeleteEdgeRule(ctx, tok, "z1", r.Ref, r.Description); err != nil {
		t.Fatal("deleting an absent rule must succeed:", err)
	}
}

func TestEdgeExpression(t *testing.T) {
	got := EdgeExpression([]string{"a.test", "www.a.test"}, []string{"wordpress_logged_in_", `we"ird`})
	want := `(http.host in {"a.test" "www.a.test"} and http.request.method in {"GET" "HEAD"} and ` +
		`not http.cookie contains "wordpress_logged_in_" and not http.cookie contains "we\"ird")`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestBunny(t *testing.T) {
	var purged bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("AccessKey") != "0123456789abcdef-0123-4567-89ab" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /pullzone/42":
			w.Write([]byte(`{"Id":42,"Name":"site","OriginUrl":"https://a.test","Hostnames":[{"Value":"site.b-cdn.net"},{"Value":"CDN.a.test"}]}`))
		case "POST /pullzone/42/purgeCache":
			purged = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	b := &Bunny{BaseURL: srv.URL}
	ctx := context.Background()
	z, err := b.PullZone(ctx, "0123456789abcdef-0123-4567-89ab", "42")
	if err != nil || z.OriginURL != "https://a.test" || len(z.Hostnames) != 2 || z.Hostnames[1] != "cdn.a.test" {
		t.Fatalf("%+v %v", z, err)
	}
	if err := b.Purge(ctx, "0123456789abcdef-0123-4567-89ab", "42"); err != nil || !purged {
		t.Fatalf("purge: %v", err)
	}
	if _, err := b.PullZone(ctx, "0123456789abcdef-0123-4567-89ab", "7"); !errors.Is(err, ErrNoPullZone) {
		t.Errorf("unknown zone: %v", err)
	}
	if _, err := b.PullZone(ctx, "ffffffffffffffffffff-0000", "42"); !errors.Is(err, ErrBunnyAuth) {
		t.Errorf("wrong key: %v", err)
	}
}
