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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/okedeji/hybernate/test/utils"
)

// Each spec has a namespace and an app of its own, so they run in any
// order, in parallel, and one failing leaves the others' state alone.
var _ = Describe("Wake on request", func() {
	const ingressURL = "http://ingress-nginx-controller.ingress-nginx/hostname"

	// request sends a request to a Service from a pod, retried only until
	// kube-proxy routes it: right after the pause the Service has no
	// endpoints and refuses. curl never retries a connection the doorman
	// holds.
	request := func(curlPod, ns, service string) string {
		return curlInCluster(curlPod, ns, fmt.Sprintf("http://%s/hostname", service),
			"--retry", "10", "--retry-delay", "1", "--retry-connrefused")
	}

	It("wakes a paused app on a request to its Service, with every replica it had", func() {
		ns := createNamespace("hybernate-e2e-wake-service")
		const name = "e2e-web"
		startWebApp(name, ns, 3)
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)

		By("sending a request to the paused app's Service")
		Expect(request("curl-wake", ns, name)).To(HavePrefix(name+"-"),
			"the held request is answered by the woken pod")
		expectWokenByRequest(ns, "deployment", name, 3)
	})

	It("wakes a paused app on a request through ingress-nginx", func() {
		ns := createNamespace("hybernate-e2e-wake-ingress")
		const name = "e2e-web"
		host := routeThroughIngress(ns, name)
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)

		By("sending a request through the ingress controller")
		// nginx picks up endpoint changes on a timer, so for a moment after
		// the pause it still sends to the deleted pod and answers 502. curl
		// retries only those errors, never a connection the doorman holds.
		body := curlInCluster("curl-wake-ingress", ns, ingressURL, "-H", "Host: "+host,
			"--retry", "10", "--retry-delay", "1", "--retry-connrefused")
		Expect(body).To(HavePrefix(name+"-"), "ingress-nginx passes the held request to the woken pod")
		expectWokenByRequest(ns, "deployment", name, 1)
	})

	It("shows a browser a waking-up page through ingress-nginx, then the app", func() {
		ns := createNamespace("hybernate-e2e-wake-page")
		const name = "e2e-web"
		host := routeThroughIngress(ns, name)
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)
		browser := []string{"-s", "--max-time", "30", "-w", "\nstatus=%{http_code}", "-H", "Host: " + host,
			"-H", "Accept: text/html", "-H", "Sec-Fetch-Mode: navigate"}

		By("opening the paused app in a browser")
		// Until nginx picks up the doorman's endpoints, a request can still
		// reach the removed pod, so this looks for the page across a few tries.
		attempt := 0
		Eventually(func(g Gomega) {
			attempt++
			page, _ := runCurl(fmt.Sprintf("curl-page-%d", attempt), ns, ingressURL, browser...)
			g.Expect(page).To(ContainSubstring("Waking up " + name))
			g.Expect(page).To(ContainSubstring("status=503"))
		}, 2*time.Minute, time.Second).Should(Succeed())

		expectWokenByRequest(ns, "deployment", name, 1)

		By("reloading once the app is Running")
		reload := 0
		Eventually(func(g Gomega) {
			reload++
			app, _ := runCurl(fmt.Sprintf("curl-page-reload-%d", reload), ns, ingressURL, browser...)
			g.Expect(app).To(ContainSubstring("status=200"))
			g.Expect(app).To(HavePrefix(name+"-"), "the reload reaches the app")
		}, time.Minute, time.Second).Should(Succeed())
	})

	It("learns that a workload Hybernate manages depends on the one its request woke", func() {
		ns := createNamespace("hybernate-e2e-wake-learn")
		const (
			name       = "e2e-web"
			callerName = "e2e-caller"
		)
		By("running a caller that Hybernate manages, with nothing in its environment naming the app")
		startWebApp(name, ns, 1)
		startWebApp(callerName, ns, 1)
		manage(ns, "Deployment", callerName, "1h")
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)

		By("connecting from the caller to the paused app")
		// Retried until kube-proxy routes the Service to the doorman; a
		// connection the doorman holds isn't refused.
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "exec", "-n", ns, "deploy/"+callerName, "--",
				"/agnhost", "connect", "--timeout", "5s", name+":80"))
			return err
		}, time.Minute, 2*time.Second).Should(Succeed())

		expectWokenByRequest(ns, "deployment", name, 1)

		By("checking the caller now depends on the app, learned from the wake")
		Eventually(func(g Gomega) {
			g.Expect(jsonpath("managedworkload", callerName, ns,
				"{.status.learnedDependencies.dependencies[*].name}")).To(Equal(name))
			g.Expect(jsonpath("managedworkload", callerName, ns,
				"{.status.learnedDependencies.dependencies[*].source}")).To(Equal("wake"))
		}, time.Minute, 5*time.Second).Should(Succeed())
		out, err := utils.Run(exec.Command(pluginBinary, "deps", name, "-n", ns))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(MatchRegexp(ns + `/` + callerName + `\s+learned from a wake\s+Running`))
	})

	It("answers a Prometheus scrape of a paused app without waking it, and wakes it for a client", func() {
		ns := createNamespace("hybernate-e2e-wake-scrape")
		const name = "e2e-scraped"
		startWebApp(name, ns, 1)
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)

		By("scraping the paused app as a ServiceMonitor's Prometheus does")
		// A refused connection, before kube-proxy routes the Service, is
		// status 000; the doorman's answer to a scraper is 503.
		attempt := 0
		Eventually(func(g Gomega) {
			attempt++
			out, _ := runCurl(fmt.Sprintf("curl-scrape-%d", attempt), ns, fmt.Sprintf("http://%s/metrics", name),
				"-s", "--max-time", "30", "-A", "Prometheus/2.53.0", "-o", "/dev/null", "-w", "status=%{http_code}")
			g.Expect(out).To(ContainSubstring("status=503"))
		}, 2*time.Minute, time.Second).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(jsonpath("managedworkload", name, ns, "{.status.phase}")).To(Equal("Paused"))
			g.Expect(jsonpath("deployment", name, ns, "{.spec.replicas}")).To(Equal("0"))
		}, 30*time.Second, 5*time.Second).Should(Succeed(), "a scrape must not wake the app")

		By("sending the same request as a client")
		Expect(request("curl-client", ns, name)).To(HavePrefix(name + "-"))
		expectWokenByRequest(ns, "deployment", name, 1)
	})

	It("wakes a paused app on a request to its NodePort", func() {
		ns := createNamespace("hybernate-e2e-wake-nodeport")
		const name = "e2e-web"
		startWebApp(name, ns, 2)
		_, err := utils.Run(exec.Command("kubectl", "patch", "service", name, "-n", ns,
			"--type=merge", "-p", `{"spec":{"type":"NodePort"}}`))
		Expect(err).NotTo(HaveOccurred())
		nodePort, err := jsonpath("service", name, ns, "{.spec.ports[0].nodePort}")
		Expect(err).NotTo(HaveOccurred())
		nodeIP, err := utils.Output(exec.Command("kubectl", "get", "nodes",
			"-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(nodeIP).NotTo(BeEmpty())
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)

		By("sending a request to the node's port")
		body := curlInCluster("curl-nodeport", ns, fmt.Sprintf("http://%s:%s/hostname", nodeIP, nodePort),
			"--retry", "10", "--retry-delay", "1", "--retry-connrefused")
		Expect(body).To(HavePrefix(name + "-"))
		expectWokenByRequest(ns, "deployment", name, 2)
	})

	It("pauses a StatefulSet and wakes it on a request, with every replica it had", func() {
		ns := createNamespace("hybernate-e2e-wake-statefulset")
		const name = "e2e-stateful"
		Expect(kubectlApply(fmt.Sprintf(`
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  serviceName: %[1]s-headless
  replicas: 2
  selector: {matchLabels: {app: %[1]s}}
  template:
    metadata: {labels: {app: %[1]s}}
    spec:
      containers:
%[3]s
---
apiVersion: v1
kind: Service
metadata: {name: %[1]s-headless, namespace: %[2]s}
spec:
  clusterIP: None
  selector: {app: %[1]s}
  ports: [{name: http, port: 80, targetPort: http}]
---
apiVersion: v1
kind: Service
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  selector: {app: %[1]s}
  ports: [{name: http, port: 80, targetPort: http}]
`, name, ns, webContainer))).To(Succeed())
		rollout(ns, "statefulset/"+name)
		manage(ns, "StatefulSet", name, "1m")
		waitForDoorman(ns, name)
		Expect(jsonpath("statefulset", name, ns, "{.spec.replicas}")).To(Equal("0"))

		By("sending a request to the StatefulSet's Service")
		Expect(request("curl-wake", ns, name)).To(HavePrefix(name + "-"))
		expectWokenByRequest(ns, "statefulset", name, 2)
	})

	It("restores a paused workload and stops routing it when its ManagedWorkload is deleted", func() {
		ns := createNamespace("hybernate-e2e-wake-delete")
		const name = "e2e-web"
		startWebApp(name, ns, 2)
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)

		By("deleting the ManagedWorkload while the app is paused")
		_, err := utils.Run(exec.Command("kubectl", "delete", "managedworkload", name, "-n", ns, "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred())

		Expect(jsonpath("deployment", name, ns, "{.spec.replicas}")).To(Equal("2"),
			"the finalizer restores the replicas before it lets the ManagedWorkload go")
		Eventually(func() (string, error) {
			return doormanSlices(ns, name)
		}, 30*time.Second, time.Second).Should(BeEmpty(), "its Services no longer route to the doorman")
		rollout(ns, "deployment/"+name)

		By("checking the Service reaches the app itself")
		Expect(request("curl-after-delete", ns, name)).To(HavePrefix(name + "-"))
	})

	It("wakes one of two paused workloads behind one Service, and leaves the other to it", func() {
		ns := createNamespace("hybernate-e2e-wake-canary")
		const service = "e2e-shop"
		tracks := []string{"e2e-stable", "e2e-canary"}
		for _, track := range tracks {
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: %[3]s, track: %[1]s}}
  template:
    metadata: {labels: {app: %[3]s, track: %[1]s}}
    spec:
      containers:
%[4]s
`, track, ns, service, webContainer))).To(Succeed())
		}
		Expect(kubectlApply(fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  selector: {app: %[1]s}
  ports: [{name: http, port: 80, targetPort: http}]
`, service, ns))).To(Succeed())
		for _, track := range tracks {
			rollout(ns, "deployment/"+track)
			manage(ns, "Deployment", track, "1m")
		}

		By("waiting for both to pause, and the Service to be routed by each")
		for _, track := range tracks {
			waitForDoorman(ns, track)
		}

		By("sending a request to the shared Service")
		body := request("curl-shop", ns, service)
		woken, asleep := tracks[0], tracks[1]
		if strings.HasPrefix(body, tracks[1]+"-") {
			woken, asleep = tracks[1], tracks[0]
		}
		Expect(body).To(HavePrefix(woken+"-"), "the request is answered by one of the two")
		expectWokenByRequest(ns, "deployment", woken, 1)
		Eventually(func() (string, error) {
			return doormanSlices(ns, asleep)
		}, time.Minute, 2*time.Second).Should(BeEmpty(), "the woken workload serves the Service for both")

		By("sending more requests, which the woken workload answers without waking the other")
		for i := range 5 {
			Expect(request(fmt.Sprintf("curl-shop-%d", i), ns, service)).To(HavePrefix(woken + "-"))
		}
		Consistently(func(g Gomega) {
			g.Expect(jsonpath("managedworkload", asleep, ns, "{.status.phase}")).To(Equal("Paused"))
			g.Expect(jsonpath("deployment", asleep, ns, "{.spec.replicas}")).To(Equal("0"))
		}, 30*time.Second, 5*time.Second).Should(Succeed())

		Expect(loggedErrors(ns)).To(BeEmpty(), "neither the operator nor the doorman logs an error for the shop")
	})
})

