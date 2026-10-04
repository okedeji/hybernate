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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/okedeji/hybernate/test/utils"
)

// namespace where the project is deployed in
const namespace = "hybernate-system"

// serviceAccountName created for the project
const serviceAccountName = "hybernate-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "hybernate-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "hybernate-metrics-binding"

var _ = Describe("Manager", func() {
	var controllerPodName string

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager", "-n", namespace,
				"--tail=500")
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching doorman pod logs")
			cmd = exec.Command("kubectl", "logs", "-l", "control-plane=doorman", "-n", namespace,
				"--prefix", "--tail=200")
			doormanLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Doorman logs:\n %s", doormanLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get doorman logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", "-l", "control-plane=controller-manager", "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", Ordered, func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=hybernate-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:8.7.1",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:8.7.1",
							"imagePullPolicy": "IfNotPresent",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("ManagedWorkload lifecycle", Ordered, func() {
		const (
			appNamespace = "hybernate-e2e-apps"
			appName      = "e2e-app"
		)

		BeforeAll(func() {
			By("creating a namespace and a two-replica Deployment to manage")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", appNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", appNamespace, "--wait=false"))
			})

			Expect(kubectlApply(deploymentManifest(appName, appNamespace, 2))).To(Succeed())

			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+appName,
				"-n", appNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("pauses, resumes, and releases a workload", func() {
			By("requesting a pause")
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  target: {kind: Deployment, name: %[1]s}
  desiredState: Paused
  prediction: {confidence: 85}
`, appName, appNamespace))).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", appName, appNamespace, "{.status.phase}")).To(Equal("Paused"))
				g.Expect(jsonpath("deployment", appName, appNamespace, "{.spec.replicas}")).To(Equal("0"))
			}).Should(Succeed())

			By("checking the API server accepted the events.k8s.io event")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "events.events.k8s.io", "-n", appNamespace,
					"-o", `jsonpath={range .items[?(@.reason=="Paused")]}{.regarding.name}/{.action}{"\n"}{end}`))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring(appName + "/Pause"))
			}).Should(Succeed())

			By("resuming to the previous replica count")
			_, err := utils.Run(exec.Command("kubectl", "patch", "managedworkload", appName, "-n", appNamespace,
				"--type=merge", "-p", `{"spec":{"desiredState":"Running"}}`))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", appName, appNamespace, "{.status.phase}")).To(Equal("Running"))
				g.Expect(jsonpath("deployment", appName, appNamespace, "{.status.readyReplicas}")).To(Equal("2"))
			}).Should(Succeed())

			By("deleting the ManagedWorkload, which must release its finalizer and leave the Deployment")
			_, err = utils.Run(exec.Command("kubectl", "delete", "managedworkload", appName,
				"-n", appNamespace, "--timeout=1m"))
			Expect(err).NotTo(HaveOccurred())
			Expect(jsonpath("deployment", appName, appNamespace, "{.spec.replicas}")).To(Equal("2"))
		})
	})

	Context("Idle clock", Ordered, func() {
		const (
			idleNamespace = "hybernate-e2e-idle"
			idleName      = "e2e-idle-app"
		)

		BeforeAll(func() {
			By("creating an idle Deployment")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", idleNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", idleNamespace, "--wait=false"))
			})
			Expect(kubectlApply(deploymentManifest(idleName, idleNamespace, 1))).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+idleName,
				"-n", idleNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("pauses a workload with no activity, and wakes it on an activity annotation", func() {
			By("managing it with a one-minute idle clock")
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  target: {kind: Deployment, name: %[1]s}
  idlePolicy:
    idleAfter: 1m
  prediction: {confidence: 85}
`, idleName, idleNamespace))).To(Succeed())

			By("waiting for the clock to run out and the workload to pause")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", idleName, idleNamespace, "{.status.phase}")).To(Equal("Paused"))
				g.Expect(jsonpath("deployment", idleName, idleNamespace, "{.spec.replicas}")).To(Equal("0"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("annotating the Deployment as active, as a developer portal would")
			_, err := utils.Run(exec.Command("kubectl", "annotate", "deployment", idleName, "-n", idleNamespace,
				"--overwrite", "hybernate.io/last-activity="+time.Now().UTC().Format(time.RFC3339)))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", idleName, idleNamespace, "{.status.phase}")).To(Equal("Running"))
				g.Expect(jsonpath("deployment", idleName, idleNamespace, "{.status.readyReplicas}")).To(Equal("1"))
				g.Expect(jsonpath("managedworkload", idleName, idleNamespace,
					"{.status.activity.lastActivitySource}")).To(Equal("woke"))
			}).Should(Succeed())
		})
	})

	Context("Dependencies", Ordered, func() {
		const depsNamespace = "hybernate-e2e-deps"

		BeforeAll(func() {
			By("creating an app and the database it depends on")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", depsNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", depsNamespace, "--wait=false"))
			})
			for _, name := range []string{"e2e-db", "e2e-app"} {
				Expect(kubectlApply(deploymentManifest(name, depsNamespace, 1))).To(Succeed())
				_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+name,
					"-n", depsNamespace, "--timeout=2m"))
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("holds a dependency while its dependent is awake, and wakes it with the dependent", func() {
			By("managing the database with a shorter clock than the app that depends on it")
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: e2e-db, namespace: %[1]s}
spec:
  target: {kind: Deployment, name: e2e-db}
  idlePolicy: {idleAfter: 1m}
  prediction: {confidence: 85}
---
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: e2e-app, namespace: %[1]s}
spec:
  target: {kind: Deployment, name: e2e-app}
  dependsOn:
    - {kind: Deployment, name: e2e-db}
  idlePolicy: {idleAfter: 2m}
  prediction: {confidence: 85}
