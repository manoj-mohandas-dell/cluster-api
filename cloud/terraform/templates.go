/*
Copyright 2017 The Kubernetes Authors.

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

package terraform

import (
	"bytes"
	"fmt"
	"text/template"

	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

type templateParams struct {
	Token        string
	Cluster      *clusterv1.Cluster
	Machine      *clusterv1.Machine
	DockerImages []string
	Preloaded    bool
}

// Returns the startup script for the nodes.
func getNodeStartupScript(params templateParams) (string, error) {
	var buf bytes.Buffer
	tName := "fullScript"
	if isPreloaded(params) {
		tName = "preloadedScript"
	}

	if err := nodeStartupScriptTemplate.ExecuteTemplate(&buf, tName, params); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func getMasterStartupScript(params templateParams) (string, error) {
	var buf bytes.Buffer
	tName := "fullScript"
	if isPreloaded(params) {
		tName = "preloadedScript"
	}

	if err := masterStartupScriptTemplate.ExecuteTemplate(&buf, tName, params); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func isPreloaded(params templateParams) bool {
	return params.Preloaded
}

// PreloadMasterScript returns a script that can be used to preload a master.
func PreloadMasterScript(version string, dockerImages []string) (string, error) {
	return preloadScript(masterStartupScriptTemplate, version, dockerImages)
}

// PreloadNodeScript returns a script that can be used to preload a master.
func PreloadNodeScript(version string, dockerImages []string) (string, error) {
	return preloadScript(nodeStartupScriptTemplate, version, dockerImages)
}

func preloadScript(t *template.Template, version string, dockerImages []string) (string, error) {
	var buf bytes.Buffer
	params := templateParams{
		Machine:      &clusterv1.Machine{},
		DockerImages: dockerImages,
	}
	params.Machine.Spec.Versions.Kubelet = version
	err := t.ExecuteTemplate(&buf, "generatePreloadedImage", params)
	return buf.String(), err
}

var (
	nodeStartupScriptTemplate   *template.Template
	masterStartupScriptTemplate *template.Template
)

func init() {
	endpoint := func(apiEndpoint *clusterv1.APIEndpoint) string {
		return fmt.Sprintf("%s:%d", apiEndpoint.Host, apiEndpoint.Port)
	}
	// Force a compliation error if getSubnet changes. This is the
	// signature the templates expect, so changes need to be
	// reflected in templates below.
	var _ func(clusterv1.NetworkRanges) string = getSubnet
	funcMap := map[string]interface{}{
		"endpoint":  endpoint,
		"getSubnet": getSubnet,
	}
	nodeStartupScriptTemplate = template.Must(template.New("nodeStartupScript").Funcs(funcMap).Parse(nodeStartupScript))
	nodeStartupScriptTemplate = template.Must(nodeStartupScriptTemplate.Parse(genericTemplates))
	masterStartupScriptTemplate = template.Must(template.New("masterStartupScript").Funcs(funcMap).Parse(masterStartupScript))
	masterStartupScriptTemplate = template.Must(masterStartupScriptTemplate.Parse(genericTemplates))
}

const genericTemplates = `
{{ define "fullScript" -}}
  {{ template "startScript" . }}
  {{ template "install" . }}
  {{ template "configure" . }}
  {{ template "endScript" . }}
{{- end }}

{{ define "preloadedScript" -}}
  {{ template "startScript" . }}
  {{ template "configure" . }}
  {{ template "endScript" . }}
{{- end }}

{{ define "generatePreloadedImage" -}}
  {{ template "startScript" . }}
  {{ template "install" . }}

systemctl enable docker || true
systemctl start docker || true

  {{ range .DockerImages }}
docker pull {{ . }}
  {{ end  }}

  {{ template "endScript" . }}
{{- end }}

{{ define "startScript" -}}
#!/bin/bash

set -e
set -x

(
{{- end }}

{{define "endScript" -}}

echo done.
) 2>&1 | tee /var/log/startup.log

{{- end }}
`

const nodeStartupScript = `
{{ define "install" -}}
# Disable swap otherwise kubelet won't run
swapoff -a
sed -i '/ swap / s/^/#/' /etc/fstab

apt-get update
apt-get install -y apt-transport-https prips
apt-key adv --keyserver hkp://keyserver.ubuntu.com --recv-keys F76221572C52609D

cat <<EOF > /etc/apt/sources.list.d/k8s.list
deb [arch=amd64] https://apt.dockerproject.org/repo ubuntu-xenial main
EOF

apt-get update
apt-get install -y docker.io

curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key add -

cat <<EOF > /etc/apt/sources.list.d/kubernetes.list
deb http://apt.kubernetes.io/ kubernetes-xenial main
EOF
apt-get update

{{- end }} {{/* end install */}}

