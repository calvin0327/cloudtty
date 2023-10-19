package workpool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/rand"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/cloudtty/cloudtty/pkg/constants"
	"github.com/cloudtty/cloudtty/pkg/manifests"
	util "github.com/cloudtty/cloudtty/pkg/utils"
)

var (
	defaultRefreshWorkerQueueDuration = time.Minute * 3
	ControllerFinalizer               = "cloudshell.cloudtty.io/worker-pool"
)

type WorkerPool struct {
	sync.Mutex
	namespace string
	clienset  clientset.Interface

	coreWorkerLimit            int
	maxWorkerLimit             int
	workerQueue                Interface
	checkAndCreateWorkSignal   chan struct{}
	isDelayWorkerQueue         bool
	refreshWorkerQueueDuration time.Duration

	queue       workqueue.RateLimitingInterface
	podInformer cache.SharedIndexInformer
	podLister   listerscorev1.PodLister
}

func New(namespace string, clientSet clientset.Interface, coreQueueLimit, maxWorkerLimit int,
	isDelayWorkerQueue bool, podInformer informercorev1.PodInformer) *WorkerPool {
	workerPool := &WorkerPool{
		clienset:                   clientSet,
		namespace:                  namespace,
		workerQueue:                newQueue(),
		coreWorkerLimit:            coreQueueLimit,
		maxWorkerLimit:             maxWorkerLimit,
		checkAndCreateWorkSignal:   make(chan struct{}),
		isDelayWorkerQueue:         isDelayWorkerQueue,
		refreshWorkerQueueDuration: defaultRefreshWorkerQueueDuration,

		queue: workqueue.NewRateLimitingQueue(
			workqueue.NewItemExponentialFailureRateLimiter(2*time.Second, 5*time.Second),
		),
		podInformer: podInformer.Informer(),
		podLister:   podInformer.Lister(),
	}

	if _, err := podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				workerPool.enqueue(obj)
			},
			UpdateFunc: func(_, newObj interface{}) {
				workerPool.enqueue(newObj)
			},
			DeleteFunc: func(obj interface{}) {
				workerPool.enqueue(obj)
			},
		},
	); err != nil {
		klog.ErrorS(err, "error when adding event handler to informer")
	}

	return workerPool
}

func (w *WorkerPool) enqueue(obj interface{}) {
	pod := obj.(*corev1.Pod)
	if isIdleWorker(pod) {
		key, _ := cache.MetaNamespaceKeyFunc(obj)
		w.queue.Add(key)
	}
}

func (w *WorkerPool) Borrow() (*corev1.Pod, error) {
	w.checkAndCreateWorkSignal <- struct{}{}

	item := w.workerQueue.Get()
	if item == nil {
		return nil, fmt.Errorf("the pool is empty")
	}
	objKey := item.(string)
	namespace, name, _ := cache.SplitMetaNamespaceKey(objKey)

	var pod *corev1.Pod
	retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		pod, err = w.podLister.Pods(namespace).Get(name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
		}

		var updated bool
		if pod.Labels != nil {
			if _, exist := pod.Labels[constants.CloudshellIdleWorkerKey]; exist {
				delete(pod.Labels, constants.CloudshellIdleWorkerKey)
				updated = true
			}
		}
		if controllerutil.RemoveFinalizer(pod, ControllerFinalizer) || updated {
			_, err = w.clienset.CoreV1().Pods(namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
		}

		return err
	})

	// TODO: the pod is nil?
	return pod, nil
}

func (w *WorkerPool) Back(worker *corev1.Pod) error {
	if worker != nil {
		retry.RetryOnConflict(retry.DefaultRetry, func() error {
			worker, err := w.podLister.Pods(worker.Namespace).Get(worker.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}

			controllerutil.AddFinalizer(worker, ControllerFinalizer)

			if worker.Labels == nil {
				worker.Labels = map[string]string{}
			}
			worker.Labels[constants.CloudshellIdleWorkerKey] = ""
			_, err = w.clienset.CoreV1().Pods(worker.Namespace).Update(context.TODO(), worker, metav1.UpdateOptions{})
			return err
		})

		if w.workerQueue.Len() < w.maxWorkerLimit {
			objKey, _ := cache.MetaNamespaceKeyFunc(worker)
			w.workerQueue.Add(objKey)
		} else {
			if err := w.deleteWorker(worker); err != nil {
				klog.ErrorS(err, "failed to delete worker")
			}
		}
	}

	return nil
}

func (w *WorkerPool) Run(worker int, stopCh <-chan struct{}) {
	if !cache.WaitForCacheSync(stopCh, w.podInformer.HasSynced) {
		klog.Errorf("cloudshell manager: wait for informer factory failed")
	}

	go w.tryRefreshWorkerQueue(stopCh)

	if !w.isDelayWorkerQueue {
		w.checkAndCreateWorkSignal <- struct{}{}
	}

	w.run(worker, stopCh)
}

