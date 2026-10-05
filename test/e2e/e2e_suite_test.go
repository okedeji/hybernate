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
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/okedeji/hybernate/test/utils"
)

var (
	// managerImage is built from the working tree and loaded into kind. Its
	// tag isn't latest, so the kubelet never tries to pull it.
	managerImage = "hybernate:e2e"
	// pauseImage runs the Deployment the lifecycle test manages. The Makefile
	// preloads it into kind so the test doesn't depend on a registry pull.
	pauseImage = "registry.k8s.io/pause:3.10"
	// pluginBinary is the kubectl plugin, built by the suite.
	pluginBinary = "bin/kubectl-hybernate"
	// webImage serves HTTP for the wake-on-request spec; also preloaded.
	webImage = "registry.k8s.io/e2e-test-images/agnhost:2.52"
)

// The third-party manifests are vendored, so the suite never depends on
// GitHub being reachable or a release asset staying the same. Their images
// are the ones the Makefile preloads.
var (
	// ingressNginxManifest is the project's final release; it's archived, but
	// still widely run, so wake on request is tested through it.
	//go:embed testdata/ingress-nginx-controller-v1.15.1.yaml
	ingressNginxManifest string
	//go:embed testdata/keda-2.20.2.yaml
	kedaManifest string
	//go:embed testdata/metrics-server-v0.7.2.yaml
	metricsServerManifest string
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting hybernate e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

// The specs run in parallel against one cluster: the first process builds
// and deploys Hybernate and installs everything any spec needs, cluster-wide
// add-ons included, and the others wait for it. Nothing cluster-wide is
// installed or removed while specs run, so no spec can pull an add-on, or
// an API it serves, out from under another.
var _ = SynchronizedBeforeSuite(func() {
	By("building the manager image")
	// The build downloads the base image and Go modules, and a CI runner's
	// network drops a stream now and then; a retry gets past that, while a
	// real build failure fails every attempt.
	var err error
	for range 3 {
		if _, err = utils.Run(exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))); err == nil {
			break
		}
	}
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("building the kubectl plugin")
	_, err = utils.Run(exec.Command("go", "build", "-o", pluginBinary, "./cmd/kubectl-hybernate"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the kubectl plugin")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	By("installing metrics-server, which the activity clock needs to read CPU")
	Expect(kubectlApply(preloaded(metricsServerManifest))).To(Succeed())
	// kind's kubelets serve self-signed certificates.
	_, err = utils.Run(exec.Command("kubectl", "patch", "deployment", "metrics-server", "-n", "kube-system",
		"--type=json", "-p", `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`))
	Expect(err).NotTo(HaveOccurred())

	By("installing ingress-nginx")
	Expect(kubectlApply(preloaded(ingressNginxManifest))).To(Succeed())

	By("installing KEDA")
	// KEDA's CRDs are too large for a client-side apply's annotation.
	cmd := exec.Command("kubectl", "apply", "--server-side", "-f", "-")
	cmd.Stdin = strings.NewReader(preloaded(kedaManifest))
	_, err = utils.Run(cmd)
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
	Expect(deploy(managerImage)).To(Succeed(), "Failed to deploy the controller-manager")

	By("waiting for everything installed to be ready")
	for _, d := range []struct{ namespace, name string }{
		{namespace, "hybernate-controller-manager"},
		{namespace, "hybernate-doorman"},
		{"ingress-nginx", "ingress-nginx-controller"},
		{"keda", "keda-operator"},
		{"keda", "keda-metrics-apiserver"},
		{"keda", "keda-admission"},
	} {
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+d.name,
			"-n", d.namespace, "--timeout=5m"))
		Expect(err).NotTo(HaveOccurred())
	}
	for _, api := range []string{"v1beta1.metrics.k8s.io", "v1beta1.external.metrics.k8s.io"} {
		_, err = utils.Run(exec.Command("kubectl", "wait", "--for=condition=Available", "apiservice/"+api,
			"--timeout=5m"))
		Expect(err).NotTo(HaveOccurred())
	}
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

// deploy applies config/default with both of its Deployments running image,
// through an overlay in a temporary directory. `make deploy` would set the
// image by editing config/manager/kustomization.yaml, leaving the working
// tree changed.
func deploy(image string) error {
	if _, err := utils.Run(exec.Command("make", "kustomize")); err != nil {
		return err
	}
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		return err
	}
	overlay, err := os.MkdirTemp("", "hybernate-e2e-deploy-")
	if err != nil {
		return fmt.Errorf("creating the overlay directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(overlay) }() // a leftover temp dir is harmless
	// Kustomize refuses an absolute path to a base, but takes a relative one
	// that leaves the overlay's directory.
	base, err := filepath.Rel(overlay, filepath.Join(projectDir, "config", "default"))
	if err != nil {
		return fmt.Errorf("locating config/default from the overlay: %w", err)
	}
	kustomization := fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [%q]
patches:
  - target: {kind: Deployment}
    patch: |-
      - {op: replace, path: /spec/template/spec/containers/0/image, value: %q}
`, base, image)
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(kustomization), 0o600); err != nil {
		return fmt.Errorf("writing the overlay: %w", err)
	}
	rendered, err := utils.Output(exec.Command(kustomizeBinary(projectDir), "build", overlay))
	if err != nil {
		return err
	}
	return kubectlApply(rendered)
}

// kustomizeBinary is where `make kustomize` put kustomize, following the
// Makefile's KUSTOMIZE and LOCALBIN.
func kustomizeBinary(projectDir string) string {
	if path, ok := os.LookupEnv("KUSTOMIZE"); ok {
		return path
	}
	if dir, ok := os.LookupEnv("LOCALBIN"); ok {
		return filepath.Join(dir, "kustomize")
	}
	return filepath.Join(projectDir, "bin", "kustomize")
}

var imageDigest = regexp.MustCompile(`@sha256:[0-9a-f]{64}`)

// preloaded is a manifest that uses the images the Makefile preloads into
// kind. Manifests pin images by the digest of their multi-platform index,
// which the single-platform images preloaded don't carry, or pull them
// Always; either way kind would pull them again, and that pull is what
// timed specs out.
func preloaded(manifest string) string {
	manifest = imageDigest.ReplaceAllString(manifest, "")
	return strings.ReplaceAll(manifest, "imagePullPolicy: Always", "imagePullPolicy: IfNotPresent")
}