`, depsNamespace))).To(Succeed())

			By("checking the database is held awake once its own clock runs out")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", "e2e-db", depsNamespace,
					`{.status.conditions[?(@.type=="HeldByDependents")].status}`)).To(Equal("True"))
				g.Expect(jsonpath("managedworkload", "e2e-db", depsNamespace, "{.status.phase}")).To(Equal("Running"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("checking both pause once the app's clock runs out")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", "e2e-app", depsNamespace, "{.status.phase}")).To(Equal("Paused"))
				g.Expect(jsonpath("managedworkload", "e2e-db", depsNamespace, "{.status.phase}")).To(Equal("Paused"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waking the app with kubectl hybernate wake, which must wake the database too")
			out, err := utils.Run(exec.Command(pluginBinary, "wake", "e2e-app", "-n", depsNamespace, "--timeout", "3m"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("is Running after"), "wake waits until the workload is Running")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", "e2e-app", depsNamespace, "{.status.phase}")).To(Equal("Running"))
				g.Expect(jsonpath("managedworkload", "e2e-db", depsNamespace, "{.status.phase}")).To(Equal("Running"))
				g.Expect(jsonpath("deployment", "e2e-db", depsNamespace, "{.status.readyReplicas}")).To(Equal("1"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("Wake on request", Ordered, func() {
		const (
			wakeNamespace = "hybernate-e2e-wake"
			webName       = "e2e-web"
			webHost       = "e2e-web.example.com"
			ingressURL    = "http://ingress-nginx-controller.ingress-nginx/hostname"
		)
		doormanSlice := webName + "-hybernate-doorman"

		BeforeAll(func() {
			By("waiting for the doorman to be ready")
			_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/hybernate-doorman",
				"-n", namespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())

			By("creating a web app behind a Service")
			_, err = utils.Run(exec.Command("kubectl", "create", "ns", wakeNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", wakeNamespace, "--wait=false"))
			})
			Expect(kubectlApply(webManifest(webName, wakeNamespace))).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+webName,
				"-n", wakeNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())

			By("installing ingress-nginx")
			manifest, err := preloaded(ingressNginxManifest)
			Expect(err).NotTo(HaveOccurred())
			Expect(kubectlApply(manifest)).To(Succeed())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", ingressNginxManifest, "--wait=false"))
			})
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/ingress-nginx-controller",
				"-n", "ingress-nginx", "--timeout=3m"))
			Expect(err).NotTo(HaveOccurred())

			By("routing a host to the app")
			// The admission webhook can still be starting after the rollout
			// reports ready, so the first apply may be refused.
			Eventually(func() error {
				return kubectlApply(fmt.Sprintf(`
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  ingressClassName: nginx
  rules:
    - host: %[3]s
      http:
        paths:
          - path: /
            pathType: Prefix
            backend: {service: {name: %[1]s, port: {name: http}}}
