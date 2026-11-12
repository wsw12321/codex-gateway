package gatewayplugin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testRouteA  = "http://route-a:secret-a@egress-allowlist:3129"
	testRouteA2 = "http://route-a2:secret-a2@egress-allowlist:3129"
)

func TestPickWritesTheBoundRouteBeforeUsingAnAccount(t *testing.T) {
	f := newFixture(t, 2)
	f.alloc.routes = map[string]Egress{f.ids["a"]: {ProxyURL: testRouteA, Revision: "r1"}}
	f.alloc.prefer = []string{f.ids["a"], f.ids["b"]}
	// The credential has no proxy yet. CPA snapshotted it before the pick, so
	// this attempt cannot use a; it moves on to b and a's route is written.
	if name, err := f.run(pickOptions{}, "succeeded"); err != nil || name != "b" {
		t.Fatalf("picked %q, %v; want b while a's route is applied", name, err)
	}
	if len(f.applied) != 1 || f.applied[0] != "a="+testRouteA {
		t.Fatalf("applied %v", f.applied)
	}
	if f.active("a") != 0 {
		t.Fatal("the skipped account kept a slot")
	}
	// From now on a carries its route and is used.
	if name, err := f.run(pickOptions{}, "succeeded"); err != nil || name != "a" {
		t.Fatalf("picked %q, %v; want a once its route is in place", name, err)
	}
	if len(f.applied) != 1 {
		t.Fatalf("an unchanged route was written again: %v", f.applied)
	}
	// A rebinding is noticed on the next pick.
	f.alloc.routes[f.ids["a"]] = Egress{ProxyURL: testRouteA2, Revision: "r2"}
	if name, _ := f.run(pickOptions{}, "succeeded"); name != "b" {
		t.Fatalf("picked %q with a stale route on a", name)
	}
	if f.applied[len(f.applied)-1] != "a="+testRouteA2 {
		t.Fatalf("applied %v", f.applied)
	}
}

func TestPickReplacesAProxyTheGatewayDidNotBind(t *testing.T) {
	f := newFixture(t, 1)
	// A credential uploaded with its own proxy: the Gateway binds it to the
	// global route, so the proxy is removed before the account is used.
	f.host.setProxy("idx-a", "http://attacker.example:8080", f.clock)
	if _, err := f.pick(pickOptions{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v; the only account must be skipped until its proxy is removed", err)
	}
	if len(f.applied) != 1 || f.applied[0] != "a=" {
		t.Fatalf("applied %v, want the proxy cleared", f.applied)
	}
	if _, err := f.pick(pickOptions{}); err != nil {
		t.Fatalf("the account stayed unusable after its proxy was cleared: %v", err)
	}
}

func TestPickSkipsAccountsWhoseRouteCannotBeConfirmed(t *testing.T) {
	for name, configure := range map[string]func(*fixture){
		"gateway cannot resolve": func(f *fixture) { f.alloc.egressErr = map[string]error{f.ids["a"]: ErrUnavailable} },
		"route cannot be written": func(f *fixture) {
			f.alloc.routes = map[string]Egress{f.ids["a"]: {ProxyURL: testRouteA, Revision: "r1"}}
			f.applyErr = ErrUnavailable
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 2)
			configure(f)
			f.alloc.prefer = []string{f.ids["a"], f.ids["b"]}
			for i := 0; i < 3; i++ {
				if name, err := f.run(pickOptions{}, "succeeded"); err != nil || name != "b" {
					t.Fatalf("picked %q, %v; want b every time", name, err)
				}
			}
			if _, err := f.pick(pickOptions{only: []string{"a"}}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("got %v with only the unconfirmed account offered", err)
			}
			if f.active("a") != 0 {
				t.Fatal("a skipped account kept a slot")
			}
		})
	}
}

