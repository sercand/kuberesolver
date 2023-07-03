package kuberesolver

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestBuilder(t *testing.T) (resolver.Builder, *fake.Clientset) {
	cl := fake.NewSimpleClientset()
	builder, err := NewBuilder(ClusterOptions{
		KubeClient: cl,
	})
	require.NoError(t, err)
	return builder, cl
}

type fakeConn struct {
	cmp   chan struct{}
	found []string
	t     *testing.T
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
	for _, a := range addresses {
		fc.found = append(fc.found, a.Addr)
		fc.t.Logf("address: %q, servername: %q", a.Addr, a.ServerName)
	}
	fc.cmp <- struct{}{}
}

func (*fakeConn) NewServiceConfig(serviceConfig string) {
	fmt.Printf("serviceConfig: %s\n", serviceConfig)
}

func TestBuilderWithExplicitPort(t *testing.T) {
	b, client := newTestBuilder(t)
	fc := &fakeConn{
		cmp: make(chan struct{}),
		t:   t,
	}

	client.CoreV1().Endpoints("test-namespace").Create(context.Background(), &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service",
			Namespace: "test-namespace",
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						IP: "1.1.1.1",
					},
					{
						IP: "2.2.2.2",
					},
				},
				NotReadyAddresses: []corev1.EndpointAddress{
					{
						IP: "3.3.3.3",
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Name:     "http",
						Port:     8080,
						Protocol: "TCP",
					},
					{
						Name:     "http",
						Port:     53,
						Protocol: "TCP",
					},
				},
			},
		},
	}, metav1.CreateOptions{})

	_, err := b.Build(parseTarget(t, "kubernetes://service.test-namespace:53"), fc, resolver.BuildOptions{})
	require.NoError(t, err)
	<-fc.cmp
	assert.ElementsMatch(t, []string{"1.1.1.1:53", "2.2.2.2:53"}, fc.found)
}

func TestBuilderWithImplicitPort(t *testing.T) {
	b, client := newTestBuilder(t)
	fc := &fakeConn{
		cmp: make(chan struct{}),
		t:   t,
	}

	client.CoreV1().Endpoints("test-namespace").Create(context.Background(), &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service",
			Namespace: "test-namespace",
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						IP: "1.1.1.1",
					},
					{
						IP: "2.2.2.2",
					},
				},
				NotReadyAddresses: []corev1.EndpointAddress{
					{
						IP: "3.3.3.3",
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Name:     "http",
						Port:     8080,
						Protocol: "TCP",
					},

					{
						Name:     "http",
						Port:     53,
						Protocol: "TCP",
					},
				},
			},
		},
	}, metav1.CreateOptions{})

	_, err := b.Build(parseTarget(t, "kubernetes://service.test-namespace"), fc, resolver.BuildOptions{})
	require.NoError(t, err)
	<-fc.cmp
	assert.ElementsMatch(t, []string{"1.1.1.1:8080", "2.2.2.2:8080"}, fc.found)
}

// parseTarget is copied from grpc package to test parsing endpoints.
func parseTarget(t testing.TB, target string) resolver.Target {
	u, err := url.Parse(target)
	require.NoError(t, err)

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
		{parseTarget(t, "/"), targetInfo{"", "", "", false, false}, true},
		{parseTarget(t, "a"), targetInfo{"a", "", "", false, true}, false},
		{parseTarget(t, "/a"), targetInfo{"a", "", "", false, true}, false},
		{parseTarget(t, "//a/b"), targetInfo{"b", "a", "", false, true}, false},
		{parseTarget(t, "a.b"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget(t, "/a.b"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget(t, "/a.b:80"), targetInfo{"a", "b", "80", false, false}, false},
		{parseTarget(t, "/a.b:port"), targetInfo{"a", "b", "port", true, false}, false},
		{parseTarget(t, "//a/b:port"), targetInfo{"b", "a", "port", true, false}, false},
		{parseTarget(t, "//a/b:port"), targetInfo{"b", "a", "port", true, false}, false},
		{parseTarget(t, "//a/b:80"), targetInfo{"b", "a", "80", false, false}, false},
		{parseTarget(t, "a.b.svc.cluster.local"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget(t, "/a.b.svc.cluster.local:80"), targetInfo{"a", "b", "80", false, false}, false},
		{parseTarget(t, "/a.b.svc.cluster.local:port"), targetInfo{"a", "b", "port", true, false}, false},
		{parseTarget(t, "//a.b.svc.cluster.local"), targetInfo{"a", "b", "", false, true}, false},
		{parseTarget(t, "//a.b.svc.cluster.local:80"), targetInfo{"a", "b", "80", false, false}, false},
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
		got, err := parseResolverTarget(parseTarget(t, test.target))
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
