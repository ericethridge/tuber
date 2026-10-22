package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/ericethridge/tuber/internal/config"
	"github.com/ericethridge/tuber/internal/doit"
	"github.com/ericethridge/tuber/internal/formatter"
	"github.com/ericethridge/tuber/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Manager orchestrates per-namespace SharedInformers, smart rollup evaluation,
// deduplication, and asynchronous delivery to the DoiT Annotations API.
type Manager struct {
	cfg       *config.Config
	client    kubernetes.Interface
	sink      doit.Sink
	formatter *formatter.Formatter
	evaluator *Evaluator
	dedup     *Deduplicator
	queue     workqueue.TypedRateLimitingInterface[string]
	itemsMu   sync.Mutex
	items     map[string]model.WorkloadEvent
	factories []informers.SharedInformerFactory
	hasSynced []cache.InformerSynced
	synced    atomic.Bool
	logger    *slog.Logger
}

// NewManager creates and wires a Manager for all namespaces in cfg.WatchNamespaces.
func NewManager(
	cfg *config.Config,
	client kubernetes.Interface,
	sink doit.Sink,
	fmttr *formatter.Formatter,
	logger *slog.Logger,
) (*Manager, error) {
	if cfg == nil {
		return nil, errors.New("config cannot be nil")
	}
	if client == nil {
		return nil, errors.New("kubernetes client cannot be nil")
	}
	if sink == nil {
		return nil, errors.New("doit sink cannot be nil")
	}
	if fmttr == nil {
		return nil, errors.New("formatter cannot be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	m := &Manager{
		cfg:       cfg,
		client:    client,
		sink:      sink,
		formatter: fmttr,
		evaluator: NewEvaluator(cfg),
		dedup:     NewDeduplicator(cfg.Rollup.DedupTTL),
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		items:  make(map[string]model.WorkloadEvent),
		logger: logger,
	}

	if err := m.registerInformers(); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Manager) registerInformers() error {
	for _, ns := range m.cfg.WatchNamespaces {
		factory := informers.NewSharedInformerFactoryWithOptions(
			m.client,
			m.cfg.Rollup.ResyncPeriod,
			informers.WithNamespace(ns),
		)

		depInformer := factory.Apps().V1().Deployments().Informer()
		if _, err := depInformer.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
			AddFunc: func(obj any, isInInitialList bool) {
				if dep, ok := obj.(*appsv1.Deployment); ok {
					m.enqueueAll(m.evaluator.EvaluateDeploymentAdd(dep, isInInitialList))
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				oldDep, ok1 := oldObj.(*appsv1.Deployment)
				newDep, ok2 := newObj.(*appsv1.Deployment)
				if ok1 && ok2 {
					m.enqueueAll(m.evaluator.EvaluateDeploymentUpdate(oldDep, newDep))
				}
			},
			DeleteFunc: func(obj any) {
				m.enqueueAll(m.evaluator.EvaluateDeploymentDelete(obj))
			},
		}); err != nil {
			return fmt.Errorf("registering Deployment handler in namespace %q: %w", ns, err)
		}

		jobInformer := factory.Batch().V1().Jobs().Informer()
		if _, err := jobInformer.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
			AddFunc: func(obj any, isInInitialList bool) {
				if job, ok := obj.(*batchv1.Job); ok {
					m.enqueueAll(m.evaluator.EvaluateJobAdd(job, isInInitialList))
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				oldJob, ok1 := oldObj.(*batchv1.Job)
				newJob, ok2 := newObj.(*batchv1.Job)
				if ok1 && ok2 {
					m.enqueueAll(m.evaluator.EvaluateJobUpdate(oldJob, newJob))
				}
			},
		}); err != nil {
			return fmt.Errorf("registering Job handler in namespace %q: %w", ns, err)
		}

		podInformer := factory.Core().V1().Pods().Informer()
		if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
			AddFunc: func(obj any, isInInitialList bool) {
				if pod, ok := obj.(*corev1.Pod); ok {
					m.enqueueAll(m.evaluator.EvaluatePodAdd(pod, isInInitialList))
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				oldPod, ok1 := oldObj.(*corev1.Pod)
				newPod, ok2 := newObj.(*corev1.Pod)
				if ok1 && ok2 {
					m.enqueueAll(m.evaluator.EvaluatePodUpdate(oldPod, newPod))
				}
			},
			DeleteFunc: func(obj any) {
				m.enqueueAll(m.evaluator.EvaluatePodDelete(obj))
			},
		}); err != nil {
			return fmt.Errorf("registering Pod handler in namespace %q: %w", ns, err)
		}

		m.factories = append(m.factories, factory)
		m.hasSynced = append(m.hasSynced,
			depInformer.HasSynced,
			jobInformer.HasSynced,
			podInformer.HasSynced,
		)
	}
	return nil
}

