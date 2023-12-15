package workpool

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/rand"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudshellv1alpha2 "github.com/cloudtty/cloudtty/pkg/apis/cloudshell/v1alpha2"
	"github.com/cloudtty/cloudtty/pkg/constants"
	"github.com/cloudtty/cloudtty/pkg/manifests"
	util "github.com/cloudtty/cloudtty/pkg/utils"
)

var (
	// DefaultScaleInWorkerQueueDuration define the duration to scale in the workers.
	DefaultScaleInWorkerQueueDuration = time.Hour * 3
	ScaleInQueueThreshold             = 0.75
	ControllerFinalizer               = "cloudshell.cloudtty.io/worker-pool"
	NotWorkerErr                      = errors.New("There is no worker in pool")
)

type WorkerPool struct {
	sync.Mutex
	client.Client

	coreWorkerLimit    int
	maxWorkerLimit     int
	workerQueue        Interface
	requestQueue       Interface
	matchRequestSignal chan struct{}

	scaleInQueueDuration time.Duration

	queue       workqueue.RateLimitingInterface
	podInformer cache.SharedIndexInformer
	podLister   listerscorev1.PodLister
}

// Request represents a request to borrow a worker from worker pool. if the delay is true,
// the cloudshell queue must not be empty, because when the worker is running, it needs to
// inform the controller.
type Request struct {
	Cloudshell      string
	Namespace       string
	Image           string
	CloudShellQueue workqueue.RateLimitingInterface
}

func New(client client.Client, coreWorkerLimit, maxWorkerLimit int, podInformer informercorev1.PodInformer) *WorkerPool {
	workerPool := &WorkerPool{
		Client:               client,
		workerQueue:          newQueue(),
		requestQueue:         newQueue(),
		coreWorkerLimit:      coreWorkerLimit,
		maxWorkerLimit:       maxWorkerLimit,
		matchRequestSignal:   make(chan struct{}),
		scaleInQueueDuration: DefaultScaleInWorkerQueueDuration,

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

func (w *WorkerPool) Run(stopCh <-chan struct{}) {
	if !cache.WaitForCacheSync(stopCh, w.podInformer.HasSynced) {
		klog.Errorf("cloudshell manager: wait for informer factory failed")
	}

	w.run(stopCh)
}

func (w *WorkerPool) run(stopCh <-chan struct{}) {
	var waitGroup sync.WaitGroup
	waitGroup.Add(3)
	go func() {
		defer waitGroup.Done()
		w.worker(stopCh)
	}()

	go func() {
		defer waitGroup.Done()
		w.tryHandleRequestQueue(stopCh)
	}()

	go func() {
		defer waitGroup.Done()
		w.tryScaleInWorkerQueue(stopCh)
	}()

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
	if err := w.reconcile(pod); err != nil {
		klog.ErrorS(err, "failed to reconcile pod", "pod", name, "num requeues", w.queue.NumRequeues(key))
		w.queue.Add(key)
		return true
	}

	w.queue.Forget(key)
	return
}

func (w *WorkerPool) reconcile(worker *corev1.Pod) error {
	key, _ := WorkerKeyFunc(worker)

	// we need remove worker from worker queue when deleting the pod.
	if !worker.DeletionTimestamp.IsZero() {
		w.workerQueue.Remove(key)

		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			newer := &corev1.Pod{}
			err := w.Get(context.TODO(), client.ObjectKey{Namespace: worker.Namespace, Name: worker.Name}, newer)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
			}
			if !controllerutil.RemoveFinalizer(newer, ControllerFinalizer) {
				return nil
			}

			return w.Update(context.TODO(), newer)
		})
	}

	if !util.IsPodReady(worker) {
		w.workerQueue.Remove(key)
	} else {
		w.workerQueue.Add(key)
		w.matchRequestSignal <- struct{}{}
	}

	return nil
}

func (w *WorkerPool) tryHandleRequestQueue(stop <-chan struct{}) {
	t := time.NewTicker(time.Second * 5)
	for {
		select {
		case <-t.C:
			w.handlerandleRequestQueue()
		case <-w.matchRequestSignal:
			w.handlerandleRequestQueue()
		case <-stop:
			return
		}
	}
}

