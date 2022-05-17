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
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudtty/cloudtty/pkg/apis/cloudshell/v1alpha1"
	cloudshellv1alpha1 "github.com/cloudtty/cloudtty/pkg/apis/cloudshell/v1alpha1"
	"github.com/cloudtty/cloudtty/pkg/manifests"
	util "github.com/cloudtty/cloudtty/pkg/utils"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	DefaultPathPrefix  = "/apis/v1alpha1/cloudshell"
	DefaultIngressName = "cloudshell-ingress"
	DefaultServicePort = 7681
	// CloudshellControllerFinalizer is added to cloudshell to ensure Work as well as the
	// execution space (namespace) is deleted before itself is deleted.
	CloudshellControllerFinalizer = "cloudtty.io/cloudshell-controller"
)

// CloudShellReconciler reconciles a CloudShell object
type CloudShellReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=cloudshell.cloudtty.io,resources=cloudshells,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudshell.cloudtty.io,resources=cloudshells/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudshell.cloudtty.io,resources=cloudshells/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;
//+kubebuilder:rbac:groups="networking.k8s.io/v1",resources=ingresses,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CloudShell object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (c *CloudShellReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	cloudshell := &cloudshellv1alpha1.CloudShell{}
	if err := c.Get(ctx, req.NamespacedName, cloudshell); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// delete ingress or virtualService rule.
	if !cloudshell.DeletionTimestamp.IsZero() {
		if err := c.removeFinalizer(cloudshell); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
		return ctrl.Result{}, c.removeCloudshell(ctx, cloudshell)
	}

	if cloudshell.Status.Phase == cloudshellv1alpha1.PhaseCompleted {
		log.Info("find a completed instance", "instance", cloudshell)
		return ctrl.Result{}, nil
	}

	if err := c.ensureFinalizer(cloudshell); err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	// Get downstream job and status
	job, err := c.GetJobForCloudshell(ctx, req.Namespace, cloudshell)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		if err := c.CreateCloudShellJob(ctx, cloudshell); err != nil {
			return ctrl.Result{}, err
		}

		// update the cloudshell status.
		if err := c.UpdateCloudshellStatus(ctx, cloudshell, cloudshellv1alpha1.PhaseCreatedJob); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("after created job, wait to 1s")
		return ctrl.Result{RequeueAfter: time.Duration(1) * time.Second}, nil
	}

	//TODO: if job had completed or failed, we think the job is done.
	if isFinished, finishedType := isJobFinished(job); isFinished {
		// update the cloudshell status.
		if err := c.UpdateCloudshellStatus(ctx, cloudshell, string(finishedType)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// if job is running, create the different route according to exposure mode.
	if job.Status.Active == 1 {
		if cloudshell.Status.Phase == cloudshellv1alpha1.PhaseCreatedJob {
			if err := c.CreateRouteRule(ctx, cloudshell); err != nil {
				return ctrl.Result{}, err
			}

			// update cloudshell phase to "PhaseCreatedRoute".
			if err := c.UpdateCloudshellStatus(ctx, cloudshell, cloudshellv1alpha1.PhaseCreatedRoute); err != nil {
				return ctrl.Result{}, err
			}
		}

		// waiting job status to Completed, requeue every 10 s
		var interval int32 = 10
		// find a modest interval
		if cloudshell.Spec.Ttl > interval*10 {
			interval = cloudshell.Spec.Ttl / 10
		}

		syncInterval := time.Duration(interval) * time.Second
		log.Info("waiting job status to completed")
		return ctrl.Result{RequeueAfter: syncInterval}, nil
	} else {
		log.Info("Waiting job to active")
		return ctrl.Result{RequeueAfter: time.Duration(5) * time.Second}, nil
	}
}

func (c *CloudShellReconciler) removeFinalizer(cluster *cloudshellv1alpha1.CloudShell) error {
	if !ctrlutil.ContainsFinalizer(cluster, CloudshellControllerFinalizer) {
		return nil
	}

	ctrlutil.RemoveFinalizer(cluster, CloudshellControllerFinalizer)
	err := c.Client.Update(context.TODO(), cluster)
	if err != nil {
		return err
	}

	return nil
}

func (c *CloudShellReconciler) ensureFinalizer(cluster *cloudshellv1alpha1.CloudShell) error {
	if ctrlutil.ContainsFinalizer(cluster, CloudshellControllerFinalizer) {
		return nil
	}

	ctrlutil.AddFinalizer(cluster, CloudshellControllerFinalizer)
	err := c.Client.Update(context.TODO(), cluster)
	if err != nil {
		return err
	}

	return nil
}

// CreateCloudShellJob clould create a job for cloudshell, the job will running a webtty server in the pod.
// the job template set default images registry "ghcr.io" and default command, no modification is supported currently,
// the configmap must be existed in the cluster.
func (r *CloudShellReconciler) CreateCloudShellJob(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	log := log.FromContext(ctx)

	jobBytes, err := util.ParseTemplate(manifests.JobTmplV1, struct {
		Namespace, Name, Ownership, Command, Configmap string
		Once                                           bool
		Ttl                                            int32
	}{
		Namespace: cloudshell.Namespace,
		Name:      fmt.Sprintf("cloudshell-%s", cloudshell.Name),
		Ownership: cloudshell.Name,
		Once:      cloudshell.Spec.Once,
		Command:   cloudshell.Spec.CommandAction,
		Configmap: cloudshell.Spec.ConfigmapName,
		Ttl:       cloudshell.Spec.Ttl,
	})
	if err != nil {
		return errors.Wrap(err, "failed create cloudshell job")
	}

	//https://dx13.co.uk/articles/2021/01/15/kubernetes-types-using-go/
	decoder := scheme.Codecs.UniversalDeserializer()
	obj, groupVersionKind, err := decoder.Decode(jobBytes, nil, nil)
	if err != nil {
		log.Error(err, "Error while decoding YAML object. Err was: ")
		return err
	}
	job := obj.(*batchv1.Job)

	// set reference for job, once the cloudshell is deleted, the job is also deleted.
	if err := ctrlutil.SetControllerReference(cloudshell, job, r.Scheme); err != nil {
		log.Error(err, "Failed to set owner reference")
		return err
	}

	log.Info("Print gvk", "gvk", groupVersionKind)
	log.Info("Print job", "job", job)

	return r.Create(ctx, job)
}

// CreateRouteRule create a service resource in the same namespace of cloudshell no matter what expose model.
// if the expose model is ingress or virtualService, it will create additional resources, e.g: ingress or virtualService.
// and the accressUrl will be update.
func (c *CloudShellReconciler) CreateRouteRule(ctx context.Context, cloudshell *v1alpha1.CloudShell) error {
	log := log.FromContext(ctx)

	service, err := c.GetServiceForCloudshell(ctx, cloudshell.Namespace, cloudshell)
	if err != nil {
		log.Error(err, "unable to get service of cloudshell %s", cloudshell.Name)
		return err
	}

	if service == nil {
		if service, err = c.CreateCloudShellService(ctx, cloudshell); err != nil && !apierrors.IsAlreadyExists(err) {
			log.Error(err, "unable to create service")
			return err
		}
	}

	var accessURL string
	switch cloudshell.Spec.ExposeMode {
	case "", cloudshellv1alpha1.ExposureServiceClusterIP:
		accessURL = fmt.Sprintf("%s:%d", service.Spec.ClusterIP, DefaultServicePort)
	case cloudshellv1alpha1.ExposureServiceNodePort:
		host, err := c.GetMasterNodeIP(ctx)
		if err != nil {
			log.Error(err, "unable to get master node IP addr")
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

		// create ingress for cloudshell.
		if err := c.CreateIngressForCloudshell(ctx, service, cloudshell); err != nil {
			log.Error(err, "unable create ingress for cloudshell")
			return err
		}
		accessURL = fmt.Sprintf("%s/%s", SetPathPrefix(cloudshell), cloudshell.Name)
	}

	cloudshell.Status.AccessURL = accessURL
	if err := c.UpdateCloudshellStatus(ctx, cloudshell, cloudshellv1alpha1.PhaseReady); err != nil {
		log.Error(err, "unable to update cloudshell %s status", cloudshell.Name)
		return err
	}
	return nil
}

// GetJobForCloudshell to find job of cloudshell according to labels "ownership".
func (r *CloudShellReconciler) GetJobForCloudshell(ctx context.Context, namespace string, owner *cloudshellv1alpha1.CloudShell) (*batchv1.Job, error) {
	log := log.FromContext(ctx)

	var childJobs batchv1.JobList
	//find job in the ns, match label "ownership: parent-CR-name "
	if err := r.List(ctx, &childJobs, client.InNamespace(namespace), client.MatchingLabels{"ownership": owner.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return nil, err
	}
	//log.Info("DEBUG, Found Jobs :", "jobs", len(childJobs.Items))
	if len(childJobs.Items) > 1 {
		err := errors.New("found duplicated child jobs")
		log.Error(err, "more than 1 jobs found")
		return nil, err
	}
	if len(childJobs.Items) == 0 {
		log.Info("job creation is still pending")
		return nil, nil
	}
	theJob := &childJobs.Items[0]
	//log.Info("find created job ", "job.status", theJob.Status)

	return theJob, nil
}

// GetMasterNodeIP could find the one master node IP.
func (c *CloudShellReconciler) GetMasterNodeIP(ctx context.Context) (string, error) {
	// Fetch Node IP address
	nodelist := corev1.NodeList{}
	masterLabel := client.MatchingLabels{"node-role.kubernetes.io/master": ""}
	if err := c.List(ctx, &nodelist, masterLabel); err != nil || len(nodelist.Items) == 0 {
		return "", err
	}
	for _, addr := range nodelist.Items[0].Status.Addresses {
		// Using External IP as first priority
		if addr.Type == corev1.NodeExternalIP || addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}
	return "", nil
}

// GetServiceForCloudshell to find service of cloudshell according to labels "ownership".
func (r *CloudShellReconciler) GetServiceForCloudshell(ctx context.Context, namespace string, owner *cloudshellv1alpha1.CloudShell) (*corev1.Service, error) {
	log := log.FromContext(ctx)

	var childSvcs corev1.ServiceList
	if err := r.List(ctx, &childSvcs, client.InNamespace(namespace), client.MatchingLabels{"ownership": owner.Name}); err != nil {
		log.Error(err, "unable to list child svc")
		return nil, err
	}
	//log.Info("DEBUG, Found Svc :", "svcs", len(childSvcs.Items))
	if len(childSvcs.Items) > 1 {
		err := errors.New("found duplicated child svcs")
		log.Error(err, "more than 1 svcs found")
		return nil, err
	}
	if len(childSvcs.Items) == 0 {
		log.Info("svc creation is still pending")
		return nil, nil
	}
	theSvc := &childSvcs.Items[0]
	//log.Info("find created svc", "nodeport", theSvc.Spec.Ports[0].NodePort)

	return theSvc, nil
}

// CreateCloudShellService Create service resource for cloudshell, the service type is either ClusterIP, NodePort,
// Ingress or virtualService. if the expose model is ingress or virtualService. it will create clusterIP type service.
func (c *CloudShellReconciler) CreateCloudShellService(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) (*corev1.Service, error) {
	log := log.FromContext(ctx)

	// if ExposeMode is nil, ingress or vituralService, default clusterIP.
	serviceType := cloudshell.Spec.ExposeMode
	if len(serviceType) == 0 || serviceType == cloudshellv1alpha1.ExposureIngress {
		serviceType = cloudshellv1alpha1.ExposureServiceClusterIP
	}

	serviceBytes, err := util.ParseTemplate(manifests.ServiceTmplV1, struct {
		GenerateName, Namespace, Ownership, JobName, Type string
	}{
		GenerateName: fmt.Sprintf("cloudshell-%s", cloudshell.Name),
		Namespace:    cloudshell.Namespace,
		Ownership:    cloudshell.Name,
		JobName:      fmt.Sprintf("cloudshell-%s", cloudshell.Name),
		Type:         string(serviceType),
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed create cloudshell job")
	}

	decoder := scheme.Codecs.UniversalDeserializer()
	obj, _, err := decoder.Decode(serviceBytes, nil, nil)
	if err != nil {
		log.Error(err, "Error while decoding YAML object. Err was: ")
		return nil, err
	}
	svc := obj.(*corev1.Service)

	// set reference for service, once the cloudshell is deleted, the service is alse deleted.
	if err := ctrlutil.SetControllerReference(cloudshell, svc, c.Scheme); err != nil {
		log.Error(err, "Failed to set owner reference")
		return nil, err
	}

	log.Info("Print svc", "svc", svc)
	return svc, c.Create(ctx, svc)
}

// CreateIngressForCloudshell create ingress for cloudshell, if there isn't an ingress controller server in the cluster,
// the ingress is still not working. before create ingress, there's must a service as the ingress backend service.
// all of services should be loaded in an ingress "cloudshell-ingress".
func (c *CloudShellReconciler) CreateIngressForCloudshell(ctx context.Context, service *corev1.Service, cloudshell *cloudshellv1alpha1.CloudShell) error {
	ingress := &networkingv1.Ingress{}
	err := c.Get(ctx, types.NamespacedName{Namespace: cloudshell.Namespace, Name: DefaultIngressName}, ingress)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// if there is not ingress in the cluster, create the base ingress.
	if ingress != nil && apierrors.IsNotFound(err) {
		ingressBytes, err := util.ParseTemplate(manifests.IngressTmplV1, struct {
			Namespace, IngressClassName, Path, ServiceName string
		}{
			Namespace:        cloudshell.Namespace,
			IngressClassName: cloudshell.Spec.IngressClassName,
			ServiceName:      service.Name,
			// set default path prefix.
			Path: fmt.Sprintf("%s/%s", SetPathPrefix(cloudshell), cloudshell.Name),
		})
		if err != nil {
			return errors.Wrap(err, "failed create cloudshell job")
		}

		decoder := scheme.Codecs.UniversalDeserializer()
		obj, _, err := decoder.Decode(ingressBytes, nil, nil)
		if err != nil {
			return err
		}
		ingress = obj.(*networkingv1.Ingress)

		return c.Create(ctx, ingress)
	}

	// there is an ingress in the cluster, add a rule to the ingress.
	IngressRule := ingress.Spec.Rules[0].IngressRuleValue.HTTP
	newPathRule := IngressRule.Paths[0].DeepCopy()

	newPathRule.Backend.Service.Name = service.Name
	newPathRule.Path = fmt.Sprintf("%s/%s", SetPathPrefix(cloudshell), cloudshell.Name)
	IngressRule.Paths = append(IngressRule.Paths, *newPathRule)
	return c.Update(ctx, ingress)
}

// SetupWithManager sets up the controller with the Manager.
func (c *CloudShellReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudshellv1alpha1.CloudShell{}).
		Complete(c)
}

// UpdateCloudshellStatus update the clodushell status.
func (c *CloudShellReconciler) UpdateCloudshellStatus(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell, phase string) error {
	cloudshell.Status.Phase = phase
	if err := c.Status().Update(ctx, cloudshell); err != nil {
		log.Log.Error(err, "unable to update instance status")
		return err
	}
	return nil
}

// removeCloudshell remove the cloudshell, at the same time, update addition resource.
// i.g: ingress or vitualService. if all of cloudshells was removed, it will delete the
// ingress or vitualService.
func (c *CloudShellReconciler) removeCloudshell(ctx context.Context, cloudshell *cloudshellv1alpha1.CloudShell) error {
	switch cloudshell.Spec.ExposeMode {
	case "", v1alpha1.ExposureServiceClusterIP, v1alpha1.ExposureServiceNodePort:
		// TODO: whether to delete ownReference resource.
	case v1alpha1.ExposureIngress:
		ingress := &networkingv1.Ingress{}
		err := c.Get(ctx, types.NamespacedName{Namespace: cloudshell.Namespace, Name: DefaultIngressName}, ingress)
		if err != nil {
			return err
		}

		ingressRule := ingress.Spec.Rules[0].IngressRuleValue.HTTP
		if len(ingressRule.Paths) == 1 {
			return c.Delete(ctx, ingress)
		}

		for i := 0; i < len(ingressRule.Paths); i++ {
			if ingressRule.Paths[i].Path != cloudshell.Status.AccessURL {
				continue
			}
			ingressRule.Paths = append(ingressRule.Paths[:i], ingressRule.Paths[i+1:]...)
			return c.Update(ctx, ingress)
		}
	}
	return nil
}

// SetPathPrefix return access url according to cloudshell.
func SetPathPrefix(cloudshell *v1alpha1.CloudShell) string {
	var pathPrefix string
	if len(cloudshell.Spec.PathPrefix) != 0 {
		pathPrefix = cloudshell.Spec.PathPrefix
	}

	if strings.HasSuffix(pathPrefix, "/") {
		pathPrefix = pathPrefix[:len(pathPrefix)-1] + DefaultPathPrefix
	} else {
		pathPrefix += DefaultPathPrefix
	}
	return pathPrefix
}

// isJobFinished check whether the job is completed.
// todo: Type JobFailed
func isJobFinished(job *batchv1.Job) (bool, batchv1.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}

	return false, ""
}
