# kuberesolver
Grpc Client-Side Load Balancer with Kubernetes name resolver

```go
// Register kuberesolver to grpc
kuberesolver.RegisterInCluster()
// is same as
resolver.Register(kuberesolver.NewBuilder(nil))
// you can bring your own k8s client, below is default behaviour
client, err := kuberesolver.NewInClusterK8sClient()
resolver.Register(kuberesolver.NewBuilder(client))

// USAGE:
// if schema is 'k8s' then grpc will use kuberesolver to resolve addresses
cc, err := grpc.Dial("k8s:///service-name.namespace:portname", opts...)
```

An url can be one of the following, [grpc naming docs](https://github.com/grpc/grpc/blob/master/doc/naming.md)
```
k8s:///service-name:8080
k8s:///service-name:portname
k8s:///service-name.namespace:8080
k8s:///service-name.namespace.svc:8080
k8s:///service-name.namespace.svc.cluster.local:8080

k8s://namespace/service-name:8080
k8s://service-name:8080/
k8s://service-name.namespace:8080/
```

To enable the GRPC warning log, add envrionment variable
```
GRPC_GO_LOG_SEVERITY_LEVEL: warning
```
