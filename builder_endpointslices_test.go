package kuberesolver

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/resolver"
)

// recordingConn is a resolver.ClientConn that remembers every state pushed to
// it. UpdateState never blocks, so the resolver can push as often as it likes.
type recordingConn struct {
	resolver.ClientConn // unused methods; calling one panics

	mu     sync.Mutex
	states []resolver.State
}

func (rc *recordingConn) UpdateState(s resolver.State) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.states = append(rc.states, s)

	return nil
}

// addrs returns the sorted addresses of the last pushed state.
func (rc *recordingConn) addrs() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if len(rc.states) == 0 {
		return nil
	}

	var out []string
	for _, a := range rc.states[len(rc.states)-1].Addresses {
		out = append(out, a.Addr)
	}

	sort.Strings(out)

	return out
}

func (rc *recordingConn) pushes() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	return len(rc.states)
}

const (
	addrA = "10.0.0.1:9000"
	addrB = "10.0.0.2:9000"
)

// grpcPortName is the name of the port in the test slices.
const grpcPortName = "grpc"

const sliceJSON = `{"metadata":{"name":%q},"endpoints":[{"addresses":[%q],"conditions":{"ready":true}}],"ports":[{"name":"grpc","port":9000}]}`

// TestWatchMergesMultipleEndpointSlices drives the real watch path: the API
// server streams two ADDED events for two slices of the same Service, and the
// resolver must expose the pods of both.
func TestWatchMergesMultipleEndpointSlices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !strings.Contains(r.URL.Path, "/watch/") {
			// LIST returns nothing, so the addresses must come from the watch.
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"type":"ADDED","object":`+sliceJSON+"}\n", "svc-a", "10.0.0.1")
		_, _ = fmt.Fprintf(w, `{"type":"ADDED","object":`+sliceJSON+"}\n", "svc-b", "10.0.0.2")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	rc := &recordingConn{}

	rs, err := NewBuilder(NewInsecureK8sClient(srv.URL), kubernetesSchema).
		Build(parseTarget("kubernetes:///svc.ns:grpc"), rc, resolver.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, []string{addrA, addrB}, rc.addrs())
	}, 5*time.Second, 10*time.Millisecond)
}

// newTestResolver returns a kResolver wired to rc without starting the watch
// goroutine, so tests can feed it events directly.
func newTestResolver(t *testing.T, rc resolver.ClientConn, client K8sClient) *kResolver {
	t.Helper()

	label := t.Name()

	return &kResolver{
		target:         targetInfo{serviceName: "svc", serviceNamespace: "ns", port: grpcPortName, resolveByPortName: true},
		cc:             rc,
		k8sClient:      client,
		t:              time.NewTimer(time.Hour),
		freq:           time.Hour,
		slices:         map[string]EndpointSlice{},
		endpoints:      endpointsForTarget.WithLabelValues(label),
		addresses:      addressesForTarget.WithLabelValues(label),
		lastUpdateUnix: clientLastUpdate.WithLabelValues(label),
	}
}

// slice builds a ready EndpointSlice with one endpoint per address.
func slice(name string, addrs ...string) EndpointSlice {
	ready := true

	s := EndpointSlice{
		Metadata: Metadata{Name: name},
		Ports:    []EndpointPort{{Name: grpcPortName, Port: 9000}},
	}
	for _, a := range addrs {
		s.Endpoints = append(s.Endpoints, Endpoint{
			Addresses:  []string{a},
			Conditions: EndpointConditions{Ready: &ready},
		})
	}

	return s
}

func TestApplyEventMergesSlices(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1", "10.0.0.2")})
	k.applyEvent(Event{Type: Added, Object: slice("svc-b", "10.0.0.3")})

	assert.Equal(t, []string{addrA, addrB, "10.0.0.3:9000"}, rc.addrs())
	assert.Equal(t, 3.0, testutil.ToFloat64(k.endpoints))
	assert.Equal(t, 3.0, testutil.ToFloat64(k.addresses))
}

func TestApplyEventModifiedReplacesOnlyThatSlice(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1")})
	k.applyEvent(Event{Type: Added, Object: slice("svc-b", "10.0.0.2")})
	k.applyEvent(Event{Type: Modified, Object: slice("svc-a", "10.0.0.9")})

	assert.Equal(t, []string{addrB, "10.0.0.9:9000"}, rc.addrs())
}

func TestApplyEventDeletedRemovesSlice(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1")})
	k.applyEvent(Event{Type: Added, Object: slice("svc-b", "10.0.0.2")})
	k.applyEvent(Event{Type: Deleted, Object: slice("svc-b", "10.0.0.2")})

	assert.Equal(t, []string{addrA}, rc.addrs())
}

func TestApplyEventDeletingLastSliceKeepsLastState(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1")})
	k.applyEvent(Event{Type: Deleted, Object: slice("svc-a", "10.0.0.1")})

	// Same behaviour as before the fix: an empty list is never pushed.
	assert.Equal(t, 1, rc.pushes())
	assert.Equal(t, []string{addrA}, rc.addrs())
	assert.Equal(t, 0.0, testutil.ToFloat64(k.addresses))
}

func TestApplyEventDualStack(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	k.applyEvent(Event{Type: Added, Object: slice("svc-ipv4", "10.0.0.1")})
	k.applyEvent(Event{Type: Added, Object: slice("svc-ipv6", "fd00::1")})

	assert.Equal(t, []string{addrA, "[fd00::1]:9000"}, rc.addrs())
}

func TestApplyEventDeduplicatesAddresses(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	// The EndpointSlice controller can list an endpoint in two slices while
	// it moves it from one to the other.
	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1", "10.0.0.2")})
	k.applyEvent(Event{Type: Added, Object: slice("svc-b", "10.0.0.2")})

	assert.Equal(t, []string{addrA, addrB}, rc.addrs())
}

func TestApplyEventIgnoresErrorEvents(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1")})
	k.applyEvent(Event{Type: Error})

	assert.Equal(t, 1, rc.pushes())
	assert.Len(t, k.slices, 1)
}

func TestApplyEventSkipsSliceWithoutPorts(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)
	k.target = targetInfo{serviceName: "svc", serviceNamespace: "ns", useFirstPort: true}

	noPorts := slice("svc-b", "10.0.0.2")
	noPorts.Ports = nil

	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1")})
	k.applyEvent(Event{Type: Added, Object: noPorts})

	assert.Equal(t, []string{addrA}, rc.addrs())
}

// TestResolveReplacesCache checks that the LIST resync drops slices whose
// DELETED event was missed, for example while the watch was reconnecting.
func TestResolveReplacesCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"items":[`+sliceJSON+`,`+sliceJSON+`]}`, "svc-a", "10.0.0.1", "svc-b", "10.0.0.2")
	}))
	defer srv.Close()

	rc := &recordingConn{}
	k := newTestResolver(t, rc, NewInsecureK8sClient(srv.URL))
	k.slices["svc-stale"] = slice("svc-stale", "10.9.9.9")

	k.resolve()

	assert.Equal(t, []string{addrA, addrB}, rc.addrs())
	assert.NotContains(t, k.slices, "svc-stale")
}

