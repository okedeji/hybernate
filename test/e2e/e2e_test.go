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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// After a spec fails, collect what it takes to tell why: Hybernate's logs,
// events and pods, and the ManagedWorkloads and EndpointSlices of the
// namespaces the specs made.
var _ = AfterEach(func() {
	if !CurrentSpecReport().Failed() {
		return
	}
	for _, d := range []struct {
		title string
		args  []string
	}{
		{"Controller logs", []string{"logs", "-l", "control-plane=controller-manager", "-n", namespace,
			"--prefix", "--tail=500"}},
		{"Previous controller logs", []string{"logs", "-l", "control-plane=controller-manager", "-n", namespace,
			"--prefix", "--previous", "--tail=100"}},
		{"Doorman logs", []string{"logs", "-l", "control-plane=doorman", "-n", namespace, "--prefix", "--tail=200"}},
		{"Hybernate's events", []string{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"}},
		{"Hybernate's pods", []string{"describe", "pods", "-n", namespace}},
		{"ManagedWorkloads", []string{"get", "managedworkloads", "-A", "-o", "wide"}},
		{"Doorman EndpointSlices", []string{"get", "endpointslices", "-A",
			"-l", "endpointslice.kubernetes.io/managed-by=doorman.hybernate.io"}},
		{"Events in the specs' namespaces", []string{"get", "events", "-A", "--sort-by=.lastTimestamp",
			"--field-selector=involvedObject.namespace!=kube-system"}},
		{"curl-metrics logs", []string{"logs", "curl-metrics", "-n", namespace}},
	} {
		out, err := utils.Run(exec.Command("kubectl", d.args...))
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get %s: %s\n", d.title, err)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "%s:\n%s\n", d.title, out)
	}
})

var _ = Describe("Manager", func() {
	var controllerPodName string

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

			By("managing a workload, so the operator has a series for it")
			const metered, meteredNamespace = "e2e-metered", "hybernate-e2e-metrics"
			createNamespace(meteredNamespace)
			Expect(kubectlApply(deploymentManifest(metered, meteredNamespace, 1))).To(Succeed())
			manage(meteredNamespace, "Deployment", metered, "1h")
			Eventually(func() (string, error) {
				return jsonpath("managedworkload", metered, meteredNamespace, "{.status.phase}")
			}).Should(Equal("Running"))

			By("scraping the controller-manager's metrics")
			operatorMetrics := scrape("curl-metrics",
				fmt.Sprintf("https://%s.%s.svc.cluster.local:8443/metrics", metricsServiceName, namespace), token)
			Expect(operatorMetrics).To(MatchRegexp(
				`(?m)^hybernate_workload_phase\{namespace="%s",phase="Running",workload="%s"\} 1$`,
				meteredNamespace, metered))

			By("scraping the doorman's metrics")
			doormanMetrics := scrape("curl-doorman-metrics",
				fmt.Sprintf("https://hybernate-doorman.%s.svc.cluster.local:8443/metrics", namespace), token)
			Expect(doormanMetrics).To(MatchRegexp(`(?m)^hybernate_doorman_held_connections \d+$`))
			Expect(doormanMetrics).To(MatchRegexp(`(?m)^hybernate_doorman_proxied_connections \d+$`))
			Expect(doormanMetrics).To(MatchRegexp(`(?m)^hybernate_doorman_port_conflicts \d+$`))
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
				out, err := utils.Output(exec.Command(pluginBinary, "scan", "-n", scanNamespace, "-o", "json", "--window", "0"))
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
			out, err := utils.Output(exec.Command(pluginBinary, "scan", "-n", scanNamespace, "-o", "json", "--window", "0",
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
			out, err := utils.Output(exec.Command(pluginBinary, "scan", "-n", historyNamespace, "-o", "json"))
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
				out, err := utils.Output(exec.Command("env", "KUBECONFIG="+kubeconfig,
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
			out, err := utils.Output(exec.Command(pluginBinary, "scan", "-n", depsScanNamespace, "--window", "0", "-o", "json"))
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

	Context("Namespace opt-in", func() {
		It("opts in a labelled namespace's workloads, each annotation over the namespace's, an invalid one under it", func() {
			ns := createNamespace("hybernate-e2e-ns-optin")
			const (
				overriding = "e2e-overriding"
				unsure     = "e2e-unsure"
			)
			By("setting the namespace's annotations, and the workloads' own")
			_, err := utils.Run(exec.Command("kubectl", "annotate", "namespace", ns,
				"hybernate.io/idle-after=45m", "hybernate.io/cpu-threshold=20",
				"hybernate.io/wake-max-wait=90s", "hybernate.io/dry-run=false"))
			Expect(err).NotTo(HaveOccurred())
			for _, name := range []string{overriding, unsure} {
				Expect(kubectlApply(deploymentManifest(name, ns, 1))).To(Succeed())
			}
			_, err = utils.Run(exec.Command("kubectl", "annotate", "deployment", overriding, "-n", ns,
				"hybernate.io/idle-after=2h", "hybernate.io/cpu-threshold=abc"))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "annotate", "deployment", unsure, "-n", ns,
				"hybernate.io/dry-run=yes"))
			Expect(err).NotTo(HaveOccurred())

			By("labelling the namespace, which opts in the workloads already in it")
			_, err = utils.Run(exec.Command("kubectl", "label", "namespace", ns, "hybernate.io/managed=true"))
			Expect(err).NotTo(HaveOccurred())

			spec := func(name, path string) func() (string, error) {
				return func() (string, error) { return jsonpath("managedworkload", name, ns, path) }
			}
			Eventually(func(g Gomega) {
				g.Expect(spec(overriding, `{.metadata.labels.hybernate\.io/from-label}`)()).To(Equal("true"))
				g.Expect(spec(overriding, "{.spec.idlePolicy.idleAfter}")()).To(Equal("2h0m0s"), "the workload's own")
				g.Expect(spec(overriding, "{.spec.idlePolicy.activity.cpuThreshold}")()).To(Equal("20"),
					"the namespace's, under the workload's that can't be read")
				g.Expect(spec(overriding, "{.spec.wake.maxWait}")()).To(Equal("1m30s"), "the namespace's")
				g.Expect(spec(overriding, "{.spec.dryRun}")()).To(BeEmpty(), "the namespace's false")
			}).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(spec(unsure, "{.spec.dryRun}")()).To(Equal("true"),
					"a dry-run setting that can't be read turns dry-run on")
				g.Expect(spec(unsure, "{.spec.idlePolicy.idleAfter}")()).To(Equal("45m0s"), "the namespace's")
				g.Expect(spec(unsure, "{.spec.idlePolicy.activity.cpuThreshold}")()).To(Equal("20"), "the namespace's")
			}).Should(Succeed())

			By("checking each workload is told which of its settings were skipped")
			Eventually(func(g Gomega) {
				out, err := utils.Output(exec.Command("kubectl", "get", "events.events.k8s.io", "-n", ns,
					"-o", `jsonpath={range .items[?(@.reason=="InvalidSetting")]}{.regarding.name}: {.note}{"\n"}{end}`))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring(overriding + `: hybernate.io/cpu-threshold="abc" on the workload`))
				g.Expect(out).To(ContainSubstring("so the namespace's value is used"))
				g.Expect(out).To(ContainSubstring(unsure + `: hybernate.io/dry-run="yes" on the workload`))
				g.Expect(out).To(ContainSubstring("so dry-run is on, to be safe"))
			}).Should(Succeed())
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
			out, err := utils.Output(exec.Command(pluginBinary, "scan", "-n", gitOpsNamespace, "-o", "json", "--window", "0"))
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
		manageUntilPaused := func(name string) {
			manage(autoscaleNamespace, "Deployment", name, "1m")
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
			createNamespace(autoscaleNamespace)
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
			manageUntilPaused("e2e-hpa")
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
			// Cron triggers that are always active between them, so KEDA keeps
			// the workload up unless it's held. A single cron window can't
			// cover the whole hour: its end is outside it.
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
      metadata: {timezone: UTC, start: "0 * * * *", end: "45 * * * *", desiredReplicas: "1"}
    - type: cron
      metadata: {timezone: UTC, start: "30 * * * *", end: "15 * * * *", desiredReplicas: "1"}
`, autoscaleNamespace))).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(jsonpath("scaledobject", "e2e-keda", autoscaleNamespace,
					`{.status.conditions[?(@.type=="Ready")].status}`)).To(Equal("True"))
				g.Expect(jsonpath("scaledobject", "e2e-keda", autoscaleNamespace,
					`{.status.conditions[?(@.type=="Active")].status}`)).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("managing it, and waiting for it to pause")
			manageUntilPaused("e2e-keda")
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

// webManifest is webContainer's HTTP server behind a Service of the same
// name.
func webManifest(name, namespace string, replicas int) string {
	return fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  replicas: %[3]d
  selector: {matchLabels: {app: %[1]s}}
  template:
    metadata: {labels: {app: %[1]s}}
    spec:
      containers:
%[4]s
---
apiVersion: v1
kind: Service
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  selector: {app: %[1]s}
  ports:
    - {name: http, port: 80, targetPort: http}
`, name, namespace, replicas, webContainer)
}

// createNamespace creates a namespace that's deleted when the spec, or the
// container it's created in, ends.
func createNamespace(name string) string {
	_, err := utils.Run(exec.Command("kubectl", "create", "ns", name))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", name, "--wait=false"))
	})
	return name
}

// manage writes a ManagedWorkload for a workload, with an idle clock.
func manage(ns, kind, name, idleAfter string) {
	Expect(kubectlApply(fmt.Sprintf(`
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  target: {kind: %[3]s, name: %[1]s}
  idlePolicy: {idleAfter: %[4]s}
  prediction: {confidence: 85}
`, name, ns, kind, idleAfter))).To(Succeed())
}

// rollout waits for a workload's rollout to finish.
func rollout(ns, workload string) {
	_, err := utils.Run(exec.Command("kubectl", "rollout", "status", workload, "-n", ns, "--timeout=3m"))
	Expect(err).NotTo(HaveOccurred())
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
	return finishedPodLogs(name, namespace)
}

// finishedPodLogs waits for a pod that runs to completion, and returns its
// logs and whether it succeeded.
func finishedPodLogs(name, namespace string) (string, bool) {
	var phase string
	Eventually(func(g Gomega) {
		var err error
		phase, err = jsonpath("pod", name, namespace, "{.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(BeElementOf("Succeeded", "Failed"))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())

	logs, err := utils.Output(exec.Command("kubectl", "logs", name, "-n", namespace))
	Expect(err).NotTo(HaveOccurred())
	return logs, phase == "Succeeded"
}

// scrape fetches one of Hybernate's metrics endpoints from a pod in its
// namespace, with a bearer token, and returns the metrics. The metrics
// servers' certificates are self-signed.
func scrape(pod, url, token string) string {
	_, err := utils.Run(exec.Command("kubectl", "run", pod, "--restart=Never",
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
						"for i in $(seq 1 30); do curl -sS -k --fail -H 'Authorization: Bearer %s' %s && exit 0 || sleep 2; done; exit 1"
					],
					"securityContext": {
						"readOnlyRootFilesystem": true,
						"allowPrivilegeEscalation": false,
						"capabilities": {"drop": ["ALL"]},
						"runAsNonRoot": true,
						"runAsUser": 1000,
						"seccompProfile": {"type": "RuntimeDefault"}
					}
				}],
				"serviceAccountName": "%s"
			}
		}`, token, url, serviceAccountName)))
	Expect(err).NotTo(HaveOccurred(), "Failed to create the %s pod", pod)
	metrics, succeeded := finishedPodLogs(pod, namespace)
	Expect(succeeded).To(BeTrue(), "scraping %s failed: %s", url, metrics)
	return metrics
}

// kubectlApply applies a manifest from a string.
func kubectlApply(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

// jsonpath reads a single field from a namespaced object.
func jsonpath(resource, name, ns, path string) (string, error) {
	return utils.Output(exec.Command("kubectl", "get", resource, name, "-n", ns, "-o", "jsonpath="+path))
}

// serviceAccountToken returns a token for the controller-manager's service
// account.
func serviceAccountToken() (string, error) {
	token, err := utils.Output(exec.Command("kubectl", "create", "token", serviceAccountName, "-n", namespace))
	return strings.TrimSpace(token), err
}
