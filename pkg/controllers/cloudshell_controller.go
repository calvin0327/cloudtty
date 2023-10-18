/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"github.com/cloudtty/cloudtty/pkg/generated/listers/cloudshell/v1alpha1"
	"github.com/cloudtty/cloudtty/pkg/helper"
	"github.com/cloudtty/cloudtty/pkg/utils/gclient"
	"github.com/pkg/errors"
	networkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/cmd/exec"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strings"
	"sync"
	"time"

	cloudshellv1alpha1 "github.com/cloudtty/cloudtty/pkg/apis/cloudshell/v1alpha1"
	"github.com/cloudtty/cloudtty/pkg/constants"
	cloudshellinformers "github.com/cloudtty/cloudtty/pkg/generated/informers/externalversions/cloudshell/v1alpha1"
	"github.com/cloudtty/cloudtty/pkg/manifests"
	util "github.com/cloudtty/cloudtty/pkg/utils"
	"github.com/cloudtty/cloudtty/pkg/workerpool"
)

const (
	// CloudshellControllerFinalizer is added to cloudshell to ensure Work as well as the
	// execution space (namespace) is deleted before itself is deleted.
	CloudshellControllerFinalizer = "cloudtty.io/cloudshell-controller"

	DefaultMaxConcurrentReconciles = 5

	// DefaultCloudShellBackOff is the default backoff period. Exported for tests.
	DefaultCloudShellBackOff = 2 * time.Second
	// MaxCloudShellBackOff is the max backoff period. Exported for tests.
	MaxCloudShellBackOff = 5 * time.Second
	KubeConfigPath       = "/root/.kube/config"
	TTYdScriptPath       = "/root/startup.sh"
)

// CloudShellController reconciles a CloudShell object
type CloudShellController struct {
	client.Client
	Scheme     *runtime.Scheme
	workerPool workpool.WorkerPool

	queue     workqueue.RateLimitingInterface
	informer  cache.SharedIndexInformer
	lister    v1alpha1.CloudShellLister
	podLister listerscorev1.PodLister
}

func NewController(client client.Client, csInformer cloudshellinformers.CloudShellInformer, podInformer informercorev1.PodInformer) *CloudShellController {
	controller := &CloudShellController{
		Client: client,
		Scheme: gclient.NewSchema(),
		queue: workqueue.NewRateLimitingQueue(
			workqueue.NewItemExponentialFailureRateLimiter(DefaultCloudShellBackOff, MaxCloudShellBackOff),
		),
		informer:  csInformer.Informer(),
		lister:    csInformer.Lister(),
		podLister: podInformer.Lister(),
	}

	//csInformer.Informer().AddEventHandler(
	//	cache.ResourceEventHandlerFuncs{
	//		AddFunc:    controller.AddEvent,
	//		UpdateFunc: controller.UpdateEventfunc,
	//		DeleteFunc: controller.DeleteEvent,
	//	},
	//)

	return controller
}

func (c *CloudShellController) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	// main reconcile logic
	err := c.syncHandler(key.(string))
	if err == nil {
		return true
	}

	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	c.queue.AddRateLimited(key)

	return true
}

func (c *CloudShellController) worker(stopCh <-chan struct{}) {
	for c.processNextItem() {
		select {
		case <-stopCh:
			return
		default:
		}
	}
}

func (c *CloudShellController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("Start CloudShell Controller")
	defer klog.InfoS("Shutting down CloudShell Controller")

	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		klog.Errorf("cloudshell manager: wait for informer factory failed")
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(stopCh)
		}()
	}
	<-stopCh

	c.queue.ShutDown()
	wg.Wait()
}

func (c *CloudShellController) syncHandler(key string) error {
	klog.V(4).Infof("Reconciling cloudshell %s", key)
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	cloudShell, err := c.lister.CloudShells(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		}
		return err
	}

	if !cloudShell.DeletionTimestamp.IsZero() {
		return c.removeCloudshell(context.TODO(), cloudShell)
	}

	// as we not have webhook to init some necessary feild to cloudshell.
	// fill this default values to cloudshell after calling "syncCloudShell".
	if err := c.fillForCloudshell(context.TODO(), cloudShell); err != nil {
		return err
	}

	return c.syncCloudShell(context.TODO(), cloudShell)
}