func TestManagementSyncsOneAccountsEgress(t *testing.T) {
	f := newFixture(t, 1)
	id := url.Values{"id": {f.ids["a"]}}
	f.alloc.routes = map[string]Egress{f.ids["a"]: {ProxyURL: testRouteA, Revision: "r1"}}
	status, fields, body := f.manage(http.MethodPut, managementPrefix+"upstream-accounts/egress", id, `{}`)
	if status != http.StatusOK || string(fields["revision"]) != `"r1"` || string(fields["id"]) != `"`+f.ids["a"]+`"` || len(fields) != 2 {
		t.Fatalf("%d %s", status, body)
	}
	if strings.Contains(body, "secret-a") {
		t.Fatalf("the route credential leaked into the response: %s", body)
	}
	// Applied ahead of time, so the first request uses the account directly.
	if name, err := f.pick(pickOptions{}); err != nil || name != "a" {
		t.Fatalf("picked %q, %v", name, err)
	}
	if status, _, _ = f.manage(http.MethodPut, managementPrefix+"upstream-accounts/egress", id, `{}`); status != http.StatusOK || len(f.applied) != 1 {
		t.Fatalf("an unchanged route was written again: %d %v", status, f.applied)
	}
	for name, test := range map[string]struct {
		query url.Values
		body  string
		want  int
	}{
		"unknown account":  {url.Values{"id": {"0123456789abcdef"}}, ``, http.StatusNotFound},
		"missing id":       {nil, ``, http.StatusNotFound},
		"body with fields": {id, `{"proxy_url":"http://x"}`, http.StatusBadRequest},
	} {
		if status, _, body = f.manage(http.MethodPut, managementPrefix+"upstream-accounts/egress", test.query, test.body); status != test.want {
			t.Fatalf("%s: %d %s", name, status, body)
		}
	}
	f.alloc.egressErr = map[string]error{f.ids["a"]: ErrUnavailable}
	if status, _, body = f.manage(http.MethodPut, managementPrefix+"upstream-accounts/egress", id, ``); status != http.StatusServiceUnavailable {
		t.Fatalf("an unresolvable route answered %d %s", status, body)
	}
}

func TestResolveEgressAcceptsOnlyTheFixedListener(t *testing.T) {
	var answer string
	var status int
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/upstream-accounts/resolve-egress" || r.Header.Get("Authorization") != "Bearer "+testInternalKey {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		got = map[string]string{"body": string(raw)}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()
	client := NewGatewayClient(server.URL, testInternalKey, "egress-allowlist:3129", time.Second)
	ctx := context.Background()
	const account = "0123456789abcdef"

	status, answer = http.StatusOK, `{"mode":"account","proxy_url":"`+testRouteA+`","revision":"r1"}`
	route, err := client.ResolveEgress(ctx, ProviderClaude, account)
	if err != nil || route.ProxyURL != testRouteA || route.Revision != "r1" {
		t.Fatalf("route = %+v, %v", route, err)
	}
	if got["body"] != `{"provider":"anthropic","account_id":"`+account+`"}` {
		t.Fatalf("request body %s", got["body"])
	}
	answer = `{"mode":"global","revision":"global:3"}`
	if route, err = client.ResolveEgress(ctx, ProviderCodex, account); err != nil || route.ProxyURL != "" {
		t.Fatalf("global route = %+v, %v", route, err)
	}
	for name, response := range map[string]string{
		"global with a proxy":     `{"mode":"global","proxy_url":"http://x:y@egress-allowlist:3129","revision":"g"}`,
		"another host":            `{"mode":"account","proxy_url":"http://u:p@attacker.example:3129","revision":"r"}`,
		"another port":            `{"mode":"account","proxy_url":"http://u:p@egress-allowlist:3128","revision":"r"}`,
		"no route credential":     `{"mode":"account","proxy_url":"http://egress-allowlist:3129","revision":"r"}`,
		"https":                   `{"mode":"account","proxy_url":"https://u:p@egress-allowlist:3129","revision":"r"}`,
		"socks":                   `{"mode":"account","proxy_url":"socks5://u:p@egress-allowlist:3129","revision":"r"}`,
		"path":                    `{"mode":"account","proxy_url":"http://u:p@egress-allowlist:3129/x","revision":"r"}`,
		"query":                   `{"mode":"account","proxy_url":"http://u:p@egress-allowlist:3129?x=1","revision":"r"}`,
		"unknown mode":            `{"mode":"direct","revision":"r"}`,
		"missing revision":        `{"mode":"global"}`,
		"duplicate key":           `{"mode":"global","mode":"account","revision":"r"}`,
		"not json":                `nope`,
		"account without a proxy": `{"mode":"account","revision":"r"}`,
	} {
		answer = response
		if route, err = client.ResolveEgress(ctx, ProviderCodex, account); err == nil {
			t.Fatalf("%s was accepted: %+v", name, route)
		}
	}
	status, answer = http.StatusServiceUnavailable, `{"mode":"global","revision":"g"}`
	if _, err = client.ResolveEgress(ctx, ProviderCodex, account); err == nil {
		t.Fatal("an error status was accepted")
	}
	if _, err = client.ResolveEgress(ctx, ProviderCodex, "not-an-id"); err == nil {
		t.Fatal("a malformed account id was sent")
	}
}
