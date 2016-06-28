package kuberesolver

import (
	"net"
	"strconv"

	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/naming"
)

type watchResult struct {
	ep  *Event
	err error
}

// A Watcher provides name resolution updates from Kubernetes endpoints
// identified by name. Updates consists of pod IPs and the first port
// defined on the endpoint.
type Watcher struct {
	watcher         watchInterface
	target          targetInfo
	endpoints       map[string]interface{}
	done            chan struct{}
	result          chan watchResult
	resourceVersion string
}

// Close closes the watcher, cleaning up any open connections.
func (w *Watcher) Close() {
	w.done <- struct{}{}
	w.watcher.Stop()
}

// Next updates the endpoints for the name being watched.
func (w *Watcher) Next() ([]*naming.Update, error) {
	updates := make([]*naming.Update, 0)
	updatedEndpoints := make(map[string]interface{})
	var ep Event

	select {
	case <-w.done:
		grpclog.Printf("kuberesolver/watcher.go: w.done channel")
		return updates, nil
	case r := <-w.watcher.ResultChan():
		ep = r
	}

	for _, subset := range ep.Object.Subsets {
		port := ""
		if w.target.useFirstPort {
			if len(subset.Ports) > 0 {
				port = strconv.Itoa(subset.Ports[0].Port)
			}
		} else if w.target.resolveByPortName {
			for _, p := range subset.Ports {
				if p.Name == w.target.port {
					port = strconv.Itoa(p.Port)
					break
				}
			}
		} else {
			port = w.target.port
		}

		if len(port) == 0 {
			if len(subset.Ports) > 0 {
				port = strconv.Itoa(subset.Ports[0].Port)
			} else {
				//does not any available port
				continue
			}
		}
		for _, address := range subset.Addresses {
			endpoint := net.JoinHostPort(address.IP, port)
			updatedEndpoints[endpoint] = nil
		}
	}

	// Create updates to add new endpoints.
	for addr, md := range updatedEndpoints {
		if _, ok := w.endpoints[addr]; !ok {
			updates = append(updates, &naming.Update{naming.Add, addr, md})
			grpclog.Printf("kuberesolver/watcher.go: %s ADDED", addr)
		}
	}

	// Create updates to delete old endpoints.
	for addr := range w.endpoints {
		if _, ok := updatedEndpoints[addr]; !ok {
			updates = append(updates, &naming.Update{naming.Delete, addr, nil})
			grpclog.Printf("kuberesolver/watcher.go: %s DELETED", addr)
		}
	}
	grpclog.Printf("kuberesolver/watcher.go: current endpoints %+v", updatedEndpoints)
	w.endpoints = updatedEndpoints
	return updates, nil
}