func (m *Manager) enqueueAll(events []model.WorkloadEvent) {
	for _, ev := range events {
		fp := ev.Fingerprint()
		if !m.dedup.ShouldProcess(fp) {
			m.logger.Debug("suppressing duplicate workload event",
				"fingerprint", fp,
				"namespace", ev.Namespace,
				"kind", ev.Kind,
				"name", ev.Name,
				"action", ev.Action,
			)
			continue
		}
		m.itemsMu.Lock()
		m.items[fp] = ev
		m.itemsMu.Unlock()
		m.queue.Add(fp)
	}
}

// Ready reports whether all namespace informer caches have completed initial sync.
func (m *Manager) Ready() bool {
	return m.synced.Load()
}

// Start runs all namespace informers, waits for cache synchronization, starts worker
// goroutines, and blocks until ctx is canceled, draining in-flight annotations on exit.
func (m *Manager) Start(ctx context.Context) error {
	m.logger.Info("starting Kubernetes workload informers",
		"cluster", m.cfg.Cluster.Name,
		"provider", m.cfg.Cluster.Provider,
		"watchNamespaces", m.cfg.WatchNamespaces,
		"dryRun", m.cfg.DryRun,
	)

	for _, f := range m.factories {
		f.Start(ctx.Done())
	}

	if !cache.WaitForCacheSync(ctx.Done(), m.hasSynced...) {
		m.queue.ShutDown()
		return errors.New("failed to sync Kubernetes informer caches before context cancellation")
	}

	m.synced.Store(true)
	m.logger.Info("all namespace informer caches synced; watching for live workload events",
		"namespaces", m.cfg.WatchNamespaces,
	)

	workers := m.cfg.Rollup.Workers
	if workers <= 0 {
		workers = config.DefaultWorkerCount
	}

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			m.runWorker(workerCtx, workerID)
		}(i)
	}

	<-ctx.Done()
	m.logger.Info("shutting down watcher manager; draining in-flight DoiT annotations")
	m.queue.ShutDownWithDrain()
	cancelWorkers()
	wg.Wait()
	return nil
}

func (m *Manager) runWorker(ctx context.Context, _ int) {
	for m.processNextItem(ctx) {
	}
}

func (m *Manager) processNextItem(ctx context.Context) bool {
	fp, shutdown := m.queue.Get()
	if shutdown {
		return false
	}
	defer m.queue.Done(fp)

	m.itemsMu.Lock()
	ev, exists := m.items[fp]
	m.itemsMu.Unlock()
	if !exists {
		m.queue.Forget(fp)
		return true
	}

	req, err := m.formatter.Format(ev)
	if err != nil {
		m.logger.Error("failed to format workload event; dropping item",
			"error", err,
			"namespace", ev.Namespace,
			"kind", ev.Kind,
			"name", ev.Name,
		)
		m.forgetItem(fp)
		return true
	}

	resp, err := m.sink.CreateAnnotation(ctx, req)
	if err != nil {
		if doit.IsPermanent(err) || m.queue.NumRequeues(fp) >= m.cfg.DoiT.MaxRetries {
			m.logger.Error("dropping DoiT annotation after failure",
				"error", err,
				"requeues", m.queue.NumRequeues(fp),
				"content", req.Content,
			)
			m.forgetItem(fp)
			return true
		}
		m.logger.Warn("transient error creating DoiT annotation; requeuing",
			"error", err,
			"requeues", m.queue.NumRequeues(fp),
			"content", req.Content,
		)
		m.queue.AddRateLimited(fp)
		return true
	}

	m.forgetItem(fp)
	m.logger.Info("created DoiT Cloud Analytics annotation",
		"annotationId", resp.ID,
		"namespace", ev.Namespace,
		"kind", ev.Kind,
		"name", ev.Name,
		"action", ev.Action,
		"content", req.Content,
	)
	return true
}

func (m *Manager) forgetItem(fp string) {
	m.itemsMu.Lock()
	delete(m.items, fp)
	m.itemsMu.Unlock()
	m.queue.Forget(fp)
}