`, webName, wakeNamespace, webHost))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("checking the route serves the app while it's awake")
			// nginx loads a new Ingress a few seconds after it's created, so
			// this retries; the wake spec then fails only on the doorman.
			body := curlInCluster("curl-ingress-ready", wakeNamespace, ingressURL,
				"-H", "Host: "+webHost, "--retry", "30", "--retry-delay", "2", "--retry-all-errors")
			Expect(body).To(HavePrefix(webName + "-"))

			By("managing the app with a one-minute idle clock")
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  target: {kind: Deployment, name: %[1]s}
  idlePolicy: {idleAfter: 1m}
  prediction: {confidence: 85}
`, webName, wakeNamespace))).To(Succeed())
		})

		waitForDoorman := func() {
			By("waiting for the app to pause and its Service to point at the doorman")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", webName, wakeNamespace, "{.status.phase}")).To(Equal("Paused"))
				g.Expect(jsonpath("managedworkload", webName, wakeNamespace,
					`{.status.conditions[?(@.type=="WakeOnRequest")].status}`)).To(Equal("True"))
				g.Expect(jsonpath("endpointslice", doormanSlice, wakeNamespace, "{.endpoints[*].addresses[0]}")).
					NotTo(BeEmpty())
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		}

		expectAwake := func() {
			By("checking the app is Running and its Service no longer points at the doorman")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", webName, wakeNamespace, "{.status.phase}")).To(Equal("Running"))
				g.Expect(jsonpath("managedworkload", webName, wakeNamespace,
					"{.status.activity.lastActivitySource}")).To(Equal("request"))
				_, err := jsonpath("endpointslice", doormanSlice, wakeNamespace, "{.metadata.name}")
				g.Expect(err).To(HaveOccurred(), "the doorman's EndpointSlice must be removed")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		}

		It("wakes a paused app on a request to its Service and answers the request", func() {
			waitForDoorman()

			By("sending a request to the paused app's Service")
			// kube-proxy programs the doorman's endpoints a moment after the
			// pause, and until then the Service has none and refuses. curl
			// retries only that, never a connection the doorman holds.
			body := curlInCluster("curl-wake", wakeNamespace, fmt.Sprintf("http://%s/hostname", webName),
				"--retry", "10", "--retry-delay", "1", "--retry-connrefused")
			Expect(body).To(HavePrefix(webName+"-"), "the held request is answered by the woken pod")

			expectAwake()
		})

		It("wakes a paused app on a request through ingress-nginx", func() {
			waitForDoorman()

			By("sending a request through the ingress controller")
			// nginx picks up endpoint changes on a timer, so for a moment after
			// the pause it still sends to the deleted pod and answers 502. curl
			// retries only those errors, never a connection the doorman holds.
			body := curlInCluster("curl-wake-ingress", wakeNamespace, ingressURL, "-H", "Host: "+webHost,
				"--retry", "10", "--retry-delay", "1", "--retry-connrefused")
			Expect(body).To(HavePrefix(webName+"-"), "ingress-nginx passes the held request to the woken pod")

			expectAwake()
		})

		It("shows a browser a waking-up page through ingress-nginx, then the app", func() {
			waitForDoorman()
			browser := []string{"-s", "--max-time", "30", "-w", "\nstatus=%{http_code}", "-H", "Host: " + webHost,
				"-H", "Accept: text/html", "-H", "Sec-Fetch-Mode: navigate"}

			By("opening the paused app in a browser")
			// Until nginx picks up the doorman's endpoints, a request can still
			// reach the removed pod, so this looks for the page across a few tries.
			attempt := 0
			Eventually(func(g Gomega) {
				attempt++
				page, _ := runCurl(fmt.Sprintf("curl-page-%d", attempt), wakeNamespace, ingressURL, browser...)
				g.Expect(page).To(ContainSubstring("Waking up " + webName))
				g.Expect(page).To(ContainSubstring("status=503"))
			}, 2*time.Minute, time.Second).Should(Succeed())

			expectAwake()

			By("reloading once the app is Running")
			reload := 0
			Eventually(func(g Gomega) {
				reload++
				app, _ := runCurl(fmt.Sprintf("curl-page-reload-%d", reload), wakeNamespace, ingressURL, browser...)
				g.Expect(app).To(ContainSubstring("status=200"))
				g.Expect(app).To(HavePrefix(webName+"-"), "the reload reaches the app")
			}, time.Minute, time.Second).Should(Succeed())
		})

		It("learns that a workload Hybernate manages depends on the one its request woke", func() {
			const callerName = "e2e-caller"
			By("running a caller that Hybernate manages, with nothing in its environment naming the app")
			Expect(kubectlApply(webManifest(callerName, wakeNamespace))).To(Succeed())
			_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+callerName,
				"-n", wakeNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  target: {kind: Deployment, name: %[1]s}
  idlePolicy: {idleAfter: 1h}
  prediction: {confidence: 85}
`, callerName, wakeNamespace))).To(Succeed())

			waitForDoorman()

			By("connecting from the caller to the paused app")
			// Retried until kube-proxy routes the Service to the doorman; a
			// connection the doorman holds isn't refused.
			Eventually(func() error {
				_, err := utils.Run(exec.Command("kubectl", "exec", "-n", wakeNamespace, "deploy/"+callerName, "--",
					"/agnhost", "connect", "--timeout", "5s", webName+":80"))
				return err
			}, time.Minute, 2*time.Second).Should(Succeed())

			expectAwake()

			By("checking the caller now depends on the app, learned from the wake")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", callerName, wakeNamespace,
					"{.status.learnedDependencies.dependencies[*].name}")).To(Equal(webName))
				g.Expect(jsonpath("managedworkload", callerName, wakeNamespace,
					"{.status.learnedDependencies.dependencies[*].source}")).To(Equal("wake"))
			}, time.Minute, 5*time.Second).Should(Succeed())
			out, err := utils.Run(exec.Command(pluginBinary, "deps", webName, "-n", wakeNamespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(MatchRegexp(wakeNamespace + `/` + callerName + `\s+learned from a wake\s+Running`))
		})
	})

	Context("Scan", Ordered, func() {
		const scanNamespace = "hybernate-e2e-scan"

		BeforeAll(func() {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", scanNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", scanNamespace, "--wait=false"))
			})
			By("labelling the kind nodes as an EKS instance type, as a cloud provider would")
			_, err = utils.Run(exec.Command("kubectl", "label", "nodes", "--all", "--overwrite",
				"node.kubernetes.io/instance-type=m6i.large", "topology.kubernetes.io/region=us-east-1"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "label", "nodes", "--all",
					"node.kubernetes.io/instance-type-", "topology.kubernetes.io/region-"))
			})
			Expect(kubectlApply(deploymentManifest("e2e-quiet", scanNamespace, 2))).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/e2e-quiet",
				"-n", scanNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: e2e-quiet, namespace: %s}
spec:
  target: {kind: Deployment, name: e2e-quiet}
  idlePolicy: {idleAfter: 1m}
  prediction: {confidence: 85}
`, scanNamespace))).To(Succeed())
		})

		It("reports what Hybernate has paused and what that frees", func() {
			type scanned struct {
				Workloads []struct {
					Name       string  `json:"name"`
					State      string  `json:"state"`
					Reason     string  `json:"reason"`
					Replicas   int     `json:"replicas"`
					Managed    bool    `json:"managed"`
					HourlyCost float64 `json:"hourlyCost"`
				} `json:"workloads"`
				Totals struct {
					Paused int `json:"paused"`
				} `json:"totals"`
				NodePrices struct {
					NodeTypes []struct {
						InstanceType string `json:"instanceType"`
						Listed       bool   `json:"listed"`
					} `json:"nodeTypes"`
				} `json:"nodePrices"`
			}
			By("checking Hybernate priced the workload at its nodes' list price while it ran")
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", "e2e-quiet", scanNamespace, "{.status.cost.listRates.cpuPerHour}")
			}, 2*time.Minute, 5*time.Second).ShouldNot(BeEmpty(), "the operator can read the nodes it runs on")

			By("scanning once Hybernate has paused the quiet workload")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", scanNamespace, "-o", "json"))
				g.Expect(err).NotTo(HaveOccurred())
				var result scanned
				g.Expect(json.Unmarshal([]byte(out), &result)).To(Succeed())
				g.Expect(result.Workloads).To(HaveLen(1))
				w := result.Workloads[0]
				g.Expect(w.Name).To(Equal("e2e-quiet"))
				g.Expect(w.State).To(Equal("paused"))
				g.Expect(w.Reason).To(HavePrefix("paused "))
				g.Expect(w.Replicas).To(Equal(2), "priced on the replicas it ran before the pause")
				g.Expect(w.Managed).To(BeTrue())
				g.Expect(w.HourlyCost).To(BeNumerically(">", 0))
				g.Expect(result.Totals.Paused).To(Equal(1))
				g.Expect(result.NodePrices.NodeTypes).To(ContainElement(SatisfyAll(
					HaveField("InstanceType", "m6i.large"), HaveField("Listed", true))))
			}, 4*time.Minute, 10*time.Second).Should(Succeed())

			By("pricing at the user's own prices in place of the nodes'")
			out, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", scanNamespace, "-o", "json",
				"--cpu-price", "10", "--memory-price", "0"))
			Expect(err).NotTo(HaveOccurred())
			var own scanned
			Expect(json.Unmarshal([]byte(out), &own)).To(Succeed())
			Expect(own.Workloads).To(HaveLen(1))
			Expect(own.Workloads[0].HourlyCost).To(BeNumerically("~", 2*0.010*10, 1e-6),
				"two replicas of 10m at $10 per vCPU-hour")
		})
	})

	Context("Scan with history", Ordered, func() {
		const (
			historyNamespace = "hybernate-e2e-history"
			promNamespace    = "hybernate-e2e-monitoring"
			historyName      = "e2e-quiet-history"
		)

		BeforeAll(func() {
			for _, ns := range []string{historyNamespace, promNamespace} {
				_, err := utils.Run(exec.Command("kubectl", "create", "ns", ns))
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() {
					_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", ns, "--wait=false"))
				})
			}
			Expect(kubectlApply(deploymentManifest(historyName, historyNamespace, 1))).To(Succeed())
			_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+historyName,
				"-n", historyNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
			pod, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", historyNamespace,
				"-l", "app="+historyName, "-o", "jsonpath={.items[0].metadata.name}"))
			Expect(err).NotTo(HaveOccurred())

			By("standing in for Prometheus with three hours of quiet CPU for the workload's pod")
			// agnhost porter answers every path with the same body, which
			// serves both the scan's check and its range query.
			end := time.Now().Truncate(5 * time.Minute)
			var values []string
			for at := end.Add(-3 * time.Hour); !at.After(end); at = at.Add(5 * time.Minute) {
				values = append(values, fmt.Sprintf(`[%d,"0.0001"]`, at.Unix()))
			}
			body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[`+
				`{"metric":{"pod":%q,"container":"app"},"values":[%s]}]}}`, pod, strings.Join(values, ","))
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata: {name: fake-prometheus, namespace: %[1]s, labels: {app: fake-prometheus}}
spec:
  containers:
    - name: porter
      image: %[2]s
      args: [porter]
      env:
        - {name: SERVE_PORT_9090, value: %[3]q}
      readinessProbe: {tcpSocket: {port: 9090}}
---
apiVersion: v1
kind: Service
metadata: {name: prometheus-operated, namespace: %[1]s, labels: {operated-prometheus: "true"}}
spec:
  selector: {app: fake-prometheus}
  ports: [{name: web, port: 9090}]
`, promNamespace, webImage, body))).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "wait", "pod/fake-prometheus", "-n", promNamespace,
				"--for=condition=Ready", "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("finds Prometheus, reads it through the API server, and replays the clock", func() {
			type scanned struct {
				Mode    string `json:"mode"`
				History struct {
					Prometheus string `json:"prometheus"`
				} `json:"history"`
				Workloads []struct {
					Name    string `json:"name"`
					History *struct {
						SleepHours float64 `json:"sleepHours"`
						Freed      float64 `json:"freed"`
					} `json:"history"`
				} `json:"workloads"`
			}
			out, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", historyNamespace, "-o", "json"))
			Expect(err).NotTo(HaveOccurred())
			var c scanned
			Expect(json.Unmarshal([]byte(out), &c)).To(Succeed())
			Expect(c.Mode).To(Equal("history"))
			Expect(c.History.Prometheus).To(Equal(promNamespace + "/prometheus-operated"))
			Expect(c.Workloads).To(HaveLen(1))
			Expect(c.Workloads[0].History).NotTo(BeNil())
			Expect(c.Workloads[0].History.SleepHours).To(BeNumerically(">=", 1),
				"quiet for three hours, it would have slept from an hour in until its deploy")
			Expect(c.Workloads[0].History.Freed).To(BeNumerically(">", 0))
		})

		It("tells a user with only view what an admin can run to let them read Prometheus, and that works", func() {
			By("scanning as a service account with the standard view role")
			_, err := utils.Run(exec.Command("kubectl", "create", "serviceaccount", "scanner", "-n", historyNamespace))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "create", "clusterrolebinding", "hybernate-e2e-scanner-view",
				"--clusterrole=view", "--serviceaccount="+historyNamespace+":scanner"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding", "hybernate-e2e-scanner-view"))
			})
			token, err := utils.Run(exec.Command("kubectl", "create", "token", "scanner", "-n", historyNamespace))
			Expect(err).NotTo(HaveOccurred())
			kubeconfig := filepath.Join(GinkgoT().TempDir(), "scanner.kubeconfig")
			raw, err := utils.Run(exec.Command("kubectl", "config", "view", "--minify", "--flatten", "--raw"))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(kubeconfig, []byte(raw), 0o600)).To(Succeed())
			for _, args := range [][]string{
				{"config", "set-credentials", "scanner", "--token=" + strings.TrimSpace(token)},
				{"config", "set-context", "--current", "--user=scanner"},
			} {
				_, err = utils.Run(exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...))
				Expect(err).NotTo(HaveOccurred())
			}
			scanAs := func() (mode string, access []string) {
				out, err := utils.Run(exec.Command("env", "KUBECONFIG="+kubeconfig,
					pluginBinary, "scan", "-n", historyNamespace, "-o", "json"))
				Expect(err).NotTo(HaveOccurred())
				var result struct {
					Mode          string   `json:"mode"`
					HistoryAccess []string `json:"historyAccess"`
				}
				Expect(json.Unmarshal([]byte(out), &result)).To(Succeed())
				return result.Mode, result.HistoryAccess
			}

			mode, access := scanAs()
			Expect(mode).To(Equal("snapshot"), "view doesn't include services/proxy")
			Expect(access).To(HaveLen(2))
			Expect(access[1]).To(HaveSuffix("--serviceaccount=" + historyNamespace + ":scanner"))

			By("running the commands it printed, as an admin")
			for _, command := range access {
				args := strings.Fields(command)
				Expect(args[0]).To(Equal("kubectl"))
				_, err = utils.Run(exec.Command("kubectl", args[1:]...))
				Expect(err).NotTo(HaveOccurred())
			}

			mode, access = scanAs()
			Expect(mode).To(Equal("history"), "the Role the scan printed is enough")
			Expect(access).To(BeEmpty())
		})
	})

	Context("Scan dependencies", Ordered, func() {
		const depsScanNamespace = "hybernate-e2e-deps-scan"

		BeforeAll(func() {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", depsScanNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", depsScanNamespace, "--wait=false"))
			})
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: e2e-db, namespace: %[1]s}
spec:
  serviceName: e2e-db-hl
  replicas: 1
  selector: {matchLabels: {app: e2e-db}}
  template:
    metadata: {labels: {app: e2e-db}}
    spec:
      containers:
        - {name: db, image: %[2]s, resources: {requests: {cpu: 10m, memory: 16Mi}}}
