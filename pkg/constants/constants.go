package constants

const (
	DefaultPathPrefix           = "/apis/v1alpha1/cloudshell"
	DefaultIngressName          = "cloudshell-ingress"
	DefaultVirtualServiceName   = "cloudshell-virtualService"
	DefaultServicePort          = 7681
	DefauletWebttyContainerName = "web-tty"

	CloudshellPodLabelKey    = "cloudshell.cloudtty.io/pod-name"
	CloudshellWorkerLabelKey = "cloudshell.cloudtty.io/worker-name"
	CloudshellIdleWorkerKey  = "cloudshell.cloudtty.io/idle-worker"
	CloudshellOwnerLabelKey  = "cloudshell.cloudtty.io/owner-name"

	PodTemplatePath = "/etc/cloudtty/pod-temp.yaml"

	// KpandaNamespace for internal project use
	KpandaNamespace = "kpanda-system"
)