{{ define "configure" -}}
KUBELET_VERSION={{ .Machine.Spec.Versions.Kubelet }}
TOKEN={{ .Token }}
MASTER={{ index .Cluster.Status.APIEndpoints 0 | endpoint }}
MACHINE={{ .Machine.ObjectMeta.Name }}
CLUSTER_DNS_DOMAIN={{ .Cluster.Spec.ClusterNetwork.ServiceDomain }}
SERVICE_CIDR={{ getSubnet .Cluster.Spec.ClusterNetwork.Services }}

# Our Debian packages have versions like "1.8.0-00" or "1.8.0-01". Do a prefix
# search based on our SemVer to find the right (newest) package version.
function getversion() {
	name=$1
	prefix=$2
	version=$(apt-cache madison $name | awk '{ print $3 }' | grep ^$prefix | head -n1)
	if [[ -z "$version" ]]; then
		echo Can\'t find package $name with prefix $prefix
		exit 1
	fi
	echo $version
}

KUBELET=$(getversion kubelet ${KUBELET_VERSION}-)
KUBEADM=$(getversion kubeadm ${KUBELET_VERSION}-)
KUBECTL=$(getversion kubectl ${KUBELET_VERSION}-)
# Explicit cni version is a temporary workaround till the right version can be automatically detected correctly
apt-get install -y kubelet=${KUBELET} kubeadm=${KUBEADM} kubectl=${KUBECTL}

systemctl enable docker || true
systemctl start docker || true

sysctl net.bridge.bridge-nf-call-iptables=1

# kubeadm uses 10th IP as DNS server
CLUSTER_DNS_SERVER=$(prips ${SERVICE_CIDR} | head -n 11 | tail -n 1)

cat > /etc/systemd/system/kubelet.service.d/20-cloud.conf << EOF
[Service]
Environment="KUBELET_DNS_ARGS=--cluster-dns=${CLUSTER_DNS_SERVER} --cluster-domain=${CLUSTER_DNS_DOMAIN}"
Environment="KUBELET_EXTRA_ARGS=--cloud-provider=vsphere"
EOF
systemctl daemon-reload
systemctl restart kubelet.service

kubeadm join --token "${TOKEN}" "${MASTER}" --skip-preflight-checks --discovery-token-unsafe-skip-ca-verification

for tries in $(seq 1 60); do
	kubectl --kubeconfig /etc/kubernetes/kubelet.conf annotate --overwrite node $(hostname) machine=${MACHINE} && break
	sleep 1
done
{{- end }} {{/* end configure */}}
`

const masterStartupScript = `
{{ define "install" -}}

# Disable swap otherwise kubelet won't run
swapoff -a
sed -i '/ swap / s/^/#/' /etc/fstab

KUBELET_VERSION={{ .Machine.Spec.Versions.Kubelet }}

curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key add -
touch /etc/apt/sources.list.d/kubernetes.list
sh -c 'echo "deb http://apt.kubernetes.io/ kubernetes-xenial main" > /etc/apt/sources.list.d/kubernetes.list'

apt-get update -y

apt-get install -y \
    socat \
    ebtables \
    docker.io \
    apt-transport-https \
    cloud-utils \
    prips

export VERSION=v${KUBELET_VERSION}
export ARCH=amd64
curl -sSL https://dl.k8s.io/release/${VERSION}/bin/linux/${ARCH}/kubeadm > /usr/bin/kubeadm.dl
chmod a+rx /usr/bin/kubeadm.dl
{{- end }} {{/* end install */}}


{{ define "configure" -}}
KUBELET_VERSION={{ .Machine.Spec.Versions.Kubelet }}
TOKEN={{ .Token }}
PORT=443
MACHINE={{ .Machine.ObjectMeta.Name }}
CONTROL_PLANE_VERSION={{ .Machine.Spec.Versions.ControlPlane }}
CLUSTER_DNS_DOMAIN={{ .Cluster.Spec.ClusterNetwork.ServiceDomain }}
POD_CIDR={{ getSubnet .Cluster.Spec.ClusterNetwork.Pods }}
SERVICE_CIDR={{ getSubnet .Cluster.Spec.ClusterNetwork.Services }}

