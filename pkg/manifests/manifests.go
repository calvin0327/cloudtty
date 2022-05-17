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

package manifests

const (
	JobTmplV1 = `
apiVersion: batch/v1
kind: Job
metadata:
  namespace: {{ .Namespace }}
  name: {{ .Name }}
  labels:
    ownership: {{ .Ownership }}
spec:
  activeDeadlineSeconds: 3600
  ttlSecondsAfterFinished: 60
  parallelism: 1
  completions: 1
  template:
    spec:
      containers:
      - name:  web-tty
        image: ghcr.io/cloudtty/cloudshell:latest
        ports:
        - containerPort: 7681
          name: tty-ui
          protocol: TCP
        command:
          - bash
          - "-c"
          - |
            once=""
            index=""
            if [ "${ONCE}" == "true" ];then once=" --once "; fi;
            if [ -f /index.html ]; then index=" --index /index.html ";fi
            if [ -z "${TTL}" ] || [ "${TTL}" == "0" ];then
                ttyd ${index} ${once} sh -c "${COMMAND}"
            else
                timeout ${TTL} ttyd ${index} ${once} sh -c "${COMMAND}" || echo "exiting"
            fi
        env:
        - name: KUBECONFIG
          value: /usr/local/kubeconfig/config
        - name: ONCE
          value: "{{ .Once }}"
        - name: TTL
          value: "{{ .Ttl }}"
        - name: COMMAND
          value: {{ .Command }}
        volumeMounts:
          - mountPath: /usr/local/kubeconfig/
            name: kubeconfig
      restartPolicy: Never
      volumes:
      - configMap:
          defaultMode: 420
          name: {{ .Configmap }}
        name: kubeconfig
`

	ServiceTmplV1 = `
apiVersion: v1
kind: Service
metadata:
  labels:
    ownership: {{ .Ownership }}
  generateName: {{ .GenerateName }}
  namespace: {{ .Namespace }}
spec:
  ports:
  - name: ttyd
    port: 7681
    protocol: TCP
    targetPort: 7681
  selector:
    job-name: {{ .JobName }}
  type: {{ .Type }}
`

	IngressTmplV1 = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: cloudshell-ingress
  namespace: {{ .Namespace }}
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /
spec:
  ingressClassName: {{ .IngressClassName }}
  rules:
  - http:
      paths:
      - path: {{ .Path }}
        pathType: Exact
        backend:
          service:
            name: {{ .ServiceName }}
            port:
              number: 7681
`
)