func (c *CloudShellController) syncCloudShell(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	var err error
	pod := &corev1.Pod{}
	podName, ok := hasBindPod(cloudshell)
	if ok {
		if err = c.Get(context.TODO(), client.ObjectKey{Namespace: cloudshell.Namespace, Name: podName}, pod); err != nil {
			return err
		}
	}
	if IsCloudshellFinished(cloudshell) {
		if ok {
			// todo restore the pod, cleanup the kubeConfig
			_ = c.workerPool.Back(pod)
		}
		if cloudshell.Spec.Cleanup {
			if err = c.Delete(ctx, cloudshell); err != nil {
				klog.ErrorS(err, "Failed to delete cloudshell", "cloudshell", klog.KObj(cloudshell))
				return err
			}
		}
		klog.V(4).InfoS("cloudshell phase is to be finished", "cloudshell", klog.KObj(cloudshell))
		return nil
	}

	// 1. GetPodForCloudShell, borrow pod from workerPool if not exist
	// 2. StartPodForCloudShell (1) copy kubeConfig (2) start ttyd
	if !ok {
		pod, err = c.workerPool.Borrow()
		if err != nil {
			klog.ErrorS(err, "Failed to get pod", "cloudshell", klog.KObj(cloudshell))
			return err
		}
		if err = c.StartPodForCloudShell(ctx, cloudshell); err != nil {
			klog.ErrorS(err, "Failed to start pod for CloudShell", "cloudshell", klog.KObj(cloudshell))
			return err
		}

		cloudshell.SetLabels(map[string]string{constants.CloudshellPodLabelKey: pod.Name})
		if err = c.UpdateCloudshellStatus(ctx, cloudshell, cloudshellv1alpha1.PhasePodReady); err != nil {
			return err
		}
	}

	if IsCloudShellPodReady(cloudshell) {
		if len(cloudshell.Status.AccessURL) == 0 {
			if err = c.CreateRouteRule(ctx, cloudshell); err != nil {
				return err
			}
		}

		if err = c.UpdateCloudshellStatus(ctx, cloudshell, cloudshellv1alpha1.PhaseReady); err != nil {
			return err
		}
	}
	return nil
}

// StartPodForCloudShell copy kubeConfig and start ttyd
func (c *CloudShellController) StartPodForCloudShell(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	// if secretRef is empty, use the Incluster rest config to generate kubeconfig and restore a secret.
	// the kubeconfig only work on current cluster.
	var kubeConfigByte []byte
	if cloudshell.Spec.SecretRef == nil {
		var err error
		kubeConfigByte, err = GenerateKubeconfigInCluster()
		if err != nil {
			return err
		}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("cloudshell-%s-", cloudshell.Name),
				Namespace:    cloudshell.Namespace,
			},
			Data: map[string][]byte{"config": kubeConfigByte},
		}

		if err := ctrlutil.SetControllerReference(cloudshell, secret, c.Scheme); err != nil {
			klog.ErrorS(err, "Failed to set owner reference for configmap", "cloudshell", klog.KObj(cloudshell))
			return err
		}
		if err := c.Client.Create(ctx, secret); err != nil {
			return err
		}
		cloudshell.Spec.SecretRef = &cloudshellv1alpha1.LocalSecretReference{
			Name: secret.Name,
		}
	} else {
		secret := &corev1.Secret{}
		if err := c.Client.Get(ctx, client.ObjectKey{Namespace: cloudshell.Namespace, Name: cloudshell.Spec.SecretRef.Name}, secret); err != nil {
			return err
		}
		kubeConfigByte = secret.Data["config"]
	}

	if err := c.StartTTYd(ctx, cloudshell, kubeConfigByte); err != nil {
		return err
	}
	return nil
}

func (c *CloudShellController) StartTTYd(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell, kubeConfigByte []byte) error {
	// copy kubeConfig
	echoCommand := fmt.Sprintf("echo '%s' > %s", kubeConfigByte, KubeConfigPath)
	if err := runCommand(cloudshell, []string{"bash", "-c", echoCommand}, kubeConfigByte); err != nil {
		return err
	}

	// start ttyd, ttyd args passed as shell parameter
	// case: ttydCommand := []string{"/root/startup.sh", "100", "true", "false", "kubectl get po -n default"}
	ttydCommand := []string{TTYdScriptPath, string(cloudshell.Spec.Ttl), fmt.Sprint(cloudshell.Spec.Once), fmt.Sprint(cloudshell.Spec.UrlArg), cloudshell.Spec.CommandAction}
	if err := runCommand(cloudshell, ttydCommand, kubeConfigByte); err != nil {
		return err
	}

	return nil
}