# kubeadm uses 10th IP as DNS server
CLUSTER_DNS_SERVER=$(prips ${SERVICE_CIDR} | head -n 11 | tail -n 1)

# Our Debian packages have versions like "1.8.0-00" or "1.8.0-01". Do a prefix
# search based on our SemVer to find the right (newest) package version.
function getversion() {
	name=$1
	prefix=$2
	version=$(apt-cache madison $name | awk '{ print $3 }' | grep ^$prefix | head -n1)
	if [[ -z "$version" ]]; then
		echo Can\'t find package $name with prefix $prefix
		exit 1
	fi
	echo $version
}

KUBELET=$(getversion kubelet ${KUBELET_VERSION}-)
KUBEADM=$(getversion kubeadm ${KUBELET_VERSION}-)

# Explicit cni version is a temporary workaround till the right version can be automatically detected correctly
apt-get install -y \
    kubelet=${KUBELET} \
    kubeadm=${KUBEADM}

mv /usr/bin/kubeadm.dl /usr/bin/kubeadm
chmod a+rx /usr/bin/kubeadm

systemctl enable docker
systemctl start docker
cat > /etc/systemd/system/kubelet.service.d/20-cloud.conf << EOF
[Service]
Environment="KUBELET_DNS_ARGS=--cluster-dns=${CLUSTER_DNS_SERVER} --cluster-domain=${CLUSTER_DNS_DOMAIN}"
Environment="KUBELET_EXTRA_ARGS=--cloud-provider=vsphere --cloud-config=/etc/kubernetes/cloud-config/cloud-config.yaml"
EOF
systemctl daemon-reload
systemctl restart kubelet.service
` +
	"PRIVATEIP=`ip route get 8.8.8.8 | awk '{printf \"%s\", $NF; exit}'`" + `
echo $PRIVATEIP > /tmp/.ip
` +
	"PUBLICIP=`ip route get 8.8.8.8 | awk '{printf \"%s\", $NF; exit}'`" + `

# Set up kubeadm config file to pass parameters to kubeadm init.
cat > /etc/kubernetes/kubeadm_config.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1alpha1
kind: MasterConfiguration
api:
  advertiseAddress: ${PUBLICIP}
  bindPort: ${PORT}
networking:
  serviceSubnet: ${SERVICE_CIDR}
kubernetesVersion: v${CONTROL_PLANE_VERSION}
token: ${TOKEN}
apiServerCertSANs:
- ${PUBLICIP}
- ${PRIVATEIP}
apiServerExtraArgs:
  cloud-provider: vsphere
  cloud-config: /etc/kubernetes/cloud-config/cloud-config.yaml
apiServerExtraVolumes:
  - name: cloud-config
    hostPath: /etc/kubernetes/cloud-config
    mountPath: /etc/kubernetes/cloud-config
controllerManagerExtraArgs:
  cloud-provider: vsphere
  cloud-config: /etc/kubernetes/cloud-config/cloud-config.yaml
  address: 0.0.0.0
schedulerExtraArgs:
  address: 0.0.0.0
controllerManagerExtraVolumes:
  - name: cloud-config
    hostPath: /etc/kubernetes/cloud-config
    mountPath: /etc/kubernetes/cloud-config
EOF

kubeadm init --config /etc/kubernetes/kubeadm_config.yaml

# install weavenet
sysctl net.bridge.bridge-nf-call-iptables=1
export kubever=$(kubectl version --kubeconfig /etc/kubernetes/admin.conf | base64 | tr -d '\n')
kubectl apply --kubeconfig /etc/kubernetes/admin.conf -f "https://cloud.weave.works/k8s/net?env.CHECKPOINT_DISABLE=1&env.IPALLOC_RANGE=${POD_CIDR}&disable-npc=true&k8s-version=$kubever"

for tries in $(seq 1 60); do
	kubectl --kubeconfig /etc/kubernetes/kubelet.conf annotate --overwrite node $(hostname) machine=${MACHINE} && break
	sleep 1
done

{{- end }} {{/* end configure */}}
`