// TestResolveKeepsCacheOnListError checks that a failed LIST (for example a
// missing "list" RBAC verb) leaves the cache and gRPC state alone.
func TestResolveKeepsCacheOnListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	rc := &recordingConn{}
	k := newTestResolver(t, rc, NewInsecureK8sClient(srv.URL))
	k.applyEvent(Event{Type: Added, Object: slice("svc-a", "10.0.0.1")})

	k.resolve()

	assert.Equal(t, 1, rc.pushes())
	assert.Contains(t, k.slices, "svc-a")
}

// Scale tests: a Service with more than 100 endpoints is split by the
// EndpointSlice controller into several slices of at most 100 endpoints each
// (--max-endpoints-per-slice). These tests use production-like shapes.

// pods returns n unique pod IPs starting at pod index from.
func pods(from, n int) []string {
	ips := make([]string, n)
	for i := range ips {
		p := from + i
		ips[i] = fmt.Sprintf("10.1.%d.%d", p/256, p%256)
	}

	return ips
}

// podAddrs returns the sorted host:port addresses gRPC should get for ips.
func podAddrs(ips ...[]string) []string {
	var out []string

	for _, group := range ips {
		for _, ip := range group {
			out = append(out, net.JoinHostPort(ip, "9000"))
		}
	}

	sort.Strings(out)

	return out
}

