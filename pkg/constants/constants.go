package constants

const (
	DefaultPathPrefix           = "/apis/v1alpha1/cloudshell"
	DefaultIngressName          = "cloudshell-ingress"
	DefaultVirtualServiceName   = "cloudshell-virtualService"
	DefaultServicePort          = 7681
	DefauletWebttyContainerName = "web-tty"

	CloudshellWorkerLabelKey = "cloudshell.cloudtty.io/worker-name"
	CloudshellIdleWorkerKey  = "cloudshell.cloudtty.io/idle-worker"

	PodTemplatePath = "/etc/cloudtty/pod-temp.yaml"
)