---
apiVersion: v1
kind: Service
metadata: {name: e2e-db-hl, namespace: %[1]s}
spec:
  clusterIP: None
  selector: {app: e2e-db}
  ports: [{port: 5432}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: e2e-app-config, namespace: %[1]s}
data:
  PGHOST: e2e-db-0.e2e-db-hl
`, depsScanNamespace, pauseImage))).To(Succeed())
			Expect(kubectlApply(deploymentManifest("e2e-app", depsScanNamespace, 1))).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "set", "env", "deployment/e2e-app", "-n", depsScanNamespace,
				"--from=configmap/e2e-app-config"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("finds a dependency on a headless address in a ConfigMap and says to declare it", func() {
			out, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", depsScanNamespace, "--window", "0", "-o", "json"))
			Expect(err).NotTo(HaveOccurred())
			var result struct {
				Workloads []struct {
					Name         string `json:"name"`
					Dependencies []struct {
						Kind     string `json:"kind"`
						Name     string `json:"name"`
						Via      string `json:"via"`
						Headless bool   `json:"headless"`
					} `json:"dependencies"`
				} `json:"workloads"`
			}
			Expect(json.Unmarshal([]byte(out), &result)).To(Succeed())
			var app []string
			for _, w := range result.Workloads {
				if w.Name != "e2e-app" {
					continue
				}
				for _, d := range w.Dependencies {
					app = append(app, fmt.Sprintf("%s/%s via %s headless=%t", d.Kind, d.Name, d.Via, d.Headless))
				}
			}
			Expect(app).To(ConsistOf("StatefulSet/e2e-db via PGHOST headless=true"))

			table, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", depsScanNamespace, "--window", "0"))
			Expect(err).NotTo(HaveOccurred())
			Expect(table).To(MatchRegexp(`deployment/e2e-app\s+->\s+statefulset/e2e-db\s+PGHOST\s+connected once Hybernate manages it`))
		})

		It("writes the HTML report, without dependency addresses", func() {
			report := filepath.Join(GinkgoT().TempDir(), "workload-scan.html")
			out, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", depsScanNamespace, "--window", "0",
				"--html", report))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Report: " + report))
			page, err := os.ReadFile(report)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(page)).To(ContainSubstring("<h1>Workload scan</h1>"))
			Expect(string(page)).To(ContainSubstring("deployment/e2e-app"))
			Expect(string(page)).To(ContainSubstring("PGHOST"), "the variable is named")
			Expect(string(page)).NotTo(ContainSubstring("e2e-db-0.e2e-db-hl"), "its address isn't")
		})

		It("learns the dependency once Hybernate manages the app, and holds the database awake for it", func() {
			for name, kind := range map[string]string{"e2e-app": "Deployment", "e2e-db": "StatefulSet"} {
				idleAfter := "1h"
				if name == "e2e-db" {
					idleAfter = "1m"
				}
				Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  target: {kind: %[3]s, name: %[1]s}
  idlePolicy: {idleAfter: %[4]s}
  prediction: {confidence: 85}
`, name, depsScanNamespace, kind, idleAfter))).To(Succeed())
			}

			By("learning it from the app's environment, with no dependsOn")
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", "e2e-app", depsScanNamespace,
					"{.status.learnedDependencies.dependencies[*].name}")
			}, time.Minute, 5*time.Second).Should(Equal("e2e-db"))

			By("holding the database awake past its own idle clock, for the app")
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", "e2e-db", depsScanNamespace,
					`{.status.conditions[?(@.type=="HeldByDependents")].status}`)
			}, 4*time.Minute, 10*time.Second).Should(Equal("True"))
			Expect(jsonpath("statefulset", "e2e-db", depsScanNamespace, "{.spec.replicas}")).To(Equal("1"))

			table, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", depsScanNamespace, "--window", "0"))
			Expect(err).NotTo(HaveOccurred())
			Expect(table).To(MatchRegexp(`deployment/e2e-app\s+->\s+statefulset/e2e-db\s+PGHOST\s+connected by Hybernate`))

			out, err := utils.Run(exec.Command(pluginBinary, "deps", "e2e-db", "-n", depsScanNamespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(MatchRegexp(`Depended on by:\n\s+` + depsScanNamespace + `/e2e-app\s+learned from PGHOST\s+Running`))
		})
	})

	Context("Label opt-in", Ordered, func() {
		const (
			optInNamespace = "hybernate-e2e-optin"
			optInName      = "e2e-labelled"
		)

		BeforeAll(func() {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", optInNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", optInNamespace, "--wait=false"))
			})
			Expect(kubectlApply(deploymentManifest(optInName, optInNamespace, 2))).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+optInName,
				"-n", optInNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("measures a labelled workload, pauses it once enabled, and restores it when the label goes", func() {
			By("opting the Deployment in with a label, measuring first")
			_, err := utils.Run(exec.Command("kubectl", "annotate", "deployment", optInName, "-n", optInNamespace,
				"hybernate.io/dry-run=true", "hybernate.io/idle-after=1m"))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "label", "deployment", optInName, "-n", optInNamespace,
				"hybernate.io/managed=true"))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", optInName, optInNamespace,
					`{.metadata.labels.hybernate\.io/from-label}`)).To(Equal("true"))
				g.Expect(jsonpath("managedworkload", optInName, optInNamespace,
					"{.metadata.ownerReferences[0].name}")).To(Equal(optInName))
				g.Expect(jsonpath("managedworkload", optInName, optInNamespace, "{.spec.dryRun}")).To(Equal("true"))
				g.Expect(jsonpath("managedworkload", optInName, optInNamespace,
					"{.spec.idlePolicy.idleAfter}")).To(Equal("1m0s"))
			}).Should(Succeed())

			By("checking dry-run reaches Idle without pausing")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", optInName, optInNamespace, "{.status.phase}")).To(Equal("Idle"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
			Expect(jsonpath("deployment", optInName, optInNamespace, "{.spec.replicas}")).To(Equal("2"))
			Expect(jsonpath("managedworkload", optInName, optInNamespace, "{.status.dryRun.pauses}")).To(Equal("1"))

			By("marking it active, which ends the would-be pause dry-run counted")
			_, err = utils.Run(exec.Command("kubectl", "annotate", "deployment", optInName, "-n", optInNamespace,
				"--overwrite", "hybernate.io/last-activity="+time.Now().UTC().Format(time.RFC3339)))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", optInName, optInNamespace, "{.status.phase}")).To(Equal("Running"))
				out, err := utils.Run(exec.Command("kubectl", "get", "events.events.k8s.io", "-n", optInNamespace,
					"-o", `jsonpath={range .items[?(@.reason=="ActivityResumed")]}{.note}{"\n"}{end}`))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("would have paused 1 time"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("enabling it with kubectl hybernate enable")
			out, err := utils.Run(exec.Command(pluginBinary, "enable", optInName, "-n", optInNamespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("dry-run ended"))

			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", optInName, optInNamespace, "{.status.phase}")).To(Equal("Paused"))
				g.Expect(jsonpath("deployment", optInName, optInNamespace, "{.spec.replicas}")).To(Equal("0"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("removing the label, which must scale it back up before releasing it")
			_, err = utils.Run(exec.Command("kubectl", "label", "deployment", optInName, "-n", optInNamespace,
				"hybernate.io/managed-"))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				_, err := jsonpath("managedworkload", optInName, optInNamespace, "{.metadata.name}")
				g.Expect(err).To(HaveOccurred(), "the ManagedWorkload must be deleted")
				g.Expect(jsonpath("deployment", optInName, optInNamespace, "{.spec.replicas}")).To(Equal("2"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("GitOps", Ordered, func() {
		const (
			gitOpsNamespace = "hybernate-e2e-gitops"
			gitOpsName      = "e2e-synced"
		)
		// syncFromGit applies the Deployment as Argo CD does, taking the
		// replicas back from whoever set them, as a sync does.
		syncFromGit := func() error {
			cmd := exec.Command("kubectl", "apply", "--server-side", "--field-manager=argocd-controller",
				"--force-conflicts", "-f", "-")
			cmd.Stdin = strings.NewReader(deploymentManifest(gitOpsName, gitOpsNamespace, 2))
			_, err := utils.Run(cmd)
			return err
		}

		BeforeAll(func() {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", gitOpsNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", gitOpsNamespace, "--wait=false"))
			})
			Expect(syncFromGit()).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+gitOpsName,
				"-n", gitOpsNamespace, "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("reports Argo CD undoing a pause, with the fix, and doesn't fight it", func() {
			By("scanning, which says the replicas are set from Git before it's opted in")
			out, err := utils.Run(exec.Command(pluginBinary, "scan", "-n", gitOpsNamespace, "-o", "json", "--window", "0"))
			Expect(err).NotTo(HaveOccurred())
			var scanned struct {
				Workloads []struct {
					ReplicasFromGit string `json:"replicasFromGit"`
				} `json:"workloads"`
			}
			Expect(json.Unmarshal([]byte(out), &scanned)).To(Succeed())
			Expect(scanned.Workloads).To(HaveLen(1))
			Expect(scanned.Workloads[0].ReplicasFromGit).To(Equal("Argo CD"))

			By("managing it with a one-minute idle clock, and waiting for it to pause")
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  target: {kind: Deployment, name: %[1]s}
  idlePolicy: {idleAfter: 1m}
  prediction: {confidence: 85}
`, gitOpsName, gitOpsNamespace))).To(Succeed())
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", gitOpsName, gitOpsNamespace, "{.status.phase}")
			}, 4*time.Minute, 5*time.Second).Should(Equal("Paused"))

			By("syncing from Git, as Argo CD would")
			Expect(syncFromGit()).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", gitOpsName, gitOpsNamespace, "{.status.phase}")).To(Equal("Running"))
				g.Expect(jsonpath("managedworkload", gitOpsName, gitOpsNamespace,
					`{.status.conditions[?(@.type=="GitOpsConflict")].status}`)).To(Equal("True"))
				g.Expect(jsonpath("managedworkload", gitOpsName, gitOpsNamespace,
					`{.status.conditions[?(@.type=="GitOpsConflict")].message}`)).To(ContainSubstring("/spec/replicas"))
				g.Expect(jsonpath("managedworkload", gitOpsName, gitOpsNamespace, "{.status.lastScaledUp.by}")).
					To(Equal("argocd-controller"))
				g.Expect(jsonpath("managedworkload", gitOpsName, gitOpsNamespace, "{.status.lastScaledUp.gitOps}")).
					To(Equal("Argo CD"))
			}, time.Minute, 5*time.Second).Should(Succeed())

			By("checking it isn't paused again in a loop with Git, though its idle clock runs out")
			Consistently(func() (string, error) {
				return jsonpath("deployment", gitOpsName, gitOpsNamespace, "{.spec.replicas}")
			}, 2*time.Minute, 10*time.Second).Should(Equal("2"))
		})
	})

	Context("Autoscalers", Ordered, func() {
		const autoscaleNamespace = "hybernate-e2e-autoscale"
		manage := func(name string) {
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  target: {kind: Deployment, name: %[1]s}
  idlePolicy: {idleAfter: 1m}
  prediction: {confidence: 85}
`, name, autoscaleNamespace))).To(Succeed())
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", name, autoscaleNamespace, "{.status.phase}")
			}, 4*time.Minute, 5*time.Second).Should(Equal("Paused"))
		}
		replicas := func(name string) func() (string, error) {
			return func() (string, error) {
				return jsonpath("deployment", name, autoscaleNamespace, "{.spec.replicas}")
			}
		}
		wake := func(name string) {
			out, err := utils.Run(exec.Command(pluginBinary, "wake", name, "-n", autoscaleNamespace, "--timeout", "3m"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("is Running after"))
		}

		BeforeAll(func() {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", autoscaleNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", autoscaleNamespace, "--wait=false"))
			})

			By("installing KEDA")
			manifest, err := preloaded(kedaManifest)
			Expect(err).NotTo(HaveOccurred())
			// KEDA's CRDs are too large for a client-side apply's annotation.
			cmd := exec.Command("kubectl", "apply", "--server-side", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "--wait=false", "-f", "-")
				cmd.Stdin = strings.NewReader(manifest)
				_, _ = utils.Run(cmd)
			})
			for _, d := range []string{"keda-operator", "keda-metrics-apiserver", "keda-admission"} {
				_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+d, "-n", "keda",
					"--timeout=3m"))
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("pauses an HPA's workload, which the HPA leaves at zero, and resumes it within the HPA's range", func() {
			Expect(kubectlApply(deploymentManifest("e2e-hpa", autoscaleNamespace, 1))).To(Succeed())
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: e2e-hpa, namespace: %s}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: e2e-hpa}
  minReplicas: 2
  maxReplicas: 3
  metrics:
    - type: Resource
      resource: {name: cpu, target: {type: Utilization, averageUtilization: 80}}
