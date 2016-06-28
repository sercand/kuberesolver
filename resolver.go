package kuberesolver

import (
	"fmt"
	"net/http"
	"net/url"

	"google.golang.org/grpc/naming"
)

// Resolver resolves service names using Kubernetes endpoints.
type kubeResolver struct {
	K8sClient       *k8sClient
	Namespace       string
	target          targetInfo
	resourceVersion string
}

// NewResolver returns a new Kubernetes resolver.
func newResolver(client *k8sClient, namespace string, targetInfo targetInfo) kubeResolver {
	if namespace == "" {
		namespace = "default"
	}
	return kubeResolver{client, namespace, targetInfo, "0"}
}

// Resolve creates a Kubernetes watcher for the named target.
func (r *kubeResolver) Resolve(target string) (naming.Watcher, error) {
	u, err := url.Parse(fmt.Sprintf("%s/api/v1/watch/namespaces/%s/endpoints/%s",
		r.K8sClient.host, r.Namespace, target))
	if err != nil {
		return nil, err
	}
	// Calls to the Kubernetes endpoints watch API must include the resource
	// version to ensure watches only return updates since the last watch.
	q := u.Query()
	q.Set("resourceVersion", r.resourceVersion)
	u.RawQuery = q.Encode()
	req, err := r.K8sClient.getRequest(u.String())
	if err != nil {
		return nil, err
	}
	resp, err := r.K8sClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("invalid response code")
	}
	w := &Watcher{
		target:          r.target,
		watcher:         newStreamWatcher(resp.Body),
		endpoints:       make(map[string]interface{}),
		done:            make(chan struct{}),
		result:          make(chan watchResult),
		resourceVersion: "0",
	}
	return w, nil
}