func (w *WorkerPool) run(workers int, stopCh <-chan struct{}) {
	var waitGroup sync.WaitGroup
	for i := 0; i < workers; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			w.worker(stopCh)
		}()
	}

	<-stopCh

	w.queue.ShutDown()
	waitGroup.Wait()
}

func (w *WorkerPool) worker(stopCh <-chan struct{}) {
	for w.processNextCluster() {
		select {
		case <-stopCh:
			return
		default:
		}
	}
}

func (w *WorkerPool) processNextCluster() (continued bool) {
	key, shutdown := w.queue.Get()
	if shutdown {
		return false
	}
	defer w.queue.Done(key)
	continued = true

	namespace, name, err := cache.SplitMetaNamespaceKey(key.(string))
	if err != nil {
		klog.ErrorS(err, "failed to split pod key", "key", key)
		return
	}

	pod, err := w.podLister.Pods(namespace).Get(name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "failed to get pod from lister", "policy", name)
			return
		}
		return
	}

	pod = pod.DeepCopy()
	if err := w.reconcilePod(pod); err != nil {
		klog.ErrorS(err, "failed to reconcile pod", "pod", name, "num requeues", w.queue.NumRequeues(key))
		w.queue.Add(key)
		return true
	}

	w.queue.Forget(key)
	return
}

func (w *WorkerPool) reconcilePod(pod *corev1.Pod) error {
	if !pod.DeletionTimestamp.IsZero() {
		key, _ := cache.MetaNamespaceKeyFunc(pod)
		w.workerQueue.Remove(key)

		newer := pod.DeepCopy()
		retry.RetryOnConflict(retry.DefaultRetry, func() error {
			newer, err := w.podLister.Pods(newer.Namespace).Get(newer.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
			}
			if !controllerutil.RemoveFinalizer(newer, ControllerFinalizer) {
				return nil
			}
			_, err = w.clienset.CoreV1().Pods(newer.Namespace).Update(context.TODO(), newer, metav1.UpdateOptions{})
			return err
		})
		return nil
	}

	if util.IsPodReady(pod) {
		key, _ := cache.MetaNamespaceKeyFunc(pod)
		w.workerQueue.Add(key)
	}

	return nil
}

func (w *WorkerPool) tryRefreshWorkerQueue(stop <-chan struct{}) {
	t := time.NewTimer(w.refreshWorkerQueueDuration)
	for {
		select {
		case <-t.C:
			w.refreshWorkerQueue()
		case <-w.checkAndCreateWorkSignal:
			w.refreshWorkerQueue()
		case <-stop:
			return
		}
	}
}

func (w *WorkerPool) refreshWorkerQueue() {
	w.Lock()
	defer w.Unlock()

	pods, err := w.clienset.CoreV1().Pods(w.namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.Set{constants.CloudshellIdleWorkerKey: ""}.String(),
	})
	if err != nil {
		klog.ErrorS(err, "error when listing pod from informer cache")
	}

	for _, pod := range pods.Items {
		if !isIdleWorker(&pod) {
			continue
		}

		if w.workerQueue.Len() < w.maxWorkerLimit {
			if util.IsPodReady(&pod) {
				key, _ := cache.MetaNamespaceKeyFunc(pod)
				w.workerQueue.Add(key)
			}

			// TODO: if the pod failed?
		} else {
			if err := w.deleteWorker(&pod); err != nil {
				klog.ErrorS(err, "error when deleting pod from informer cache")
			}
		}
	}

	if len(pods.Items) < w.coreWorkerLimit {
		for i := 0; i < w.coreWorkerLimit-len(pods.Items); i++ {
			if err := w.createWorker(); err != nil {
				klog.ErrorS(err, "failed to create worker")
			}
		}
	}
}

func (w *WorkerPool) createWorker() error {
	podBytes, err := util.ParseTemplate(manifests.PodTmplV1, struct {
		Name, Namespace string
	}{
		Name:      fmt.Sprintf("cloudshell-worker-%s", rand.String(5)),
		Namespace: w.namespace,
	})
	if err != nil {
		return errors.Wrap(err, "failed create cloudshell job")
	}

	decoder := scheme.Codecs.UniversalDeserializer()
	obj, _, err := decoder.Decode(podBytes, nil, nil)
	if err != nil {
		klog.ErrorS(err, "failed to decode pod manifest")
		return err
	}
	pod := obj.(*corev1.Pod)

	pod.SetLabels(map[string]string{constants.CloudshellIdleWorkerKey: "", "worker-name": pod.Name})
	controllerutil.AddFinalizer(pod, ControllerFinalizer)

	_, err = w.clienset.CoreV1().Pods(w.namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	return nil
}

func (w *WorkerPool) deleteWorker(worker *corev1.Pod) error {
	return w.clienset.CoreV1().Pods(worker.GetNamespace()).Delete(context.TODO(), worker.GetName(), metav1.DeleteOptions{})
}

func isIdleWorker(worker *corev1.Pod) bool {
	if worker.Labels != nil {
		_, exist := worker.Labels[constants.CloudshellIdleWorkerKey]
		return exist
	}

	return true
}