`, autoscaleNamespace))).To(Succeed())
			Eventually(replicas("e2e-hpa"), 2*time.Minute, 5*time.Second).Should(Equal("2"), "the HPA scales to its minimum")

			By("managing it, and waiting for it to pause")
			manage("e2e-hpa")
			Expect(jsonpath("managedworkload", "e2e-hpa", autoscaleNamespace,
				`{.status.conditions[?(@.type=="Autoscaled")].reason}`)).To(Equal("HPA"))
			Consistently(replicas("e2e-hpa"), time.Minute, 10*time.Second).Should(Equal("0"),
				"an HPA doesn't scale a workload up from zero")

			By("waking it")
			wake("e2e-hpa")
			Expect(replicas("e2e-hpa")()).To(Equal("2"), "within the HPA's range")
		})

		It("holds a KEDA workload at zero through KEDA, and releases it on wake", func() {
			Expect(kubectlApply(deploymentManifest("e2e-keda", autoscaleNamespace, 1))).To(Succeed())
			// A cron trigger that's always active, so KEDA keeps the workload
			// up unless it's held.
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: e2e-keda, namespace: %s}
spec:
  scaleTargetRef: {name: e2e-keda}
  minReplicaCount: 0
  maxReplicaCount: 2
  triggers:
    - type: cron
      metadata: {timezone: UTC, start: "0 * * * *", end: "59 * * * *", desiredReplicas: "1"}
`, autoscaleNamespace))).To(Succeed())
			Eventually(func() (string, error) {
				return jsonpath("scaledobject", "e2e-keda", autoscaleNamespace, `{.status.conditions[?(@.type=="Ready")].status}`)
			}, 2*time.Minute, 5*time.Second).Should(Equal("True"))

			By("managing it, and waiting for it to pause")
			manage("e2e-keda")
			Expect(jsonpath("scaledobject", "e2e-keda", autoscaleNamespace,
				`{.metadata.annotations.autoscaling\.keda\.sh/paused-replicas}`)).To(Equal("0"))
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", "e2e-keda", autoscaleNamespace,
					`{.status.conditions[?(@.type=="Autoscaled")].reason}`)
			}, time.Minute, 5*time.Second).Should(Equal("KEDA"))
			Consistently(replicas("e2e-keda"), time.Minute, 10*time.Second).Should(Equal("0"),
				"KEDA holds it, though its trigger is active")

			By("waking it, which releases KEDA")
			wake("e2e-keda")
			Expect(jsonpath("scaledobject", "e2e-keda", autoscaleNamespace,
				`{.metadata.annotations.autoscaling\.keda\.sh/paused-replicas}`)).To(BeEmpty())
			Expect(replicas("e2e-keda")()).NotTo(Equal("0"))
		})
	})

	Context("Protected namespaces", Ordered, func() {
		const protectedNamespace = "hybernate-e2e-protected"

		BeforeAll(func() {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", protectedNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", protectedNamespace, "--wait=false"))
			})
			Expect(kubectlApply(deploymentManifest("e2e-guarded", protectedNamespace, 2))).To(Succeed())
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: e2e-guarded, namespace: %s}
spec:
  target: {kind: Deployment, name: e2e-guarded}
  idlePolicy: {idleAfter: 1m}
  prediction: {confidence: 85}