func (c *CloudShellController) fillForCloudshell(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	if len(cloudshell.Spec.ExposeMode) == 0 {
		cloudshell.Spec.ExposeMode = cloudshellv1alpha1.ExposureServiceNodePort
	}

	// we consider that the ttl is too short is not meaningful.
	if cloudshell.Spec.Ttl < 300 {
		cloudshell.Spec.Ttl = 300
	}
	if len(cloudshell.Spec.CommandAction) == 0 {
		cloudshell.Spec.CommandAction = "bash"
	}
	return c.ensureFinalizer(cloudshell)
}

func (c *CloudShellController) removeFinalizer(cloudshell *cloudshellv1alpha1.CloudShell) error {
	if !ctrlutil.ContainsFinalizer(cloudshell, CloudshellControllerFinalizer) {
		return nil
	}

	ctrlutil.RemoveFinalizer(cloudshell, CloudshellControllerFinalizer)
	return c.Client.Update(context.TODO(), cloudshell)
}

func (c *CloudShellController) ensureFinalizer(cloudshell *cloudshellv1alpha1.CloudShell) error {
	if ctrlutil.ContainsFinalizer(cloudshell, CloudshellControllerFinalizer) {
		return nil
	}

	ctrlutil.AddFinalizer(cloudshell, CloudshellControllerFinalizer)
	return c.Client.Update(context.TODO(), cloudshell)
}

// CreateRouteRule create a service resource in the same namespace of cloudshell no matter what expose model.
// if the expose model is ingress or virtualService, it will create additional resources, e.g: ingress or virtualService.
// and the accressUrl will be update.
func (c *CloudShellController) CreateRouteRule(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	service, err := c.GetServiceForCloudshell(ctx, cloudshell.Namespace, cloudshell)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if service, err = c.CreateCloudShellService(ctx, cloudshell); err != nil {
			return err
		}
	}

	var accessURL string
	switch cloudshell.Spec.ExposeMode {
	case cloudshellv1alpha1.ExposureServiceClusterIP:
		accessURL = fmt.Sprintf("%s:%d", service.Spec.ClusterIP, constants.DefaultServicePort)
	case "", cloudshellv1alpha1.ExposureServiceNodePort:
		// Default(No explicit `ExposeMode` specified in CR) mode is Nodeport
		host, err := c.GetMasterNodeIP(ctx)
		if err != nil {
			klog.InfoS("Unable to get master node IP addr", "cloudshell", klog.KObj(cloudshell), "err", err)
		}

		var nodePort int32
		// TODO: nodePort may be blank due to delay filled by k8s, should `ctrl.Result{RequeueAfter: 5}, nil`
		if service.Spec.Type == corev1.ServiceTypeNodePort {
			for _, port := range service.Spec.Ports {
				if port.NodePort != 0 {
					nodePort = port.NodePort
					break
				}
			}
		}
		accessURL = fmt.Sprintf("%s:%d", host, nodePort)
	case cloudshellv1alpha1.ExposureIngress:
		if err := c.CreateIngressForCloudshell(ctx, service, cloudshell); err != nil {
			klog.ErrorS(err, "failed create ingress for cloudshell", "cloudshell", klog.KObj(cloudshell))
			return err
		}
		accessURL = SetRouteRulePath(cloudshell)
	case cloudshellv1alpha1.ExposureVirtualService:
		if err := c.CreateVirtualServiceForCloudshell(ctx, service, cloudshell); err != nil {
			klog.ErrorS(err, "failed create virtualservice for cloudshell", "cloudshell", klog.KObj(cloudshell))
			return err
		}
		accessURL = SetRouteRulePath(cloudshell)
	}

	cloudshell.Status.AccessURL = accessURL
	return c.UpdateCloudshellStatus(ctx, cloudshell, cloudshellv1alpha1.PhaseCreatedRoute)
}

