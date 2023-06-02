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

func (fc *fakeConn) ParseServiceConfig(_ string) *serviceconfig.ParseResult {
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
	_, err := bl.Build(parseTarget("kubernetes://kube-dns.kube-system:53"), fc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-fc.cmp
	if len(fc.found) == 0 {
		t.Fatal("could not found endpoints")
	}
// 	fmt.Printf("ResolveNow \n")
// 	rs.ResolveNow(resolver.ResolveNowOptions{})
// 	<-fc.cmp

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

func parseTarget(target string) resolver.Target {
	u, err := url.Parse(target)
	if err != nil {
		panic(err)
	}

	return resolver.Target{
		URL: *u,
	}
}

func TestParseResolverTarget(t *testing.T) {
	for i, test := range []struct {
		target resolver.Target
		want   targetInfo
		err    bool
	}{
		{parseTarget("/"), targetInfo{"", "", "", false, false}, true},
		{parseTarget("a"), targetInfo{"a", "", "", false, true}, false},
		{parseTarget("/a"), targetInfo{"a", "", "", false, true}, false},
		{parseTarget("//a/b"), targetInfo{"b", "a", "", false, true}, false},
		{parseTarget("a.b"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget("/a.b"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget("/a.b:80"), targetInfo{"a", "b", "80", false, false}, false},
		{parseTarget("/a.b:port"), targetInfo{"a", "b", "port", true, false}, false},
		{parseTarget("//a/b:port"), targetInfo{"b", "a", "port", true, false}, false},
		{parseTarget("//a/b:port"), targetInfo{"b", "a", "port", true, false}, false},
		{parseTarget("//a/b:80"), targetInfo{"b", "a", "80", false, false}, false},
		{parseTarget("a.b.svc.cluster.local"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget("/a.b.svc.cluster.local:80"), targetInfo{"a", "b", "80", false, false}, false},
		{parseTarget("/a.b.svc.cluster.local:port"), targetInfo{"a", "b", "port", true, false}, false},
		{parseTarget("//a.b.svc.cluster.local"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget("//a.b.svc.cluster.local:80"), targetInfo{"a", "b", "80", false, false}, false},
	} {
		got, err := parseResolverTarget(test.target)
		if err == nil && test.err {
			t.Errorf("case %d: want error but got nil", i)
			continue
		}
		if err != nil && !test.err {
			t.Errorf("case %d: got '%v' error but don't want an error", i, err)
			continue
		}
		if got != test.want {
			t.Errorf("case %d parseResolverTarget(%q) = %+v, want %+v", i, &test.target.URL, got, test.want)
		}
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
		{"kubernetes://a.x:30/", targetInfo{"a", "x", "30", false, false}, false},
		{"kubernetes://a.b.svc.cluster.local", targetInfo{"a", "b", "", false, true}, false},
		{"kubernetes://a.b.svc.cluster.local:80", targetInfo{"a", "b", "80", false, false}, false},
		{"kubernetes:///a.b.svc.cluster.local", targetInfo{"a", "b", "", false, true}, false},
		{"kubernetes:///a.b.svc.cluster.local:80", targetInfo{"a", "b", "80", false, false}, false},
		{"kubernetes:///a.b.svc.cluster.local:port", targetInfo{"a", "b", "port", true, false}, false},
	} {
		got, err := parseResolverTarget(parseTarget(test.target))
		if err == nil && test.err {
			t.Errorf("case %d: want error but got nil", i)
			continue
		}
		if err != nil && !test.err {
			t.Errorf("case %d:got '%v' error but don't want an error", i, err)
			continue
		}
		if got != test.want {
			t.Errorf("case %d: parseTarget(%q) = %+v, want %+v", i, test.target, got, test.want)
		}
	}
}
