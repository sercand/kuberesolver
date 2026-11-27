package kuberesolver

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

// isKubeProxyAvailable checks whether a Kubernetes API proxy is reachable
// at 127.0.0.1:8001.
func isKubeProxyAvailable() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:8001", 2*time.Second)
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}

// newMockKubeServer starts a httptest.Server that returns fake EndpointSlice
// responses for both the watch and list API paths.
func newMockKubeServer(t *testing.T) *httptest.Server {
	t.Helper()

	ready := true
	fakeSlice := EndpointSlice{
		Endpoints: []Endpoint{
			{
				Addresses: []string{"10.0.0.1", "10.0.0.2"},
				Conditions: EndpointConditions{
					Ready: &ready,
				},
			},
		},
		Ports: []EndpointPort{
			{Name: "dns", Port: 53},
		},
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/watch/") {
			// Watch endpoint: stream a single ADDED event, then hold the
			// connection open until the client disconnects.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)

			event := Event{
				Type:   Added,
				Object: fakeSlice,
			}
			if err := json.NewEncoder(w).Encode(event); err != nil {
				t.Logf("mock server: failed to encode watch event: %v", err)
				return
			}

			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Block until the client closes the connection.
			<-r.Context().Done()
		} else {
			// List endpoint
			w.Header().Set("Content-Type", "application/json")

			list := EndpointSliceList{
				Items: []EndpointSlice{fakeSlice},
			}
			_ = json.NewEncoder(w).Encode(list)
		}
	}))
}

// getTestAPIURL returns the base URL to use for integration tests together
// with a cleanup function. It prefers a live kubectl proxy when available and
// falls back to a local mock server otherwise.
func getTestAPIURL(t *testing.T) (apiURL string, cleanup func()) {
	t.Helper()

	if isKubeProxyAvailable() {
		t.Log("Using live Kubernetes API proxy at http://127.0.0.1:8001")
		return "http://127.0.0.1:8001", func() {}
	}

	t.Log("Kubernetes API proxy not available, using mock server")
	srv := newMockKubeServer(t)

	return srv.URL, srv.Close
}

type fakeConn struct {
	cmp   chan struct{}
	mu    sync.Mutex
	found []string
}

func (fc *fakeConn) foundCount() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	return len(fc.found)
}

func (fc *fakeConn) UpdateState(state resolver.State) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	for i, a := range state.Addresses {
		fc.found = append(fc.found, a.Addr)
		fmt.Printf("%d, address: %s\n", i, a.Addr)
		fmt.Printf("%d, servername: %s\n", i, a.ServerName)
	}

	// Never block the resolver: it pushes once after the initial LIST and
	// again for every watch event.
	select {
	case fc.cmp <- struct{}{}:
	default:
	}

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
	fmt.Printf("addresses: %s\n", addresses)
}

func (*fakeConn) NewServiceConfig(serviceConfig string) {
	fmt.Printf("serviceConfig: %s\n", serviceConfig)
}