// GetPodForCloudshell to find pod of cloudshell according to labels ""cloudshell.cloudtty.io/owner-name"".
func (c *CloudShellController) GetPodForCloudshell(ctx context.Context, namespace string, cloudshell *cloudshellv1alpha1.CloudShell) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	podName, ok := cloudshell.Labels[constants.CloudshellPodLabelKey]
	if ok {
		if err := c.Get(context.TODO(), client.ObjectKey{Namespace: cloudshell.Namespace, Name: podName}, pod); err != nil {
			return pod, err
		}
	}

	return pod, nil
}

// GetMasterNodeIP could find the one master node IP.
func (c *CloudShellController) GetMasterNodeIP(ctx context.Context) (string, error) {
	// the label "node-role.kubernetes.io/master" be removed in k8s 1.24, and replace with
	// "node-role.kubernetes.io/cotrol-plane".
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes, client.MatchingLabels{"node-role.kubernetes.io/master": ""}); err != nil {
		return "", err
	}
	if len(nodes.Items) == 0 {
		if err := c.List(ctx, nodes, client.MatchingLabels{"node-role.kubernetes.io/control-plane": ""}); err != nil || len(nodes.Items) == 0 {
			return "", err
		}
	}

	for _, addr := range nodes.Items[0].Status.Addresses {
		// Using External IP as first priority
		if addr.Type == corev1.NodeExternalIP || addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}
	return "", nil
}

// GetServiceForCloudshell to find service of cloudshell according to labels "cloudshell.cloudtty.io/owner-name".
func (c *CloudShellController) GetServiceForCloudshell(ctx context.Context, namespace string, cloudshell *cloudshellv1alpha1.CloudShell) (*corev1.Service, error) {
	var services corev1.ServiceList
	if err := c.List(ctx, &services, client.InNamespace(namespace), client.MatchingLabels{constants.CloudshellOwnerLabelKey: cloudshell.Name}); err != nil {
		return nil, err
	}
	if len(services.Items) > 1 {
		klog.InfoS("found multiple cloudshell service", "cloudshell", klog.KObj(cloudshell))
		return nil, errors.New("found multiple cloudshell services")
	}
	if len(services.Items) == 0 {
		return nil, apierrors.NewNotFound(corev1.Resource("services"), fmt.Sprintf("cloudshell-%s", cloudshell.Name))
	}
	return &services.Items[0], nil
}

