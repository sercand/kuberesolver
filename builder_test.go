package kuberesolver

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/resolver"
)

func newTestBuilder() resolver.Builder {
	cl := NewInsecureK8sClient("http://127.0.0.1:8001")
	return NewBuilder(cl, KubernetesSchema)
}

type fakeConn struct {
	cmp   chan struct{}
	found []string
}

func (fc *fakeConn) NewAddress(addresses []resolver.Address) {
	for i, a := range addresses {
		fc.found = append(fc.found, a.Addr)
		fmt.Printf("%d, address: %s\n", i, a.Addr)
		fmt.Printf("%d, servername: %s\n", i, a.ServerName)
		fmt.Printf("%d, type: %+v\n", i, a.Type)
	}
	fc.cmp <- struct{}{}
}

func (*fakeConn) NewServiceConfig(serviceConfig string) {
	fmt.Printf("serviceConfig: %s\n", serviceConfig)
}

func (*fakeConn) UpdateState(state resolver.State) {
	fmt.Printf("state: %+v\n", state)
}

func SkipTestBuilder(t *testing.T) {
	bl := newTestBuilder()
	fc := &fakeConn{
		cmp: make(chan struct{}),
	}
	rs, err := bl.Build(resolver.Target{Endpoint: "kube-dns.kube-system:53", Scheme: "kubernetes", Authority: ""}, fc, resolver.BuildOption{})
	if err != nil {
		t.Fatal(err)
	}
	<-fc.cmp
	if len(fc.found) == 0 {
		t.Fatal("could not found endpoints")
	}
	fmt.Printf("ResolveNow \n")
	rs.ResolveNow(resolver.ResolveNowOption{})
	<-fc.cmp

}

//
// split2 returns the values from strings.SplitN(s, sep, 2).
// If sep is not found, it returns ("", s, false) instead.
func split2(s, sep string) (string, string, bool) {
	spl := strings.SplitN(s, sep, 2)
	if len(spl) < 2 {
		return "", "", false
	}
	return spl[0], spl[1], true
}

// copied from grpc package to test parsing endpoints
//
// parseTarget splits target into a struct containing scheme, authority and
// endpoint.
func parseTarget(target string) (ret resolver.Target) {
	var ok bool
	ret.Scheme, ret.Endpoint, ok = split2(target, "://")
	if !ok {
		return resolver.Target{Endpoint: target}
	}
	ret.Authority, ret.Endpoint, _ = split2(ret.Endpoint, "/")
	return ret
}

func TestParseResolverTarget(t *testing.T) {
	for _, test := range []struct {
		target resolver.Target
		want   targetInfo
		err    bool
	}{
		{resolver.Target{"", "", ""}, targetInfo{"", "", "", false, false}, true},
		{resolver.Target{"", "a", ""}, targetInfo{"a", "default", "", false, true}, false},
		{resolver.Target{"", "", "a"}, targetInfo{"a", "default", "", false, true}, false},
		{resolver.Target{"", "a", "b"}, targetInfo{"b", "a", "", false, true}, false},
		{resolver.Target{"", "a.b", ""}, targetInfo{"a", "b", "", false, true}, false},
		{resolver.Target{"", "", "a.b"}, targetInfo{"a", "b", "", false, true}, false},
		{resolver.Target{"", "", "a.b:80"}, targetInfo{"a", "b", "80", false, false}, false},
		{resolver.Target{"", "", "a.b:port"}, targetInfo{"a", "b", "port", true, false}, false},
		{resolver.Target{"", "a", "b:port"}, targetInfo{"b", "a", "port", true, false}, false},
		{resolver.Target{"", "b.a:port", ""}, targetInfo{"b", "a", "port", true, false}, false},
		{resolver.Target{"", "b.a:80", ""}, targetInfo{"b", "a", "80", false, false}, false},

		{resolver.Target{"", "", "app:443"}, targetInfo{"app", "default", "443", false, false}, false},
		{resolver.Target{"", "", "app.ns:65533"}, targetInfo{"app", "ns", "65533", false, false}, false},
		{resolver.Target{"", "", "app.ns.svc:443"}, targetInfo{"app", "ns", "443", false, false}, false},
		{resolver.Target{"", "", "app.ns.svc.cluster.local:65533"}, targetInfo{"app", "ns", "65533", false, false}, false},
	} {
		got, err := parseResolverTarget(test.target)
		if err == nil && test.err {
			t.Errorf("want error but got nil")
			continue
		}
		if err != nil && !test.err {
			t.Errorf("got '%v' error but don't want an error", err)
			continue
		}
		if got != test.want {
			t.Errorf("parseTarget(%q) = %+v, want %+v", test.target, got, test.want)
		}
	}
}

func TestParseTargets(t *testing.T) {
	for _, test := range []struct {
		target string
		want   targetInfo
		err    bool
	}{
		{"", targetInfo{}, true},
		{"k8s:///", targetInfo{}, true},
		{"k8s://a:30", targetInfo{}, true},
		{"k8s://a/", targetInfo{"a", "default", "", false, true}, false},
		{"k8s:///a", targetInfo{"a", "default", "", false, true}, false},
		{"k8s://a/b", targetInfo{"b", "a", "", false, true}, false},
		{"k8s://a.b/", targetInfo{"a", "b", "", false, true}, false},
		{"k8s:///a.b:80", targetInfo{"a", "b", "80", false, false}, false},
		{"k8s:///a.b:port", targetInfo{"a", "b", "port", true, false}, false},
		{"k8s:///a:port", targetInfo{"a", "default", "port", true, false}, false},
		{"k8s://x/a:port", targetInfo{"a", "x", "port", true, false}, false},
		{"k8s://a.x:port/", targetInfo{"a", "x", "port", true, false}, false},
		{"k8s://a.x:30/", targetInfo{"a", "x", "30", false, false}, false},

		{"k8s:///app:443", targetInfo{"app", "default", "443", false, false}, false},
		{"k8s:///app.ns:443", targetInfo{"app", "ns", "443", false, false}, false},
		{"k8s:///app.ns.svc:443", targetInfo{"app", "ns", "443", false, false}, false},
		{"k8s:///app.ns.svc.cluster.local:65533", targetInfo{"app", "ns", "65533", false, false}, false},
	} {
		got, err := parseResolverTarget(parseTarget(test.target))
		if err == nil && test.err {
			t.Errorf("want error but got nil")
			continue
		}
		if err != nil && !test.err {
			t.Errorf("got '%v' error but don't want an error", err)
			continue
		}
		if got != test.want {
			t.Errorf("parseTarget(%q) = %+v, want %+v", test.target, got, test.want)
		}
	}
}