`, protectedNamespace))).To(Succeed())
		})

		It("wakes what it had paused once the namespace is protected, and opts nothing in there", func() {
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", "e2e-guarded", protectedNamespace, "{.status.phase}")
			}, 4*time.Minute, 5*time.Second).Should(Equal("Paused"))

			By("protecting the namespace")
			_, err := utils.Run(exec.Command("kubectl", "label", "namespace", protectedNamespace,
				"hybernate.io/protected=true"))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(jsonpath("managedworkload", "e2e-guarded", protectedNamespace, "{.status.phase}")).
					To(Equal("Running"))
				g.Expect(jsonpath("managedworkload", "e2e-guarded", protectedNamespace,
					`{.status.conditions[?(@.type=="Protected")].status}`)).To(Equal("True"))
				g.Expect(jsonpath("deployment", "e2e-guarded", protectedNamespace, "{.spec.replicas}")).To(Equal("2"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("labelling a new workload there for Hybernate, which must not opt it in")
			Expect(kubectlApply(deploymentManifest("e2e-labelled", protectedNamespace, 1))).To(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "label", "deployment", "e2e-labelled", "-n", protectedNamespace,
				"hybernate.io/managed=true"))
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				_, err := jsonpath("managedworkload", "e2e-labelled", protectedNamespace, "{.metadata.name}")
				return err
			}, 30*time.Second, 5*time.Second).Should(HaveOccurred(), "no ManagedWorkload is created")
		})
	})
})

// deploymentManifest is a Deployment of the pause container, which uses no
// CPU and so never registers as active on its own.
func deploymentManifest(name, namespace string, replicas int) string {
	return fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: %[4]d
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
        - name: app
          image: %[3]s
          imagePullPolicy: IfNotPresent
          resources:
            requests: {cpu: 10m, memory: 16Mi}
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
            seccompProfile: {type: RuntimeDefault}
`, name, namespace, pauseImage, replicas)
}