// CreateCloudShellService Create service resource for cloudshell, the service type is either ClusterIP, NodePort,
// Ingress or virtualService. if the expose model is ingress or virtualService. it will create clusterIP type service.
func (c *CloudShellController) CreateCloudShellService(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) (*corev1.Service, error) {
	serviceType := cloudshell.Spec.ExposeMode
	if len(serviceType) == 0 {
		serviceType = cloudshellv1alpha1.ExposureServiceNodePort
	}
	// if ExposeMode is ingress or vituralService, the svc type should be ClusterIP.
	if serviceType == cloudshellv1alpha1.ExposureIngress ||
		serviceType == cloudshellv1alpha1.ExposureVirtualService {
		serviceType = cloudshellv1alpha1.ExposureServiceClusterIP
	}

	serviceBytes, err := util.ParseTemplate(manifests.ServiceTmplV1, helper.NewServiceTemplateValue(cloudshell, serviceType))
	if err != nil {
		return nil, errors.Wrap(err, "Failed to parse cloudshell service manifest")
	}

	decoder := scheme.Codecs.UniversalDeserializer()
	obj, _, err := decoder.Decode(serviceBytes, nil, nil)
	if err != nil {
		klog.ErrorS(err, "failed to decode service manifest", "cloudshell", klog.KObj(cloudshell))
		return nil, err
	}
	svc := obj.(*corev1.Service)
	svc.SetLabels(map[string]string{constants.CloudshellOwnerLabelKey: cloudshell.Name})

	// set reference for service, once the cloudshell is deleted, the service is alse deleted.
	if err := ctrlutil.SetControllerReference(cloudshell, svc, c.Scheme); err != nil {
		return nil, err
	}

	if err := c.Create(ctx, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

// CreateIngressForCloudshell create ingress for cloudshell, if there isn't an ingress controller server
// in the cluster, the ingress is still not working. before create ingress, there's must a service
// as the ingress backend service. all of services should be loaded in an ingress "cloudshell-ingress".
func (c *CloudShellController) CreateIngressForCloudshell(ctx context.Context, service *corev1.Service, cloudshell *cloudshellv1alpha1.CloudShell) error {
	ingress := &networkingv1.Ingress{}
	objectKey := IngressNamespacedName(cloudshell)
	err := c.Get(ctx, objectKey, ingress)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// if there is no ingress in the cluster, create the base ingress.
	if ingress != nil && apierrors.IsNotFound(err) {
		var ingressClassName string
		if cloudshell.Spec.IngressConfig != nil && len(cloudshell.Spec.IngressConfig.IngressClassName) > 0 {
			ingressClassName = cloudshell.Spec.IngressConfig.IngressClassName
		}

		// set default path prefix.
		rulePath := SetRouteRulePath(cloudshell)
		ingressTemplateValue := helper.NewIngressTemplateValue(objectKey, ingressClassName, service.Name, rulePath)
		ingressBytes, err := util.ParseTemplate(manifests.IngressTmplV1, ingressTemplateValue)

		if err != nil {
			return errors.Wrap(err, "failed to parse cloudshell ingress manifest")
		}

		decoder := scheme.Codecs.UniversalDeserializer()
		obj, _, err := decoder.Decode(ingressBytes, nil, nil)
		if err != nil {
			klog.ErrorS(err, "failed to decode ingress manifest", "cloudshell", klog.KObj(cloudshell))
			return err
		}
		ingress = obj.(*networkingv1.Ingress)
		ingress.SetLabels(map[string]string{constants.CloudshellOwnerLabelKey: cloudshell.Name})

		return c.Create(ctx, ingress)
	}

	// there is an ingress in the cluster, add a rule to the ingress.
	IngressRule := ingress.Spec.Rules[0].IngressRuleValue.HTTP
	pathType := networkingv1.PathTypePrefix
	IngressRule.Paths = append(IngressRule.Paths, networkingv1.HTTPIngressPath{
		PathType: &pathType,
		Path:     SetRouteRulePath(cloudshell),
		Backend: networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{
				Name: service.Name,
				Port: networkingv1.ServiceBackendPort{
					Number: 7681,
				},
			},
		},
	})
	// TODO: All paths will be rewritten here
	ans := ingress.GetAnnotations()
	if ans == nil {
		ans = make(map[string]string)
	}
	ans["nginx.ingress.kubernetes.io/rewrite-target"] = "/"
	ingress.SetAnnotations(ans)
	return c.Update(ctx, ingress)
}

// CreateVirtualServiceForCloudshell create a virtualService resource in the cluster. if there
// is no istio server be deployed in the cluster. will not create the resource.
func (c *CloudShellController) CreateVirtualServiceForCloudshell(ctx context.Context, service *corev1.Service, cloudshell *cloudshellv1alpha1.CloudShell) error {
	config := cloudshell.Spec.VirtualServiceConfig
	if config == nil {
		return errors.New("unable create virtualservice, missing configuration options")
	}

	// TODO: check the crd in the cluster.

	objectKey := VsNamespacedName(cloudshell)
	virtualService := &istionetworkingv1beta1.VirtualService{}

	err := c.Get(ctx, objectKey, virtualService)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// if there is not virtualService in the cluster, create the base virtualService.
	if virtualService != nil && apierrors.IsNotFound(err) {
		rulePath := SetRouteRulePath(cloudshell)
		virtualServiceTemplateValue := helper.NewVirtualServiceTemplateValue(objectKey, config, service.Name, rulePath)
		vitualServiceBytes, err := util.ParseTemplate(manifests.VirtualServiceV1Beta1, virtualServiceTemplateValue)
		if err != nil {
			return errors.Wrapf(err, "failed to parse cloudshell [%s] virtualservice", cloudshell.Name)
		}

		scheme := runtime.NewScheme()
		istionetworkingv1beta1.AddToScheme(scheme)
		decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
		obj, _, err := decoder.Decode(vitualServiceBytes, nil, nil)
		if err != nil {
			klog.ErrorS(err, "failed to decode virtualservice manifest", "cloudshell", klog.KObj(cloudshell))
			return err
		}
		virtualService = obj.(*istionetworkingv1beta1.VirtualService)
		virtualService.SetLabels(map[string]string{constants.CloudshellOwnerLabelKey: cloudshell.Name})

		return c.Create(ctx, virtualService)
	}

	// there is a virtualService in the cluster, add a destination to it.
	newHttpRoute := virtualService.Spec.Http[0].DeepCopy()
	newHttpRoute.Match[0].Uri.MatchType = &networkingv1beta1.StringMatch_Prefix{Prefix: SetRouteRulePath(cloudshell)}
	newHttpRoute.Route[0].Destination.Host = fmt.Sprintf("%s.%s.svc.cluster.local", service.Name, objectKey.Namespace)

	virtualService.Spec.Http = append(virtualService.Spec.Http, newHttpRoute)
	return c.Update(ctx, virtualService)
}

// UpdateCloudshellStatus update the clodushell status.
func (c *CloudShellController) UpdateCloudshellStatus(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell, phase string) error {
	firstTry := true
	cloudshell.Status.Phase = phase
	status := cloudshell.Status
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		if !firstTry {
			if getErr := c.Get(ctx, types.NamespacedName{Name: cloudshell.Name, Namespace: cloudshell.Namespace}, cloudshell); err != nil {
				return getErr
			}
		}
		cloudshell.Status = status
		cc := cloudshell.DeepCopy()

		err = c.Status().Update(ctx, cc)
		firstTry = false
		return
	})
}