func (w *WorkerPool) handlerandleRequestQueue() {
	w.Lock()
	defer w.Unlock()

	for _, item := range w.requestQueue.All() {
		req := item.(Request)
		if _, err := w.matchWorkerFor(&req); err != nil {
			if err != NotWorkerErr {
				klog.ErrorS(err, "failed to create worker for cloudshell", req.Cloudshell)
			}

			pods, err := w.podLister.Pods(req.Namespace).List(labels.Set{constants.WorkerRequestLabelKey: req.Cloudshell}.AsSelector())
			if err != nil {
				klog.ErrorS(err, "failed to list worker for cloudshell", req.Cloudshell)
			}

			// TODO: pod is failed?

			if len(pods) == 0 {
				if err := w.createWorker(&req); err != nil {
					klog.ErrorS(err, "failed to create worker for cloudshell", req.Cloudshell)
				}
			}
		} else {
			cloudshell := &cloudshellv1alpha2.CloudShell{}
			if err = w.Get(context.TODO(), client.ObjectKey{Namespace: req.Namespace, Name: req.Cloudshell}, cloudshell); err != nil {
				if apierrors.IsNotFound(err) {
					w.requestQueue.Remove(req)
				}

				klog.ErrorS(err, "failed to get cloudshell", req.Cloudshell)
				continue
			}

			key, _ := cache.MetaNamespaceKeyFunc(cloudshell)
			req.CloudShellQueue.Add(key)

			w.requestQueue.Remove(req)
		}
	}
}

func (w *WorkerPool) tryScaleInWorkerQueue(stop <-chan struct{}) {
	t := time.NewTicker(w.scaleInQueueDuration)
	for {
		select {
		case <-t.C:
			w.scaleInWorkerQueue()
		case <-stop:
			return
		}
	}
}

func (w *WorkerPool) scaleInWorkerQueue() {
	workers, err := w.podLister.List(labels.Set{constants.WorkerOwnerLabelKey: ""}.AsSelector())
	if err != nil {
		klog.ErrorS(err, "error when listing pod from informer cache")
	}

	idelWorkers := []*corev1.Pod{}
	for _, pod := range workers {
		// the tolerance time of not available pod is 30 minutes, if
		// time out, we need to delete these pods.
		if util.IsPodNotAvailable(pod, 30*60) {
			if err := w.deleteWorker(pod); err != nil {
				klog.ErrorS(err, "failed to delete worker that is not available for 30 minute", klog.KObjs(pod))
			}
		}

		if util.IsPodReady(pod) {
			if req := w.matchRequestFor(pod); req == nil {
				idelWorkers = append(idelWorkers, pod)
			}
		}
	}

	if len(idelWorkers) > w.coreWorkerLimit {
		// Sort by number of bindings first, and then by chronological
		// order of creation if they are equal.
		sort.Slice(idelWorkers, func(i, j int) bool {
			one, two := idelWorkers[i], idelWorkers[j]

			c1 := one.Labels[constants.WorkerBindingCountLabelKey]
			c2 := two.Labels[constants.WorkerBindingCountLabelKey]
			n1, _ := strconv.ParseInt(c1, 10, 32)
			n2, _ := strconv.ParseInt(c2, 10, 32)

			if n1 == n2 {
				return one.CreationTimestamp.Before(&two.CreationTimestamp)
			}

			return n1 < n2
		})

		scaleNumber := float64(len(idelWorkers)-w.coreWorkerLimit) * ScaleInQueueThreshold
		len := int(math.Floor(scaleNumber + 0.5))

		for i := 0; i < len; i++ {
			worker := idelWorkers[i]
			if err := w.deleteWorker(worker); err != nil {
				klog.ErrorS(err, "failed to delete worker that needs to be scaled in", klog.KObjs(worker))
			} else {
				key, _ := WorkerKeyFunc(worker)
				w.workerQueue.Remove(key)
			}
		}
	}

	klog.V(2).Infof("the worker pool size is %d", w.workerQueue.Len())
}

func (w *WorkerPool) Borrow(req *Request) (*corev1.Pod, error) {
	worker, err := w.matchWorkerFor(req)
	if err != nil {
		if err == NotWorkerErr {
			// add the request to the request queue.
			w.requestQueue.Add(*req)
		}

		return nil, err
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if len(worker.Labels) > 0 {
			// recorde the binding count for worker, the count affects logic of gc.
			var count int64 = 1
			val, exist := worker.Labels[constants.WorkerBindingCountLabelKey]
			if exist {
				if num, err := strconv.ParseInt(val, 10, 32); err == nil {
					count = num + 1
				}
			}
			worker.Labels[constants.WorkerBindingCountLabelKey] = fmt.Sprint(count)

			worker.Labels[constants.WorkerOwnerLabelKey] = req.Cloudshell
			delete(worker.Labels, constants.WorkerRequestLabelKey)
		}

		controllerutil.RemoveFinalizer(worker, ControllerFinalizer)
		return w.Update(context.TODO(), worker)
	})
	if err != nil {
		return nil, err
	}

	// remove worker from queue.
	key, _ := WorkerKeyFunc(worker)
	w.workerQueue.Remove(key)

	return worker, nil
}

