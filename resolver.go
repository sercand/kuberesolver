package kuberesolver

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/naming"
)

// Resolver resolves service names using Kubernetes endpoints.
type kubeResolver struct {
	k8sClient       *k8sClient
	namespace       string
	target          targetInfo
	resourceVersion string
	watcher         *Watcher
}

// NewResolver returns a new Kubernetes resolver.
func newResolver(client *k8sClient, namespace string, targetInfo targetInfo) *kubeResolver {
	if namespace == "" {
		namespace = "default"
	}
	return &kubeResolver{client, namespace, targetInfo, "0", nil}
}

// Resolve creates a Kubernetes watcher for the named target.
func (r *kubeResolver) Resolve(target string) (naming.Watcher, error) {
	resultChan := make(chan watchResult)
	stopCh := make(chan struct{})

	go Until(func() {
		err := r.resolveWrapper(target, stopCh, resultChan)
		grpclog.Printf("kuberesolver/resolve.go: ResolveWrapper ended with error=%v", err)
	}, time.Second, stopCh)

	w := &Watcher{
		target:    r.target,
		endpoints: make(map[string]interface{}),
		stopCh:    stopCh,
		result:    resultChan,
	}
	r.watcher = w
	return w, nil
}

func (r *kubeResolver) resolveWrapper(target string, stopCh <-chan struct{}, resultCh chan<- watchResult) error {
	u, err := url.Parse(fmt.Sprintf("%s/api/v1/watch/namespaces/%s/endpoints/%s",
		r.k8sClient.host, r.namespace, target))
	if err != nil {
		return err
	}
	// Calls to the Kubernetes endpoints watch API must include the resource
	// version to ensure watches only return updates since the last watch.
	//	q := u.Query()
	//	q.Set("resourceVersion", "0")
	//u.RawQuery = q.Encode()

	grpclog.Printf("kuberesolver/resolve.go: ResolveWrapper start url=%s", u.String())

	req, err := r.k8sClient.getRequest(u.String())
	if err != nil {
		grpclog.Printf("kuberesolver/resolver.go: ResolveWrapper getRequest failed with error=%v", err)
		return err
	}
	resp, err := r.k8sClient.Do(req)
	if err != nil {
		grpclog.Printf("kuberesolver/resolver.go: ResolveWrapper request failed with error=%v", err)
		return err
	}
	if resp.StatusCode != http.StatusOK {
		grpclog.Printf("kuberesolver/resolve.go: ResolveWrapper invalid response code=%d", resp.StatusCode)
		//	defer resp.Body.Close()
		//	return fmt.Errorf("invalid response code %d", resp.StatusCode)
	}
	sw := newStreamWatcher(resp.Body)
	for {
		select {
		case <-stopCh:
			grpclog.Printf("kuberesolver/resolver.go: ResolveWrapper stop Channel")
			return nil
		case up, more := <-sw.ResultChan():
			if more {
				//	r.resourceVersion = up.Object.Metadata.ResourceVersion
				resultCh <- watchResult{err: nil, ep: &up}
			} else {
				grpclog.Printf("kuberesolver/resolver.go: ResolveWrapper stop ResultChan")
				return nil
			}
		}
	}
}
