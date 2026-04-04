/*
Copyright 2018 Planet Labs Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/oklog/run"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"github.com/alecthomas/kingpin/v2"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	"github.com/maskshell/draino/internal/kubernetes"
)

// Default leader election settings.
const (
	DefaultLeaderElectionLeaseDuration time.Duration = 15 * time.Second
	DefaultLeaderElectionRenewDeadline time.Duration = 10 * time.Second
	DefaultLeaderElectionRetryPeriod   time.Duration = 2 * time.Second
)

func main() {
	var (
		app = kingpin.New(filepath.Base(os.Args[0]), "Automatically cordons and drains nodes that match the supplied conditions.").DefaultEnvars()

		debug            = app.Flag("debug", "Run with debug logging.").Short('d').Bool()
		listen           = app.Flag("listen", "Address at which to expose /metrics and /healthz.").Default(":10002").String()
		kubecfg          = app.Flag("kubeconfig", "Path to kubeconfig file. Leave unset to use in-cluster config.").String()
		apiserver        = app.Flag("master", "Address of Kubernetes API server. Leave unset to use in-cluster config.").String()
		dryRun           = app.Flag("dry-run", "Emit an event without cordoning or draining matching nodes.").Bool()
		maxGracePeriod   = app.Flag("max-grace-period", "Maximum time evicted pods will be given to terminate gracefully.").Default(kubernetes.DefaultMaxGracePeriod.String()).Duration()
		evictionHeadroom = app.Flag("eviction-headroom", "Additional time to wait after a pod's termination grace period for it to have been deleted.").Default(kubernetes.DefaultEvictionOverhead.String()).Duration()
		drainBuffer      = app.Flag("drain-buffer", "Minimum time between starting each drain. Nodes are always cordoned immediately.").Default(kubernetes.DefaultDrainBuffer.String()).Duration()
		nodeLabels       = app.Flag("node-label", "(Deprecated) Nodes with this label will be eligible for cordoning and draining. May be specified multiple times").Strings()
		nodeLabelsExpr   = app.Flag("node-label-expr", "Nodes that match this expression will be eligible for cordoning and draining.").String()
		namespace        = app.Flag("namespace", "Namespace used to create leader election lock object.").Default("kube-system").String()

		leaderElectionLeaseDuration = app.Flag("leader-election-lease-duration", "Lease duration for leader election.").Default(DefaultLeaderElectionLeaseDuration.String()).Duration()
		leaderElectionRenewDeadline = app.Flag("leader-election-renew-deadline", "Leader election renew deadline.").Default(DefaultLeaderElectionRenewDeadline.String()).Duration()
		leaderElectionRetryPeriod   = app.Flag("leader-election-retry-period", "Leader election retry period.").Default(DefaultLeaderElectionRetryPeriod.String()).Duration()
		leaderElectionTokenName     = app.Flag("leader-election-token-name", "Leader election token name.").Default(kubernetes.Component).String()

		skipDrain             = app.Flag("skip-drain", "Whether to skip draining nodes after cordoning.").Default("false").Bool()
		evictDaemonSetPods    = app.Flag("evict-daemonset-pods", "Evict pods that were created by an extant DaemonSet.").Bool()
		evictStatefulSetPods  = app.Flag("evict-statefulset-pods", "Evict pods that were created by an extant StatefulSet.").Bool()
		evictLocalStoragePods = app.Flag("evict-emptydir-pods", "Evict pods with local storage, i.e. with emptyDir volumes.").Bool()
		evictUnreplicatedPods = app.Flag("evict-unreplicated-pods", "Evict pods that were not created by a replication controller.").Bool()

		protectedPodAnnotations = app.Flag("protected-pod-annotation", "Protect pods with this annotation from eviction. May be specified multiple times.").PlaceHolder("KEY[=VALUE]").Strings()

		conditions = app.Arg("node-conditions", "Nodes for which any of these conditions are true will be cordoned and drained.").Required().Strings()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	// this is required to make all packages using klog write to stderr instead of tmp files
	klog.InitFlags(nil)

	// Set up OpenTelemetry Prometheus exporter
	registry := prometheus.NewRegistry()
	otelPromExporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	kingpin.FatalIfError(err, "cannot create Prometheus exporter")
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelPromExporter))
	defer meterProvider.Shutdown(context.Background()) //nolint:errcheck
	meter := meterProvider.Meter(kubernetes.Component)

	metrics, err := kubernetes.InitMetrics(meter)
	kingpin.FatalIfError(err, "cannot create metrics")

	web := &httpRunner{l: *listen, h: map[string]http.Handler{
		"/metrics": promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		"/healthz": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	}}

	log, err := zap.NewProduction()
	if *debug {
		log, err = zap.NewDevelopment()
	}
	kingpin.FatalIfError(err, "cannot create log")
	defer log.Sync() //nolint:errcheck

	go func() {
		log.Info("web server is running", zap.String("listen", *listen))
		kingpin.FatalIfError(await(web), "error serving")
	}()

	c, err := kubernetes.BuildConfigFromFlags(*apiserver, *kubecfg)
	kingpin.FatalIfError(err, "cannot create Kubernetes client configuration")

	cs, err := kubeclient.NewForConfig(c)
	kingpin.FatalIfError(err, "cannot create Kubernetes client")

	pf := []kubernetes.PodFilterFunc{kubernetes.MirrorPodFilter}
	if !*evictLocalStoragePods {
		pf = append(pf, kubernetes.LocalStoragePodFilter)
	}
	if !*evictUnreplicatedPods {
		pf = append(pf, kubernetes.UnreplicatedPodFilter)
	}
	if !*evictDaemonSetPods {
		pf = append(pf, kubernetes.NewDaemonSetPodFilter(cs))
	}
	if !*evictStatefulSetPods {
		pf = append(pf, kubernetes.NewStatefulSetPodFilter(cs))
	}
	systemKnownAnnotations := []string{
		"cluster-autoscaler.kubernetes.io/safe-to-evict=false", // https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md#what-types-of-pods-can-prevent-ca-from-removing-a-node
	}
	pf = append(pf, kubernetes.UnprotectedPodFilter(append(systemKnownAnnotations, *protectedPodAnnotations...)...))
	var h cache.ResourceEventHandler = kubernetes.NewDrainingResourceEventHandler(
		kubernetes.NewAPICordonDrainer(cs,
			kubernetes.MaxGracePeriod(*maxGracePeriod),
			kubernetes.EvictionHeadroom(*evictionHeadroom),
			kubernetes.WithSkipDrain(*skipDrain),
			kubernetes.WithPodFilter(kubernetes.NewPodFilters(pf...)),
			kubernetes.WithAPICordonDrainerLogger(log),
		),
		kubernetes.NewEventRecorder(cs),
		kubernetes.WithLogger(log),
		kubernetes.WithDrainBuffer(*drainBuffer),
		kubernetes.WithConditionsFilter(*conditions),
		kubernetes.WithMetrics(metrics))

	if *dryRun {
		h = cache.FilteringResourceEventHandler{
			FilterFunc: kubernetes.NewNodeProcessed().Filter,
			Handler: kubernetes.NewDrainingResourceEventHandler(
				&kubernetes.NoopCordonDrainer{},
				kubernetes.NewEventRecorder(cs),
				kubernetes.WithLogger(log),
				kubernetes.WithDrainBuffer(*drainBuffer),
				kubernetes.WithConditionsFilter(*conditions),
				kubernetes.WithMetrics(metrics)),
		}
	}

	if len(*nodeLabels) > 0 {
		log.Debug("node labels", zap.Any("labels", nodeLabels))
		if *nodeLabelsExpr != "" {
			kingpin.Fatalf("nodeLabels and NodeLabelsExpr cannot both be set")
		}
		if nodeLabelsExpr, err = kubernetes.ConvertLabelsToFilterExpr(*nodeLabels); err != nil {
			kingpin.Fatalf(err.Error())
		}
	}

	var nodeLabelFilter cache.ResourceEventHandler
	log.Debug("label expression", zap.Any("expr", nodeLabelsExpr))

	nodeLabelFilterFunc, err := kubernetes.NewNodeLabelFilter(nodeLabelsExpr, log)
	if err != nil {
		log.Fatal("Failed to parse node label expression", zap.Error(err))
	}

	nodeLabelFilter = cache.FilteringResourceEventHandler{FilterFunc: nodeLabelFilterFunc, Handler: h}

	nodes, err := kubernetes.NewNodeWatch(cs, nodeLabelFilter)
	kingpin.FatalIfError(err, "cannot create node watch")

	id, err := os.Hostname()
	kingpin.FatalIfError(err, "cannot get hostname")

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		*namespace,
		*leaderElectionTokenName,
		cs.CoreV1(),
		cs.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: kubernetes.NewEventRecorder(cs),
		},
	)
	kingpin.FatalIfError(err, "cannot create lock")

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: *leaderElectionLeaseDuration,
		RenewDeadline: *leaderElectionRenewDeadline,
		RetryPeriod:   *leaderElectionRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Info("node watcher is running")
				kingpin.FatalIfError(await(&informerRunner{inf: nodes}), "error watching")
			},
			OnStoppedLeading: func() {
				kingpin.Fatalf("lost leader election")
			},
		},
	})
}

type runner interface {
	Run(stop <-chan struct{}) error
}

// informerRunner adapts a cache.SharedInformer to the runner interface.
type informerRunner struct {
	inf cache.SharedInformer
}

func (r *informerRunner) Run(stop <-chan struct{}) error {
	r.inf.Run(stop)
	return nil
}

func await(rs ...runner) error {
	stop := make(chan struct{})
	g := &run.Group{}
	for i := range rs {
		r := rs[i] // https://golang.org/doc/faq#closures_and_goroutines
		g.Add(func() error { return r.Run(stop) }, func(err error) { close(stop) })
	}
	return g.Run()
}

type httpRunner struct {
	l string
	h map[string]http.Handler
}

func (r *httpRunner) Run(stop <-chan struct{}) error {
	rt := httprouter.New()
	for path, handler := range r.h {
		rt.Handler("GET", path, handler)
	}

	s := &http.Server{Addr: r.l, Handler: rt}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		<-stop
		s.Shutdown(ctx) //nolint:errcheck
	}()
	return s.ListenAndServe()
}
