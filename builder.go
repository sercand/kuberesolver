package kuberesolver

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/resolver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	kubernetesSchema = "kubernetes"
	defaultFreq      = time.Minute * 30
	resyncDisabled   = 0
)

type targetInfo struct {
	serviceName       string
	serviceNamespace  string
	port              string
	resolveByPortName bool
	useFirstPort      bool
}

func (ti targetInfo) String() string {
	return fmt.Sprintf("kubernetes://%s/%s:%s", ti.serviceNamespace, ti.serviceName, ti.port)
}

// RegisterInCluster registers the kuberesolver builder to grpc with kubernetes schema.
func RegisterInCluster() error {
	return RegisterInClusterWithOptions(ClusterOptions{})
}

// RegisterInClusterWithSchema registers the kuberesolver builder to the grpc with custom schema.
func RegisterInClusterWithSchema(schema string) error {
	builder, err := NewBuilder(ClusterOptions{
		Schema: schema,
	})
	if err != nil {
		return err
	}
	resolver.Register(builder)
	return nil
}

type ClusterOptions struct {
	// Schema defaults to kubernetesSchema.
	Schema string
	// KubeClient defaults to an in cluster client.
	KubeClient kubernetes.Interface
	// PromRegister will default to the global registry unless
	// passed.
	PromRegister prometheus.Registerer
}

func RegisterInClusterWithOptions(opts ClusterOptions) error {
	builder, err := NewBuilder(opts)
	if err != nil {
		return err
	}
	resolver.Register(builder)
	return nil
}

// NewBuilder creates a kubeBuilder which is used by grpc resolver.
func NewBuilder(opts ClusterOptions) (resolver.Builder, error) {
	if opts.Schema == "" {
		opts.Schema = kubernetesSchema
	}
	if opts.KubeClient == kubernetes.Interface(nil) {
		kubeCfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		opts.KubeClient, err = kubernetes.NewForConfig(kubeCfg)
		if err != nil {
			return nil, err
		}
	}
	if opts.PromRegister == prometheus.Registerer(nil) {
		opts.PromRegister = prometheus.DefaultRegisterer
	}

	endpointsGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kuberesolver_endpoints_total",
			Help: "The number of endpoints for a given target",
		},
		[]string{"target"},
	)
	// Will fail for duplicate registration calls. Should only happen in tests.
	_ = opts.PromRegister.Register(endpointsGauge)

	addressesGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kuberesolver_addresses_total",
			Help: "The number of addresses for a given target",
		},
		[]string{"target"},
	)
	// Will fail for duplicate registration calls. Should only happen in tests.
	_ = opts.PromRegister.Register(addressesGauge)

	return &kubeBuilder{
		k8sClient:          opts.KubeClient,
		schema:             opts.Schema,
		endpointsForTarget: endpointsGauge,
		addressesForTarget: addressesGauge,
	}, nil
}

type kubeBuilder struct {
	k8sClient          kubernetes.Interface
	schema             string
	endpointsForTarget *prometheus.GaugeVec
	addressesForTarget *prometheus.GaugeVec
}

func splitServicePortNamespace(hpn string) (service, port, namespace string) {
	service = hpn

	colon := strings.LastIndexByte(service, ':')
	if colon != -1 {
		service, port = service[:colon], service[colon+1:]
	}

	// we want to split into the service name, namespace, and whatever else is left
	// this will support fully qualified service names, e.g. {service-name}.<namespace>.svc.<cluster-domain-name>.
	// Note that since we lookup the endpoints by service name and namespace, we don't care about the
	// cluster-domain-name, only that we can parse out the service name and namespace properly.
	parts := strings.SplitN(service, ".", 3)
	if len(parts) >= 2 {
		service, namespace = parts[0], parts[1]
	}

	return
}

func parseResolverTarget(target resolver.Target) (targetInfo, error) {
	var service, port, namespace string
	if target.URL.Host == "" {
		// kubernetes:///service.namespace:port
		service, port, namespace = splitServicePortNamespace(target.Endpoint())
	} else if target.URL.Port() == "" && target.Endpoint() != "" {
		// kubernetes://namespace/service:port
		service, port, _ = splitServicePortNamespace(target.Endpoint())
		namespace = target.URL.Hostname()
	} else {
		// kubernetes://service.namespace:port
		service, port, namespace = splitServicePortNamespace(target.URL.Host)
	}

	if service == "" {
		return targetInfo{}, fmt.Errorf("target %s must specify a service", &target.URL)
	}

	resolveByPortName := false
	useFirstPort := false
	if port == "" {
		useFirstPort = true
	} else if _, err := strconv.Atoi(port); err != nil {
		resolveByPortName = true
	}

	return targetInfo{
		serviceName:       service,
		serviceNamespace:  namespace,
		port:              port,
		resolveByPortName: resolveByPortName,
		useFirstPort:      useFirstPort,
	}, nil
}