func (c *CloudShellController) removeCloudshell(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	if err := c.removeCloudshellRoute(ctx, cloudshell); err != nil {
		return err
	}
	if err := c.removeFinalizer(cloudshell); err != nil {
		return err
	}

	klog.V(4).InfoS("delete cloudshell", "cloudshell", klog.KObj(cloudshell))
	return nil
}

// removeCloudshell remove the cloudshell, at the same time, update addition resource.
// i.g: ingress or vitualService. if all of cloudshells was removed, it will delete the
// ingress or vitualService.
func (c *CloudShellController) removeCloudshellRoute(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	// delete route rule.
	switch cloudshell.Spec.ExposeMode {
	case "", cloudshellv1alpha1.ExposureServiceClusterIP, cloudshellv1alpha1.ExposureServiceNodePort:
		// TODO: whether to delete ownReference resource.
	case cloudshellv1alpha1.ExposureIngress:
		ingress := &networkingv1.Ingress{}
		if err := c.Get(ctx, IngressNamespacedName(cloudshell), ingress); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		ingressRule := ingress.Spec.Rules[0].IngressRuleValue.HTTP

		// remove rule from ingress. if the length of ingressRule is zero, delete it directly.
		for i := 0; i < len(ingressRule.Paths); i++ {
			if ingressRule.Paths[i].Path == cloudshell.Status.AccessURL {
				ingressRule.Paths = append(ingressRule.Paths[:i], ingressRule.Paths[i+1:]...)
				break
			}
		}

		if len(ingressRule.Paths) == 0 {
			return c.Delete(ctx, ingress)
		}
		return c.Update(ctx, ingress)
	case cloudshellv1alpha1.ExposureVirtualService:
		virtualService := &istionetworkingv1beta1.VirtualService{}
		if err := c.Get(ctx, VsNamespacedName(cloudshell), virtualService); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		// remove rule from virtualService. if the length of virtualService is zero, delete it directly.
		httpRoute := virtualService.Spec.Http
		for i := 0; i < len(httpRoute); i++ {
			match := httpRoute[i].Match
			if len(match) > 0 && match[0].Uri != nil {
				if prefix, ok := match[0].Uri.MatchType.(*networkingv1beta1.StringMatch_Prefix); ok &&
					prefix.Prefix == cloudshell.Status.AccessURL {

					httpRoute = append(httpRoute[:i], httpRoute[i+1:]...)
					break
				}
			}
		}

		if len(httpRoute) == 0 {
			return c.Delete(ctx, virtualService)
		}

		virtualService.Spec.Http = httpRoute
		return c.Update(ctx, virtualService)
	}
	return nil
}

// SetRouteRulePath return access url according to cloudshell.
func SetRouteRulePath(cloudshell *cloudshellv1alpha1.CloudShell) string {
	var pathPrefix string
	if len(cloudshell.Spec.PathPrefix) != 0 {
		pathPrefix = cloudshell.Spec.PathPrefix
	}

	if strings.HasSuffix(pathPrefix, "/") {
		pathPrefix = pathPrefix[:len(pathPrefix)-1] + constants.DefaultPathPrefix
	} else {
		pathPrefix += constants.DefaultPathPrefix
	}

	if len(cloudshell.Spec.PathSuffix) > 0 {
		return fmt.Sprintf("%s/%s/%s", pathPrefix, cloudshell.Name, cloudshell.Spec.PathSuffix)
	}
	return fmt.Sprintf("%s/%s", pathPrefix, cloudshell.Name)
}

