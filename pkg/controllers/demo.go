package main

import (
	"bytes"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubectl/pkg/cmd/exec"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	config, err := ctrl.GetConfig()
	if err != nil {
		panic(err)
	}
	config.GroupVersion = &corev1.SchemeGroupVersion
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	config.APIPath = "/api"

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	options := &exec.ExecOptions{}
	options.StreamOptions = exec.StreamOptions{
		IOStreams: genericiooptions.IOStreams{
			In:     bytes.NewBuffer([]byte{}),
			Out:    bytes.NewBuffer([]byte{}),
			ErrOut: bytes.NewBuffer([]byte{}),
		},
		Stdin: false,
		// TTY:   false,
		// Quiet: true,

		Namespace: "kpanda-system",
		PodName:   "cloudshell-test-595b76bc79-xpfll",
	}

	// secret, err := client.CoreV1().Secrets("kpanda-system").Get(context.TODO(), "cloudshell-demo-65cd4b5b69-zbkb4-1695714269500-6ls96", v1.GetOptions{})
	// if err != nil {
	// 	panic(err)
	// }

	// configRaw := secret.Data["config"]

	// echoCommand := fmt.Sprintf("echo '%s' > /root/config", configRaw)

	cmdArr := []string{"/root/startup.sh", "kubectl logs -n default dao-800-dao-2048-7cd996fcdd-56kgk"}

	options.Command = cmdArr
	options.Executor = &exec.DefaultRemoteExecutor{}
	options.Config = config
	options.PodClient = client.CoreV1()

	if err := options.Validate(); err != nil {
		panic(err)
	}

	if err := options.Run(); err != nil {
		fmt.Println(err)
	}
}
