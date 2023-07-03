package kuberesolver

import "os"

const (
	kubernetesNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	defaultNamespace        = "default"
)

func getCurrentNamespaceOrDefault() string {
	ns, err := os.ReadFile(kubernetesNamespaceFile)
	if err != nil {
		return defaultNamespace
	}
	return string(ns)
}