// allStates returns the addresses of every state pushed so far.
func (rc *recordingConn) allStates() [][]string {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	out := make([][]string, len(rc.states))
	for i, s := range rc.states {
		for _, a := range s.Addresses {
			out[i] = append(out[i], a.Addr)
		}
	}

	return out
}

// threeSlices returns 250 ready pods split 100/100/50, as the EndpointSlice
// controller would split them.
func threeSlices() []EndpointSlice {
	return []EndpointSlice{
		slice("svc-a", pods(0, 100)...),
		slice("svc-b", pods(100, 100)...),
		slice("svc-c", pods(200, 50)...),
	}
}

func TestScale250PodsAcrossThreeSlices(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	for _, s := range threeSlices() {
		k.applyEvent(Event{Type: Added, Object: s})
	}

	assert.Len(t, rc.addrs(), 250)
	assert.Equal(t, podAddrs(pods(0, 250)), rc.addrs())
	assert.Equal(t, 250.0, testutil.ToFloat64(k.endpoints))
	assert.Equal(t, 250.0, testutil.ToFloat64(k.addresses))
}

func TestScaleUpAndDown(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	for _, s := range threeSlices() {
		k.applyEvent(Event{Type: Added, Object: s})
	}

	// Scale up 250 → 300: the controller creates a 4th slice.
	k.applyEvent(Event{Type: Added, Object: slice("svc-d", pods(250, 50)...)})

	assert.Equal(t, podAddrs(pods(0, 300)), rc.addrs())
	assert.Equal(t, 300.0, testutil.ToFloat64(k.endpoints))
	assert.Equal(t, 300.0, testutil.ToFloat64(k.addresses))

	// Scale down 300 → 250: the 4th slice is deleted.
	k.applyEvent(Event{Type: Deleted, Object: slice("svc-d", pods(250, 50)...)})

	assert.Equal(t, podAddrs(pods(0, 250)), rc.addrs())
	assert.Equal(t, 250.0, testutil.ToFloat64(k.endpoints))
	assert.Equal(t, 250.0, testutil.ToFloat64(k.addresses))
}

func TestScaleRollingUpdate(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	for _, s := range threeSlices() {
		k.applyEvent(Event{Type: Added, Object: s})
	}

	before := len(rc.allStates())

	// The controller rebalances pods 90-99 from svc-a into svc-c. It updates
	// svc-c first, so for one event those pods are listed in both slices.
	k.applyEvent(Event{Type: Modified, Object: slice("svc-c", append(pods(200, 50), pods(90, 10)...)...)})
	k.applyEvent(Event{Type: Modified, Object: slice("svc-a", pods(0, 90)...)})

	// Every state pushed during the move must hold each of the 250 pods
	// exactly once: no doubled pods, no missing pods.
	for i, state := range rc.allStates()[before:] {
		sort.Strings(state)
		assert.Equal(t, podAddrs(pods(0, 250)), state, "push %d during the move", i)
	}

	// A rollout then replaces pods 100-109 in svc-b with new pods 300-309.
	k.applyEvent(Event{Type: Modified, Object: slice("svc-b", append(pods(110, 90), pods(300, 10)...)...)})

	assert.Equal(t, podAddrs(pods(0, 100), pods(110, 140), pods(300, 10)), rc.addrs())
	assert.Equal(t, 250.0, testutil.ToFloat64(k.addresses))
}

func TestScaleNotReadyPods(t *testing.T) {
	rc := &recordingConn{}
	k := newTestResolver(t, rc, nil)

	notReady := false
	slices := threeSlices()

	// In svc-b, pods 100-129 are not ready, and pod 130 has no ready
	// condition at all, which also counts as not ready.
	for i := 0; i < 30; i++ {
		slices[1].Endpoints[i].Conditions.Ready = &notReady
	}

	slices[1].Endpoints[30].Conditions.Ready = nil

	for _, s := range slices {
		k.applyEvent(Event{Type: Added, Object: s})
	}

	assert.Equal(t, podAddrs(pods(0, 100), pods(131, 69), pods(200, 50)), rc.addrs())
	assert.Equal(t, 250.0, testutil.ToFloat64(k.endpoints))
	assert.Equal(t, 219.0, testutil.ToFloat64(k.addresses))

	// Then every pod in svc-c becomes not ready. The other slices must stay.
	allNotReady := slice("svc-c", pods(200, 50)...)
	for i := range allNotReady.Endpoints {
		allNotReady.Endpoints[i].Conditions.Ready = &notReady
	}

	k.applyEvent(Event{Type: Modified, Object: allNotReady})

	assert.Equal(t, podAddrs(pods(0, 100), pods(131, 69)), rc.addrs())
	assert.Equal(t, 250.0, testutil.ToFloat64(k.endpoints))
	assert.Equal(t, 169.0, testutil.ToFloat64(k.addresses))
}

