//go:build e2e
// +build e2e

/*
Copyright 2026.

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

package e2e

import (
	"fmt"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/okedeji/hybernate/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/hybernate:v0.0.1"
	// pauseImage runs the Deployment the lifecycle test manages. The Makefile
	// preloads it into kind so the test doesn't depend on a registry pull.
	pauseImage = "registry.k8s.io/pause:3.10"
	// pluginBinary is the kubectl plugin, built by the suite.
	pluginBinary = "bin/kubectl-hybernate"
	// webImage serves HTTP for the wake-on-request spec; also preloaded.
	webImage = "registry.k8s.io/e2e-test-images/agnhost:2.52"
	// ingressNginxManifest is the project's final release; it's archived, but
	// still widely run, so wake on request is tested through it.
	ingressNginxManifest = "https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/baremetal/deploy.yaml"
	// kedaManifest is pinned so the autoscaler specs don't change under
	// the suite; its images are preloaded by the Makefile.
	kedaManifest = "https://github.com/kedacore/keda/releases/download/v2.20.2/keda-2.20.2.yaml"
	// metricsServerManifest is pinned so the idle clock spec doesn't change
	// under the suite; its image is preloaded by the Makefile.
	metricsServerManifest = "https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml"
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting hybernate e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

// The specs run in parallel, each container of them in order on its own,
// against one cluster: the first process builds and deploys Hybernate and
// installs what every spec needs, and the others wait for it.
var _ = SynchronizedBeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("building the kubectl plugin")
	_, err = utils.Run(exec.Command("go", "build", "-o", pluginBinary, "./cmd/kubectl-hybernate"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the kubectl plugin")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	By("installing metrics-server, which the activity clock needs to read CPU")
	_, err = utils.Run(exec.Command("kubectl", "apply", "-f", metricsServerManifest))
	Expect(err).NotTo(HaveOccurred())
	// kind's kubelets serve self-signed certificates.
	_, err = utils.Run(exec.Command("kubectl", "patch", "deployment", "metrics-server", "-n", "kube-system",
		"--type=json", "-p", `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`))
	Expect(err).NotTo(HaveOccurred())

	By("creating the manager namespace, with the restricted security policy")
	_, err = utils.Run(exec.Command("kubectl", "create", "ns", namespace))
	Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")
	_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted"))
	Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

	By("installing CRDs and deploying the controller-manager")
	_, err = utils.Run(exec.Command("make", "install"))
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")
	_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage)))
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

	_, err = utils.Run(exec.Command("kubectl", "wait", "--for=condition=Available",
		"apiservice/v1beta1.metrics.k8s.io", "--timeout=3m"))
	Expect(err).NotTo(HaveOccurred())
}, func() {})

var _ = SynchronizedAfterSuite(func() {}, func() {
	// The specs delete their namespaces without waiting, so their
	// ManagedWorkloads can still hold the cleanup finalizer. Once the
	// operator is gone nothing releases it, and deleting the CRD hangs.
	By("deleting every ManagedWorkload while the operator can still release its finalizer")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "managedworkloads", "--all", "--all-namespaces", "--timeout=2m"))

	By("undeploying the controller-manager, uninstalling CRDs, and removing its namespace")
	_, _ = utils.Run(exec.Command("make", "undeploy"))
	_, _ = utils.Run(exec.Command("make", "uninstall"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace))
})
