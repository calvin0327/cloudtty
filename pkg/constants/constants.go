package constants

const (
	DefaultPathPrefix         = "/apis/v1alpha1/cloudshell"
	DefaultIngressName        = "cloudshell-ingress"
	DefaultVirtualServiceName = "cloudshell-virtualService"
	DefaultServicePort        = 7681
	DefaultTtydImage          = "release-ci.daocloud.io/cloudtty/cloudshell:v1.22"

	CloudshellPodLabelKey = "cloudshell.cloudtty.io/pod-name"

	WorkerOwnerLabelKey        = "worker.cloudtty.io/owner-name"
	WorkerRequestLabelKey      = "worker.cloudtty.io/request"
	WorkerBindingCountLabelKey = "worker.cloudtty.io/binding-count"

	PodTemplatePath = "/etc/cloudtty/pod-temp.yaml"
)