// TestScaleEndToEnd sends the 250-pod Service through the real HTTP path, once
// as watch events and once as the initial LIST.
func TestScaleEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		list   []EndpointSlice
		events []EndpointSlice
	}{
		{name: "watch events", events: threeSlices()},
		{name: "initial list", list: threeSlices()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				if !strings.Contains(r.URL.Path, "/watch/") {
					_ = json.NewEncoder(w).Encode(EndpointSliceList{Items: tc.list})
					return
				}

				w.WriteHeader(http.StatusOK)

				for _, s := range tc.events {
					_ = json.NewEncoder(w).Encode(Event{Type: Added, Object: s})
					// One flush per event, like the API server sends them.
					w.(http.Flusher).Flush()
				}

				<-r.Context().Done()
			}))
			defer srv.Close()

			rc := &recordingConn{}

			rs, err := NewBuilder(NewInsecureK8sClient(srv.URL), kubernetesSchema).
				Build(parseTarget("kubernetes:///svc.ns:grpc"), rc, resolver.BuildOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer rs.Close()

			assert.EventuallyWithT(t, func(c *assert.CollectT) {
				assert.Equal(c, podAddrs(pods(0, 250)), rc.addrs())
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

// TestMakeAddressesPort checks where the port of each address comes from: the
// target string, the slice's ports, or nowhere. Target and slice use different
// numbers so the source of the port is always visible.
func TestMakeAddressesPort(t *testing.T) {
	grpcPort := EndpointPort{Name: grpcPortName, Port: 9000}
	metricsPort := EndpointPort{Name: "metrics", Port: 9100}

	for _, tc := range []struct {
		name   string
		target string
		ports  []EndpointPort
		want   []string
	}{
		{
			name:   "numeric target port wins over slice ports",
			target: "kubernetes:///svc.ns:8080",
			ports:  []EndpointPort{grpcPort},
			want:   []string{"10.0.0.1:8080"},
		},
		{
			name:   "named target port is looked up in the slice",
			target: "kubernetes:///svc.ns:grpc",
			ports:  []EndpointPort{metricsPort, grpcPort},
			want:   []string{"10.0.0.1:9000"},
		},
		{
			name:   "no target port uses the slice's first port",
			target: "kubernetes:///svc.ns",
			ports:  []EndpointPort{metricsPort, grpcPort},
			want:   []string{"10.0.0.1:9100"},
		},
		{
			// A headless Service can have no ports. This case fails if the
			// empty-port check in makeAddresses is moved before the loop.
			name:   "numeric target port with no slice ports",
			target: "kubernetes:///svc.ns:8080",
			ports:  nil,
			want:   []string{"10.0.0.1:8080"},
		},
		{
			name:   "no port anywhere skips the slice",
			target: "kubernetes:///svc.ns",
			ports:  nil,
			want:   nil,
		},
		{
			// Known bug, not fixed yet: the unmatched port name ends up in the
			// address. Update this case when the bug is fixed.
			name:   "named target port missing from the slice",
			target: "kubernetes:///svc.ns:http",
			ports:  []EndpointPort{grpcPort},
			want:   []string{"10.0.0.1:http"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti, err := parseResolverTarget(parseTarget(tc.target))
			if err != nil {
				t.Fatal(err)
			}

			k := &kResolver{target: ti}

			s := slice("svc-a", "10.0.0.1")
			s.Ports = tc.ports

			var got []string
			for _, a := range k.makeAddresses(s) {
				got = append(got, a.Addr)
			}

			assert.Equal(t, tc.want, got)
		})
	}
}