// matchWorkerRequest returns the worker name.
func (w *WorkerPool) matchWorkerFor(req *Request) (*corev1.Pod, error) {
	item := w.workerQueue.Match(func(item interface{}) bool {
		ns, _, image, _ := SplitWorkerKey(item.(string))
		return ns == req.Namespace && image == req.Image
	})
	if item != nil {
		_, name, _, _ := SplitWorkerKey(item.(string))
		worker, err := w.podLister.Pods(req.Namespace).Get(name)
		// TODO: if err is notfound?
		if err != nil {
			return nil, err
		}
		return worker, nil
	}

	return nil, NotWorkerErr
}

func (w *WorkerPool) matchRequestFor(worker *corev1.Pod) *Request {
	item := w.requestQueue.Match(func(item interface{}) bool {
		req := item.(Request)
		ttyd := worker.Spec.Containers[0]
		return req.Namespace == worker.Namespace && req.Image == ttyd.Image
	})
	if item != nil {
		req := item.(Request)
		return &req
	}

	return nil
}

func (w *WorkerPool) Back(worker *corev1.Pod) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		newer, err := w.podLister.Pods(worker.Namespace).Get(worker.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		// if the worker is not ready, we delete it directly.
		if !util.IsPodReady(newer) {
			return w.deleteWorker(newer)
		}

		req := w.matchRequestFor(newer)

		// if worker is not match to request and out of limit, we need to delete the worker.
		if req == nil && w.workerQueue.Len() >= w.maxWorkerLimit {
			return w.deleteWorker(newer)
		}

		controllerutil.AddFinalizer(newer, ControllerFinalizer)
		if newer.Labels == nil {
			newer.Labels = map[string]string{}
		}

		newer.Labels[constants.WorkerOwnerLabelKey] = ""
		if err := w.Update(context.TODO(), newer); err != nil {
			return err
		}

		objKey, _ := WorkerKeyFunc(worker)
		w.workerQueue.Add(objKey)

		if req != nil {
			w.matchRequestSignal <- struct{}{}
		}

		return nil
	})
}

func (w *WorkerPool) createWorker(req *Request) error {
	podBytes, err := util.ParseTemplate(manifests.PodTmplV1, struct {
		Name, Namespace, Image string
	}{
		Name:      fmt.Sprintf("cloudshell-worker-%s", rand.String(5)),
		Namespace: req.Namespace,
		Image:     req.Image,
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
	controllerutil.AddFinalizer(pod, ControllerFinalizer)

	pod.SetLabels(map[string]string{
		constants.WorkerOwnerLabelKey: "", constants.WorkerRequestLabelKey: req.Cloudshell,
	})

	return w.Create(context.TODO(), pod)
}

func (w *WorkerPool) deleteWorker(worker *corev1.Pod) error {
	return w.Delete(context.TODO(), worker)
}

func isIdleWorker(worker *corev1.Pod) bool {
	if worker.Labels != nil {
		owner, exist := worker.Labels[constants.WorkerOwnerLabelKey]
		if !exist {
			return true
		}

		return len(owner) == 0
	}

	return false
}

// SplitWorkerKey returns the namespace, name, image that
// WorkerKeyFunc encoded into key.
//
// packing/unpacking won't be necessary.
func SplitWorkerKey(key string) (namespace, name, image string, err error) {
	parts := strings.Split(key, "//")
	switch len(parts) {
	case 2:
		// name only, no namespace
		return "", parts[1], parts[0], nil
	case 3:
		// namespace and name
		return parts[0], parts[2], parts[1], nil
	}

	return "", "", "", fmt.Errorf("unexpected key format: %q", key)
}

func WorkerKeyFunc(obj interface{}) (string, error) {
	worker, ok := obj.(*corev1.Pod)
	if !ok {
		return "", fmt.Errorf("no support the type struct")
	}

	objName, err := cache.ObjectToName(worker)
	if err != nil {
		return "", err
	}

	// TODO: the first container is ttyd?
	ttyd := worker.Spec.Containers[0]

	if len(objName.Namespace) > 0 {
		return fmt.Sprintf("%s//%s//%s", objName.Namespace, ttyd.Image, objName.Name), nil
	}

	return fmt.Sprintf("%s//%s", ttyd.Image, objName.Name), nil
}