// The operator and the doorman are restarted here, so this runs on its own,
// after every other spec.
var _ = Describe("Operator restart", Serial, func() {
	It("keeps a paused workload paused across a restart of the operator and the doorman, and still wakes it", func() {
		ns := createNamespace("hybernate-e2e-restart")
		const name = "e2e-web"
		startWebApp(name, ns, 3)
		manage(ns, "Deployment", name, "1m")
		waitForDoorman(ns, name)
		pausedAt, err := jsonpath("managedworkload", name, ns, "{.status.pause.pausedAt}")
		Expect(err).NotTo(HaveOccurred())
		Expect(pausedAt).NotTo(BeEmpty())
		Expect(jsonpath("managedworkload", name, ns, "{.status.pause.previousReplicas}")).To(Equal("3"))

		By("deleting the operator's and the doorman's pods")
		for _, d := range []struct{ deployment, label string }{
			{"hybernate-controller-manager", "control-plane=controller-manager"},
			{"hybernate-doorman", "control-plane=doorman"},
		} {
			_, err := utils.Run(exec.Command("kubectl", "delete", "pods", "-n", namespace, "-l", d.label,
				"--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
			// The old pods are gone, so every pod the selector matches now is
			// a new one; the ReplicaSet may not have created them yet.
			Eventually(func() error {
				_, err := utils.Run(exec.Command("kubectl", "wait", "pods", "-n", namespace, "-l", d.label,
					"--for=condition=Ready", "--timeout=10s"))
				return err
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
			rollout(namespace, "deployment/"+d.deployment)
		}

		By("checking the workload stays paused, with its pause record intact")
		Consistently(func(g Gomega) {
			g.Expect(jsonpath("managedworkload", name, ns, "{.status.phase}")).To(Equal("Paused"))
			g.Expect(jsonpath("deployment", name, ns, "{.spec.replicas}")).To(Equal("0"))
			g.Expect(jsonpath("managedworkload", name, ns, "{.status.pause.previousReplicas}")).To(Equal("3"))
			g.Expect(jsonpath("managedworkload", name, ns, "{.status.pause.pausedAt}")).To(Equal(pausedAt))
		}, 45*time.Second, 5*time.Second).Should(Succeed())

		By("checking its Service points at the new doorman pods")
		Eventually(func(g Gomega) {
			pods, err := utils.Output(exec.Command("kubectl", "get", "pods", "-n", namespace,
				"-l", "control-plane=doorman", "--field-selector=status.phase=Running",
				"-o", "jsonpath={.items[*].status.podIP}"))
			g.Expect(err).NotTo(HaveOccurred())
			routed, err := doormanAddresses(ns, name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Fields(routed)).NotTo(BeEmpty())
			g.Expect(strings.Fields(pods)).To(ContainElements(strings.Fields(routed)))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("sending a request to the paused app's Service")
		body := curlInCluster("curl-wake", ns, fmt.Sprintf("http://%s/hostname", name),
			"--retry", "10", "--retry-delay", "1", "--retry-connrefused")
		Expect(body).To(HavePrefix(name + "-"))
		expectWokenByRequest(ns, "deployment", name, 3)
	})
})

// webContainer is an HTTP server that answers /hostname with the name of
// the pod that served the request, indented to sit under a pod template's
// containers. Its CPU request is large enough that serving nothing stays
// under the idle clock's threshold.
var webContainer = fmt.Sprintf(`        - name: web
          image: %s
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
            seccompProfile: {type: RuntimeDefault}`, webImage)

// startWebApp runs webManifest and waits for all of its replicas.
func startWebApp(name, ns string, replicas int) {
	Expect(kubectlApply(webManifest(name, ns, replicas))).To(Succeed())
	rollout(ns, "deployment/"+name)
}

// routeThroughIngress starts a web app behind an Ingress of its own host,
// and returns the host once nginx serves the app on it.
func routeThroughIngress(ns, name string) string {
	startWebApp(name, ns, 1)
	host := ns + ".example.com"
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
`, name, ns, host))
	}, 2*time.Minute, 5*time.Second).Should(Succeed())

	By("checking the route serves the app while it's awake")
	// nginx loads a new Ingress a few seconds after it's created, so this
	// retries; the spec then fails only on the doorman.
	body := curlInCluster("curl-ingress-ready", ns, "http://ingress-nginx-controller.ingress-nginx/hostname",
		"-H", "Host: "+host, "--retry", "30", "--retry-delay", "2", "--retry-all-errors")
	Expect(body).To(HavePrefix(name + "-"))
	return host
}

// waitForDoorman waits for a workload to pause on its idle clock and its
// Services to point at the doorman.
func waitForDoorman(ns, name string) {
	By("waiting for " + name + " to pause and its Service to point at the doorman")
	Eventually(func(g Gomega) {
		g.Expect(jsonpath("managedworkload", name, ns, "{.status.phase}")).To(Equal("Paused"))
		g.Expect(jsonpath("managedworkload", name, ns,
			`{.status.conditions[?(@.type=="WakeOnRequest")].status}`)).To(Equal("True"))
		g.Expect(doormanAddresses(ns, name)).NotTo(BeEmpty())
	}, 4*time.Minute, 5*time.Second).Should(Succeed())
}

// expectWokenByRequest checks that a request woke the workload: it's
// Running with every replica it had before the pause Ready, and its
// Services no longer point at the doorman.
func expectWokenByRequest(ns, resource, name string, replicas int) {
	By("checking " + name + " is Running with " + strconv.Itoa(replicas) + " replicas, and off the doorman")
	Eventually(func(g Gomega) {
		g.Expect(jsonpath("managedworkload", name, ns, "{.status.phase}")).To(Equal("Running"))
		g.Expect(jsonpath("managedworkload", name, ns,
			"{.status.activity.lastActivitySource}")).To(Equal("request"))
		g.Expect(jsonpath(resource, name, ns, "{.spec.replicas}")).To(Equal(strconv.Itoa(replicas)))
		g.Expect(jsonpath(resource, name, ns, "{.status.readyReplicas}")).To(Equal(strconv.Itoa(replicas)))
		g.Expect(doormanSlices(ns, name)).To(BeEmpty(), "the doorman's EndpointSlices must be removed")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}

// doormanAddresses are the doorman pod addresses in the EndpointSlices the
// operator wrote for a ManagedWorkload's Services, space-separated.
func doormanAddresses(ns, workload string) (string, error) {
	return doormanSliceField(ns, workload, "{.items[*].endpoints[*].addresses[0]}")
}

// doormanSlices are the names of the EndpointSlices the operator wrote for
// a ManagedWorkload's Services, space-separated.
func doormanSlices(ns, workload string) (string, error) {
	return doormanSliceField(ns, workload, "{.items[*].metadata.name}")
}

func doormanSliceField(ns, workload, path string) (string, error) {
	return utils.Output(exec.Command("kubectl", "get", "endpointslices", "-n", ns,
		"-l", "endpointslice.kubernetes.io/managed-by=doorman.hybernate.io,hybernate.io/managed-workload="+workload,
		"-o", "jsonpath="+path))
}

// loggedErrors are the error lines the operator and the doorman logged
// about a namespace. A conflict is an optimistic write that lost a race,
// which controller-runtime retries at once, so it isn't one.
func loggedErrors(ns string) []string {
	var errs []string
	for _, label := range []string{"control-plane=controller-manager", "control-plane=doorman"} {
		logs, err := utils.Output(exec.Command("kubectl", "logs", "-n", namespace, "-l", label,
			"--prefix", "--tail=-1"))
		Expect(err).NotTo(HaveOccurred())
		for line := range strings.SplitSeq(logs, "\n") {
			if strings.Contains(line, `"level":"error"`) && strings.Contains(line, ns) &&
				!strings.Contains(line, "the object has been modified") {
				errs = append(errs, line)
			}
		}
	}
	return errs
}