// webManifest is an HTTP server behind a Service. It answers /hostname with
// the name of the pod that served the request.
func webManifest(name, namespace string) string {
	return fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
        - name: web
          image: %[3]s
          imagePullPolicy: IfNotPresent
          args: [netexec, --http-port=8080]
          ports: [{name: http, containerPort: 8080}]
          readinessProbe:
            httpGet: {path: /healthz, port: http}
            periodSeconds: 2
          resources:
            requests: {cpu: 100m, memory: 16Mi}
          securityContext:
            runAsNonRoot: true
            runAsUser: 1000
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
            seccompProfile: {type: RuntimeDefault}
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector: {app: %[1]s}
  ports:
    - {name: http, port: 80, targetPort: http}
`, name, namespace, webImage)
}

// curlInCluster runs curl in a pod and returns the last line it printed: the
// response, after any retried attempts' errors. curl waits longer than the
// doorman's default maxWait, so a failure is the doorman's.
func curlInCluster(name, namespace, url string, args ...string) string {
	args = append([]string{"-sS", "--fail-with-body", "--max-time", "150"}, args...)
	logs, succeeded := runCurl(name, namespace, url, args...)
	Expect(succeeded).To(BeTrue(), "curl failed: %s", logs)
	lines := utils.GetNonEmptyLines(logs)
	Expect(lines).NotTo(BeEmpty())
	return lines[len(lines)-1]
}

// runCurl runs curl in a pod with args and returns what it printed and
// whether it exited cleanly.
func runCurl(name, namespace, url string, args ...string) (string, bool) {
	run := append([]string{"run", name, "-n", namespace, "--restart=Never",
		"--image=curlimages/curl:8.7.1", "--image-pull-policy=IfNotPresent", "--command", "--", "curl"},
		append(args, url)...)
	_, err := utils.Run(exec.Command("kubectl", run...))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", name, "-n", namespace, "--wait=false"))
	})

	var phase string
	Eventually(func(g Gomega) {
		var err error
		phase, err = jsonpath("pod", name, namespace, "{.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(BeElementOf("Succeeded", "Failed"))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())

	logs, err := utils.Run(exec.Command("kubectl", "logs", name, "-n", namespace))
	Expect(err).NotTo(HaveOccurred())
	return logs, phase == "Succeeded"
}

// kubectlApply applies a manifest from a string.
func kubectlApply(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

var imageDigest = regexp.MustCompile(`@sha256:[0-9a-f]{64}`)

// preloaded is a manifest that uses the images the Makefile preloads into
// kind. Manifests pin images by the digest of their multi-platform index,
// which the single-platform images preloaded don't carry, or pull them
// Always; either way kind would pull them again, and that pull is what
// timed specs out.
func preloaded(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building the request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only, nothing to do if closing fails
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}
	manifest := imageDigest.ReplaceAllString(string(body), "")
	return strings.ReplaceAll(manifest, "imagePullPolicy: Always", "imagePullPolicy: IfNotPresent"), nil
}

// jsonpath reads a single field from a namespaced object.
func jsonpath(resource, name, ns, path string) (string, error) {
	return utils.Run(exec.Command("kubectl", "get", resource, name, "-n", ns, "-o", "jsonpath="+path))
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
