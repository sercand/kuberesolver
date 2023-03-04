package kuberesolver

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

func newTestBuilder() resolver.Builder {
	cl := NewInsecureK8sClient("http://127.0.0.1:8001")
	return NewBuilder(cl, kubernetesSchema)
}

type fakeConn struct {
	cmp   chan struct{}
	found []string
}

func (fc *fakeConn) UpdateState(resolver.State) error {
	return nil
}

func (fc *fakeConn) ReportError(e error) {
	log.Println(e)
}

func (fc *fakeConn) ParseServiceConfig(serviceConfigJSON string) *serviceconfig.ParseResult {
	return &serviceconfig.ParseResult{
		Config: nil,
		Err:    fmt.Errorf("no implementation for ParseServiceConfig"),
	}
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

func TestBuilder(t *testing.T) {
	bl := newTestBuilder()
	fc := &fakeConn{
		cmp: make(chan struct{}),
	}
	endURL, err := url.Parse("kubernetes://kube-dns.kube-system:53")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := bl.Build(resolver.Target{URL: *endURL}, fc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-fc.cmp
	if len(fc.found) == 0 {
		t.Fatal("could not found endpoints")
	}
	fmt.Printf("ResolveNow \n")
	rs.ResolveNow(resolver.ResolveNowOptions{})
	<-fc.cmp

}

// copied from grpc package to test parsing endpoints

// split2 returns the values from strings.SplitN(s, sep, 2).
// If sep is not found, it returns ("", "", false) instead.
func split2(s, sep string) (string, string, bool) {
	spl := strings.SplitN(s, sep, 2)
	if len(spl) < 2 {
		return "", "", false
	}
	return spl[0], spl[1], true
}

// ParseTarget splits target into a resolver.Target struct containing scheme,
// authority and endpoint.
//
// As a special case if target contains a named port like "foo:port"
// we manually handle that as url.Parse does NOT like named ports that
// aren't numeric.
func parseTarget(target string) (ret resolver.Target) {
	newTarget := strings.Replace(target, ":port", ":443", 1)
	isNamedPort := newTarget != target

	tURL, err := url.Parse(newTarget)
	if err != nil {
		return resolver.Target{URL: url.URL{Path: target}}
	}

	if isNamedPort {
		tURL.Host = strings.Replace(tURL.Host, ":443", ":port", 1)
		tURL.Path = strings.Replace(tURL.Path, ":443", ":port", 1)
	}
	return resolver.Target{URL: *tURL}
}

func TestParseResolverTarget(t *testing.T) {
	for i, test := range []struct {
		target resolver.Target
		want   targetInfo
		err    bool
	}{
		{resolver.Target{URL: url.URL{}}, targetInfo{"", "", "", false, false}, true},
		{resolver.Target{URL: url.URL{Path: "a"}}, targetInfo{"a", "", "", false, true}, false},
		{resolver.Target{URL: url.URL{Host: "a"}}, targetInfo{"a", "", "", false, true}, false},
		{resolver.Target{URL: url.URL{Path: "a/b"}}, targetInfo{"b", "a", "", false, true}, false},
		{resolver.Target{URL: url.URL{Host: "a.b"}}, targetInfo{"a", "b", "", false, true}, false},
		{resolver.Target{URL: url.URL{Path: "/a.b"}}, targetInfo{"a", "b", "", false, true}, false},
		{resolver.Target{URL: url.URL{Host: "a.b:80"}}, targetInfo{"a", "b", "80", false, false}, false},
		{resolver.Target{URL: url.URL{Path: "a.b:80"}}, targetInfo{"a", "b", "80", false, false}, false},
		{resolver.Target{URL: url.URL{Host: "a.b:port"}}, targetInfo{"a", "b", "port", true, false}, false},
		{resolver.Target{URL: url.URL{Path: "a.b:port"}}, targetInfo{"a", "b", "port", true, false}, false},
		{resolver.Target{URL: url.URL{Path: "/a.b:port"}}, targetInfo{"a", "b", "port", true, false}, false},
		{resolver.Target{URL: url.URL{Host: "a", Path: "b:port"}}, targetInfo{"b", "a", "port", true, false}, false},
		{resolver.Target{URL: url.URL{Host: "a", Path: "/b:port"}}, targetInfo{"b", "a", "port", true, false}, false},
		{resolver.Target{URL: url.URL{Host: "b.a:80"}}, targetInfo{"b", "a", "80", false, false}, false},
		{resolver.Target{URL: url.URL{Host: "b.a:port"}}, targetInfo{"b", "a", "port", true, false}, false},
	} {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			got, err := parseResolverTarget(test.target)

			if err == nil && test.err {
				t.Errorf("want error but got nil")
				return
			}
			if err != nil && !test.err {
				t.Errorf("got '%v' error but don't want an error", err)
				return
			}
			if got != test.want {
				t.Errorf("parseResolverTarget(%q) = %+v, want %+v", test.target.URL.String(), got, test.want)
				return
			}
		})
	}
}

func TestParseTargets(t *testing.T) {
	for i, test := range []struct {
		target string
		want   targetInfo
		err    bool
	}{
		{"", targetInfo{}, true},
		{"kubernetes:///", targetInfo{}, true},
		{"kubernetes://a:30", targetInfo{"a", "", "30", false, false}, false},
		{"kubernetes://a/", targetInfo{"a", "", "", false, true}, false},
		{"kubernetes:///a", targetInfo{"a", "", "", false, true}, false},
		{"kubernetes://a/b", targetInfo{"b", "a", "", false, true}, false},
		{"kubernetes://a.b/", targetInfo{"a", "b", "", false, true}, false},
		{"kubernetes:///a.b:80", targetInfo{"a", "b", "80", false, false}, false},
		{"kubernetes:///a.b:port", targetInfo{"a", "b", "port", true, false}, false},
		{"kubernetes:///a:port", targetInfo{"a", "", "port", true, false}, false},
		{"kubernetes://x/a:port", targetInfo{"a", "x", "port", true, false}, false},
		{"kubernetes://a.x:port/", targetInfo{"a", "x", "port", true, false}, false},
		{"kubernetes://a.x:30/", targetInfo{"a", "x", "30", false, false}, false},
	} {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			got, err := parseResolverTarget(parseTarget(test.target))
			if err == nil && test.err {
				t.Errorf("case %d: want error but got nil", i)
				return
			}
			if err != nil && !test.err {
				t.Errorf("%q got '%v' error but don't want an error", test.target, err)
				return
			}
			if got != test.want {
				t.Errorf("parseResolverTarget(%q) = %+v, want %+v", test.target, got, test.want)
			}
			return
		})
	}
}
