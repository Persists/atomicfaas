// The crash detector watches Knative user pods for container crashes and
// hands the in-flight invocation to the reverter via Redis.
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/redis/go-redis/v9"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	namespace := flag.String("namespace", envOr("NAMESPACE", "default"), "namespace to watch")
	service := flag.String("service", envOr("SERVICE", ""), "pin to one Knative service (empty = all)")
	redisAddr := flag.String("redis", envOr("REDIS_ADDR", "localhost:6379"), "Redis address")
	flag.Parse()

	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis unreachable at %s: %v", *redisAddr, err)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	}
	if err != nil {
		log.Fatalf("no kubernetes config (neither in-cluster nor kubeconfig): %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}

	selector := "serving.knative.dev/service"
	if *service != "" {
		selector += "=" + *service
	}
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithNamespace(*namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = selector }))
	pods := factory.Core().V1().Pods().Informer()

	d := newDetector(&handoff{rdb: rdb})
	pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: d.onUpdate,
		DeleteFunc: d.onDelete,
	})

	stop := make(chan struct{}) // never closed: informers run for the process lifetime
	log.Printf("crash-detector: watching namespace=%s selector=%q", *namespace, selector)
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	<-stop
}