// IngressNamespacedName return a namespacedName accroding to ingressConfig.
func IngressNamespacedName(cloudshell *cloudshellv1alpha1.CloudShell) types.NamespacedName {
	// set custom name and namespace to ingress.
	config := cloudshell.Spec.IngressConfig
	ingressName := constants.DefaultIngressName
	if config != nil && len(config.IngressName) > 0 {
		ingressName = config.IngressName
	}

	namespace := cloudshell.Namespace
	if config != nil && len(config.Namespace) > 0 {
		namespace = config.Namespace
	}

	return types.NamespacedName{Name: ingressName, Namespace: namespace}
}

// VsNamespacedName return a namespacedName accroding to virtaulServiceConfig.
func VsNamespacedName(cloudshell *cloudshellv1alpha1.CloudShell) types.NamespacedName {
	// set custom name and namespace to ingress.
	config := cloudshell.Spec.VirtualServiceConfig
	vsName := constants.DefaultVirtualServiceName
	if config != nil && len(config.VirtualServiceName) > 0 {
		vsName = config.VirtualServiceName
	}

	namespace := cloudshell.Namespace
	if config != nil && len(config.Namespace) > 0 {
		namespace = config.Namespace
	}

	return types.NamespacedName{Name: vsName, Namespace: namespace}
}

// isRunning check pod of job whether running, only one of the pods is running,
// and be considered the cloudtty server is working.

// TODO: The field `job.status.Ready` is alpha phase. we can depend on the field if it's to be beta.
func (c *CloudShellController) isRunning(ctx context.Context, job *batchv1.Job) (bool, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
	}

	return false, errors.Errorf("no pod of job %s is running", job.Name)
}

// GenerateKubeconfigInCluster load serviceaccount info under
// "/var/run/secrets/kubernetes.io/serviceaccount" and generate kubeconfig str.
func GenerateKubeconfigInCluster() ([]byte, error) {
	const (
		tokenFile  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	)

	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(host) == 0 || len(port) == 0 {
		return nil, errors.New("unable to load in-cluster configuration, KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT must be defined")
	}

	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}

	rootCA, err := os.ReadFile(rootCAFile)
	if err != nil {
		return nil, err
	}

	kubeConfigTemplateValue := helper.NewKubeConfigTemplateValue(host, port, string(token), rootCA)
	return util.ParseTemplate(manifests.KubeconfigTmplV1, kubeConfigTemplateValue)
}

func hasBindPod(cloudshell *cloudshellv1alpha1.CloudShell) (string, bool) {
	podName, ok := cloudshell.Labels[constants.CloudshellPodLabelKey]
	return podName, ok
}

func runCommand(cloudshell *cloudshellv1alpha1.CloudShell, command []string, kubeConfigByte []byte) error {
	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeConfigByte)
	if err != nil {
		klog.V(4).ErrorS(err, "unable to create client config from kubeConfig bytes", "cloudshell", cloudshell.Name)
		return err
	}

	clusterConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return err
	}
	clusterConfig.GroupVersion = &corev1.SchemeGroupVersion
	clusterConfig.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	clusterConfig.APIPath = "/api"

	clusterClient, err := kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		panic(err)
	}

	options := &exec.ExecOptions{
		Command:   command,
		Executor:  &exec.DefaultRemoteExecutor{},
		Config:    clusterConfig,
		PodClient: clusterClient.CoreV1(),
		StreamOptions: exec.StreamOptions{
			IOStreams: genericiooptions.IOStreams{
				In:     bytes.NewBuffer([]byte{}),
				Out:    bytes.NewBuffer([]byte{}),
				ErrOut: bytes.NewBuffer([]byte{}),
			},
			Stdin:     false,
			Namespace: cloudshell.Namespace,
			PodName:   cloudshell.Labels[constants.CloudshellPodLabelKey],
		},
	}

	if err := options.Validate(); err != nil {
		return err
	}

	if err := options.Run(); err != nil {
		klog.ErrorS(err, "failed to run command")
		return err
	}
	return nil
}