// Build creates a new resolver for the given target.
//
// gRPC dial calls Build synchronously, and fails if the returned error is
// not nil.
func (b *kubeBuilder) Build(target resolver.Target, cc resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	ti, err := parseResolverTarget(target)
	if err != nil {
		return nil, err
	}
	if ti.serviceNamespace == "" {
		ti.serviceNamespace = getCurrentNamespaceOrDefault()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &kResolver{
		target:    ti,
		ctx:       ctx,
		cancel:    cancel,
		cc:        cc,
		k8sClient: b.k8sClient,
		t:         time.NewTimer(defaultFreq),
		freq:      defaultFreq,
		endpoints: b.endpointsForTarget.WithLabelValues(ti.String()),
		addresses: b.addressesForTarget.WithLabelValues(ti.String()),
	}

	factory := informers.NewSharedInformerFactoryWithOptions(b.k8sClient, resyncDisabled,
		informers.WithNamespace(r.target.serviceNamespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			// Watching a single Endpoints object.
			opts.FieldSelector = fields.OneTermEqualSelector(
				metav1.ObjectNameField, r.target.serviceName).String()
		}))
	informer := factory.Core().V1().Endpoints().Informer()
	reg, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			e, ok := obj.(*corev1.Endpoints)
			if !ok {
				return
			}
			r.handleEndpointsUpdate(e)
		},
		UpdateFunc: func(_, newObj any) {
			e, ok := newObj.(*corev1.Endpoints)
			if !ok {
				return
			}
			r.handleEndpointsUpdate(e)
		},
		DeleteFunc: func(obj any) {
			r.handleEndpointsUpdate(nil)
		},
	})
	if err != nil {
		return nil, err
	}
	if reg.HasSynced() {
		return nil, fmt.Errorf("kuberesolve cannot sync with kubeinformer for target: %s", target.URL.String())
	}

	r.wg.Add(1)
	go func() {
		defer func() {
			handleCrash()
			r.wg.Done()
		}()
		informer.Run(r.ctx.Done())
	}()
	return r, nil
}

// Scheme returns the scheme supported by this resolver.
// Scheme is defined at https://github.com/grpc/grpc/blob/master/doc/naming.md.
func (b *kubeBuilder) Scheme() string {
	return b.schema
}

type kResolver struct {
	target    targetInfo
	ctx       context.Context
	cancel    context.CancelFunc
	cc        resolver.ClientConn
	k8sClient kubernetes.Interface
	// wg is used to enforce Close() to return after the watcher() goroutine has finished.
	wg   sync.WaitGroup
	t    *time.Timer
	freq time.Duration

	endpoints prometheus.Gauge
	addresses prometheus.Gauge
}

// ResolveNow is a no-op in this implementation.
func (k *kResolver) ResolveNow(resolver.ResolveNowOptions) {}

// Close closes the resolver.
func (k *kResolver) Close() {
	k.cancel()
	k.wg.Wait()
}

func (k *kResolver) makeAddresses(e *corev1.Endpoints) []resolver.Address {
	var newAddrs []resolver.Address
	if e == nil {
		// Handles the deletion case.
		return newAddrs
	}
	for _, subset := range e.Subsets {
		port := k.extractPortFromSubset(&subset)
		for _, address := range subset.Addresses {
			newAddrs = append(newAddrs, resolver.Address{
				Addr:       net.JoinHostPort(address.IP, port),
				ServerName: fmt.Sprint(k.target.serviceName, ".", k.target.serviceNamespace),
			})
		}
	}
	return newAddrs
}

func (k *kResolver) extractPortFromSubset(subset *corev1.EndpointSubset) string {
	if k.target.useFirstPort {
		return strconv.Itoa(int(subset.Ports[0].Port))
	}
	if k.target.resolveByPortName {
		for _, p := range subset.Ports {
			if p.Name == k.target.port {
				return strconv.Itoa(int(p.Port))
			}
		}
	}
	if port := k.target.port; len(port) != 0 {
		return port
	}
	return strconv.Itoa(int(subset.Ports[0].Port))
}

func (k *kResolver) handleEndpointsUpdate(e *corev1.Endpoints) {
	addrs := k.makeAddresses(e)
	if len(addrs) > 0 {
		// TODO: migrate to UpdateState.
		k.cc.NewAddress(addrs)
	}
	k.endpoints.Set(float64(len(e.Subsets)))
	k.addresses.Set(float64(len(addrs)))
}