func TestBuilder(t *testing.T) {
	apiURL, cleanup := getTestAPIURL(t)
	defer cleanup()

	cl := NewInsecureK8sClient(apiURL)
	bl := NewBuilder(cl, kubernetesSchema)
	fc := &fakeConn{
		cmp: make(chan struct{}, 1),
	}

	rs, err := bl.Build(parseTarget("kubernetes://kube-dns.kube-system:53"), fc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	<-fc.cmp

	if fc.foundCount() == 0 {
		t.Fatal("could not found endpoints")
	}
}

func TestResolveLag(t *testing.T) {
	apiURL, cleanup := getTestAPIURL(t)
	defer cleanup()

	cl := NewInsecureK8sClient(apiURL)
	bl := NewBuilder(cl, kubernetesSchema)
	fc := &fakeConn{
		cmp: make(chan struct{}, 1),
	}

	rs, err := bl.Build(parseTarget("kubernetes://kube-dns.kube-system:53"), fc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	<-fc.cmp

	if fc.foundCount() == 0 {
		t.Fatal("could not found endpoints")
	}

	time.Sleep(2 * time.Second)

	kresolver := rs.(*kResolver)
	clientResolveLag := testutil.ToFloat64(kresolver.lastUpdateUnix) - float64(time.Now().Unix())
	assert.Less(t, clientResolveLag, 0.0)
	t.Logf("client resolver lag: %v s", -clientResolveLag)
}

// copied from grpc package to test parsing endpoints

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
		{parseTarget("/"), targetInfo{"", "", "", "", false, false}, true},
		{parseTarget("a"), targetInfo{"", "a", "", "", false, true}, false},
		{parseTarget("/a"), targetInfo{"", "a", "", "", false, true}, false},
		{parseTarget("//a/b"), targetInfo{"", "b", "a", "", false, true}, false},
		{parseTarget("a.b"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("/a.b"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("/a.b:80"), targetInfo{"", "a", "b", "80", false, false}, false},
		{parseTarget("/a.b:port"), targetInfo{"", "a", "b", "port", true, false}, false},
		{parseTarget("//a/b:port"), targetInfo{"", "b", "a", "port", true, false}, false},
		{parseTarget("//a/b:port"), targetInfo{"", "b", "a", "port", true, false}, false},
		{parseTarget("//a/b:80"), targetInfo{"", "b", "a", "80", false, false}, false},
		{parseTarget("a.b.svc.cluster.local"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("/a.b.svc.cluster.local:80"), targetInfo{"", "a", "b", "80", false, false}, false},
		{parseTarget("/a.b.svc.cluster.local:port"), targetInfo{"", "a", "b", "port", true, false}, false},
		{parseTarget("//a.b.svc.cluster.local"), targetInfo{"", "a", "b", "", false, true}, false},
		{parseTarget("//a.b.svc.cluster.local:80"), targetInfo{"", "a", "b", "80", false, false}, false},
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
		{"kubernetes://a:30", targetInfo{"kubernetes", "a", "", "30", false, false}, false},
		{"kubernetes://a/", targetInfo{"kubernetes", "a", "", "", false, true}, false},
		{"kubernetes:///a", targetInfo{"kubernetes", "a", "", "", false, true}, false},
		{"kubernetes://a/b", targetInfo{"kubernetes", "b", "a", "", false, true}, false},
		{"kubernetes://a.b/", targetInfo{"kubernetes", "a", "b", "", false, true}, false},
		{"kubernetes:///a.b:80", targetInfo{"kubernetes", "a", "b", "80", false, false}, false},
		{"kubernetes:///a.b:port", targetInfo{"kubernetes", "a", "b", "port", true, false}, false},
		{"kubernetes:///a:port", targetInfo{"kubernetes", "a", "", "port", true, false}, false},
		{"kubernetes://x/a:port", targetInfo{"kubernetes", "a", "x", "port", true, false}, false},
		{"kubernetes://a.x:30/", targetInfo{"kubernetes", "a", "x", "30", false, false}, false},
		{"kubernetes://a.b.svc.cluster.local", targetInfo{"kubernetes", "a", "b", "", false, true}, false},
		{"kubernetes://a.b.svc.cluster.local:80", targetInfo{"kubernetes", "a", "b", "80", false, false}, false},
		{"kubernetes:///a.b.svc.cluster.local", targetInfo{"kubernetes", "a", "b", "", false, true}, false},
		{"kubernetes:///a.b.svc.cluster.local:80", targetInfo{"kubernetes", "a", "b", "80", false, false}, false},
		{"kubernetes:///a.b.svc.cluster.local:port", targetInfo{"kubernetes", "a", "b", "port", true, false}, false},
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

// blockingConn blocks in UpdateState on the push numbered blockOn (1-based)
// until release is closed, so a test can hold the watch goroutine busy.
type blockingConn struct {
	resolver.ClientConn // unused methods; calling one panics

	mu      sync.Mutex
	pushes  int
	blockOn int
	blocked chan struct{}
	release chan struct{}
}

func (bc *blockingConn) UpdateState(resolver.State) error {
	bc.mu.Lock()
	bc.pushes++
	n := bc.pushes
	bc.mu.Unlock()

	if n == bc.blockOn {
		close(bc.blocked)
		<-bc.release
	}

	return nil
}

// TestCloseWaitsForWatchAfterReconnect checks that Close still waits for the
// watch goroutine after the watch has reconnected. watch() used to call
// wg.Done() on every attempt although Build only called wg.Add(1) once, so
// after the first reconnect Close returned while the goroutine was still
// running, and the next attempt panicked on a negative WaitGroup counter.
func TestCloseWaitsForWatchAfterReconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "/watch/") {
			// End the stream at once so the resolver reconnects.
			return
		}

		_, _ = fmt.Fprintf(w, `{"items":[`+sliceJSON+`]}`, "svc-a", "10.0.0.1")
	}))
	defer srv.Close()

	// Push 1 is the LIST of the first watch attempt, push 2 the LIST of the
	// second attempt after the reconnect: block there.
	bc := &blockingConn{blockOn: 2, blocked: make(chan struct{}), release: make(chan struct{})}

	rs, err := NewBuilder(NewInsecureK8sClient(srv.URL), kubernetesSchema).
		Build(parseTarget("kubernetes:///svc.ns:grpc"), bc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-bc.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never reconnected")
	}

	closed := make(chan struct{})

	go func() {
		rs.Close()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned while the watch goroutine was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(bc.release)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the watch goroutine finished")
	}
}
