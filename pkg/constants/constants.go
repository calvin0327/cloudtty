package constants

const (
	DefaultPathPrefix           = "/apis/v1alpha1/cloudshell"
	DefaultIngressName          = "cloudshell-ingress"
	DefaultVirtualServiceName   = "cloudshell-virtualService"
	DefaultServicePort          = 7681
	DefauletWebttyContainerName = "web-tty"

	CloudshellOwnerLabelKey    = "cloudshell.cloudtty.io/owner-name"
	CloudshellWorkerLabelKey   = "cloudshell.cloudtty.io/worker-name"
	CloudshellPodLabelStateKey = "cloudshell.cloudtty.io/worker-state"

	PodTemplatePath = "/etc/cloudtty/pod-temp.yaml"
)
