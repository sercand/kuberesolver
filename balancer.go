package kuberesolver

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
)

type Balancer struct {
	Namespace string
	client    *k8sClient
	resolvers []*kubeResolver
}

type TargetUrlType int32

const (
	TargetTypeDNS        TargetUrlType = 0
	TargetTypeKubernetes TargetUrlType = 1
	kubernetesSchema                   = "kubernetes"
	dnsSchema                          = "dns"
)

type targetInfo struct {
	urlType           TargetUrlType
	target            string
	port              string
	resolveByPortName bool
	useFirstPort      bool
}

func parseTarget(target string) (targetInfo, error) {
	u, err := url.Parse(target)
	if err != nil {
		return targetInfo{}, err
	}
	ti := targetInfo{}
	if u.Scheme == kubernetesSchema {
		ti.urlType = TargetTypeKubernetes
		spl := strings.Split(u.Host, ":")
		if len(spl) == 2 {
			ti.target = spl[0]
			ti.port = spl[1]
			ti.useFirstPort = false
			if _, err := strconv.Atoi(ti.port); err != nil {
				ti.resolveByPortName = true
			} else {
				ti.resolveByPortName = false
			}
		} else {
			ti.target = spl[0]
			ti.useFirstPort = true
		}
	} else if u.Scheme == dnsSchema {
		ti.urlType = TargetTypeDNS
		ti.target = u.Host
	} else {
		ti.urlType = TargetTypeDNS
		ti.target = target
	}
	return ti, nil
}

// Dial calls grpc.Dial, also parses target and uses load balancer if necessary
func (b *Balancer) Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	pt, err := parseTarget(target)
	if err != nil {
		return nil, err
	}
	switch pt.urlType {
	case TargetTypeKubernetes:
		grpclog.Printf("kuberesolver/balancer.go: using kubernetes resolver target=%s", pt.target)
		rs := newResolver(b.client, b.Namespace, pt)
		b.resolvers = append(b.resolvers, rs)
		opts := append(opts, grpc.WithBalancer(grpc.RoundRobin(rs)))
		return grpc.Dial(pt.target, opts...)
	case TargetTypeDNS:
		return grpc.Dial(pt.target, opts...)
	default:
		return nil, errors.New("Unknown target type")
	}
}

func (b *Balancer) Healthy() error {
	for _, r := range b.resolvers {
		if r.watcher != nil {
			if len(r.watcher.endpoints) == 0 {
				return fmt.Errorf("target %s does not have endpoints", r.target.target)
			}
		}
	}
	return nil
}

func New() *Balancer {
	client, err := newInClusterClient()
	if err != nil {
		grpclog.Printf("kuberesolver/balancer.go: failed to create in cluster client, err=%v", err)
	}
	return &Balancer{
		Namespace: "default",
		client:    client,
	}
}

func NewWithNamespace(namespace string) *Balancer {
	client, _ := newInClusterClient()
	return &Balancer{
		Namespace: namespace,
		client:    client,
	}
}
