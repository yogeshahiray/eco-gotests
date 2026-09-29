package ran_du_system_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	goversion "github.com/hashicorp/go-version"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/namespace"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/nodes"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/ocm"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/sriov"
	sriovfec "github.com/rh-ecosystem-edge/eco-goinfra/pkg/sriov-fec"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/internal/certmanager"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/internal/platform"
	sysptp "github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/internal/ptp"
	systemtestsparams "github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/internal/systemtestsparams"
	. "github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/ran-du/internal/randuinittools"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/ran-du/internal/randuparams"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// prereqMinOCPVersion is the oldest OCP release the RAN DU suites are supported on.
	prereqMinOCPVersion = "4.13"
	// prereqWorkerNodeLabel selects the nodes the hard reboot suite power cycles.
	prereqWorkerNodeLabel = "node-role.kubernetes.io/worker"
	// prereqAPIServerDeployment is the deployment the reboot and kdump suites wait on after a node returns.
	prereqAPIServerDeployment = "apiserver"
	// prereqAPIServerNamespace holds the deployment above.
	prereqAPIServerNamespace = "openshift-apiserver"
	// prereqDefaultStorageClassAnnotation marks the StorageClass bound by claims that name no class.
	prereqDefaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"
	// prereqKdumpDefaultPath is the vmcore target the kdump suites read after a crash.
	prereqKdumpDefaultPath = "/var/crash"
	// prereqDaemonLogTailLines is the number of linuxptp daemon log lines the interface checks inspect.
	prereqDaemonLogTailLines = 500
	// prereqPTPLogWindow is the log window ValidatePTPStatus scans for PTP errors.
	prereqPTPLogWindow = 5 * time.Minute
	// prereqExecutableBits masks the owner, group and other execute permissions of a file.
	prereqExecutableBits = 0o111
)

// This suite is a preflight check for everything documented in prerequisites.md. It reads cluster,
// node and test runner state and never changes it, so it is safe to run before any other suite.
//
// A missing prerequisite is reported the way the suite that needs it behaves: prerequisites whose
// absence makes a suite skip cleanly (PTP disabled, empty BMC map, no cert-manager issuer, a
// boundary clock with a holdover window) are reported as skips, and everything else as a failure.
// Suites are independent, so the whole thing runs with ContinueOnFailure: one run reports every
// prerequisite that is not met rather than stopping at the first one.
var _ = Describe(
	"Prerequisites",
	Ordered,
	ContinueOnFailure,
	Label(randuparams.LabelPrerequisites),
	func() {
		var (
			clusterNodes []*nodes.Builder
			workerNodes  []*nodes.Builder
		)

		// ptpDaemonNodes returns the nodes running a linuxptp daemon pod.
		ptpDaemonNodes := func() []string {
			ptpPods, err := pod.List(APIClient, sysptp.Namespace, metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(labels.Set{
					sysptp.DaemonPodLabelKey: sysptp.DaemonPodLabelValueLinuxpt,
				}).String(),
			})
			Expect(err).ToNot(HaveOccurred(), "Failed to list PTP daemon pods in %s", sysptp.Namespace)
			Expect(ptpPods).ToNot(BeEmpty(), "No linuxptp daemon pods found in %s", sysptp.Namespace)

			var ptpNodes []string

			for _, daemonPod := range ptpPods {
				if daemonPod.Object.Spec.NodeName != "" {
					ptpNodes = append(ptpNodes, daemonPod.Object.Spec.NodeName)
				}
			}

			return ptpNodes
		}

		// skipUnlessPTPEnabled skips the caller on clusters the PTP suites do not apply to.
		skipUnlessPTPEnabled := func() {
			if !RanDuTestConfig.PtpEnabled {
				Skip("PTP is disabled (ECO_RANDU_PTP_ENABLED=false), the PTP suites skip on this cluster")
			}

			if !namespace.NewBuilder(APIClient, sysptp.Namespace).Exists() {
				Skip(fmt.Sprintf("Namespace %s does not exist, the PTP operator is not installed", sysptp.Namespace))
			}
		}

		BeforeAll(func() {
			By("Listing the cluster nodes")

			var err error

			clusterNodes, err = nodes.List(APIClient, metav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred(), "Failed to list the cluster nodes")
			Expect(clusterNodes).ToNot(BeEmpty(), "No nodes found in the cluster")

			workerNodes, err = nodes.List(APIClient, metav1.ListOptions{LabelSelector: prereqWorkerNodeLabel})
			Expect(err).ToNot(HaveOccurred(), "Failed to list the worker nodes")

			klog.V(randuparams.RanDuLogLevel).Infof("Cluster has %d nodes, %d of them labelled %s",
				len(clusterNodes), len(workerNodes), prereqWorkerNodeLabel)
		})

		It("Baseline: cluster runs a supported OCP version", func() {
			ocpVersion, err := platform.GetOCPVersion(APIClient)
			Expect(err).ToNot(HaveOccurred(), "Failed to read the cluster OCP version")

			currentVersion, err := goversion.NewVersion(ocpVersion)
			Expect(err).ToNot(HaveOccurred(), "Failed to parse the cluster OCP version %q", ocpVersion)

			minimumVersion, err := goversion.NewVersion(prereqMinOCPVersion)
			Expect(err).ToNot(HaveOccurred(), "Failed to parse the minimum OCP version %q", prereqMinOCPVersion)

			klog.V(randuparams.RanDuLogLevel).Infof("Cluster runs OCP %s", ocpVersion)

			Expect(currentVersion.GreaterThanOrEqual(minimumVersion)).To(BeTrue(),
				"Cluster runs OCP %s, the RAN DU suites require %s or newer", ocpVersion, prereqMinOCPVersion)
		})

		It("Baseline: test runner credentials have cluster-admin rights", func() {
			By("Submitting a SelfSubjectAccessReview for every verb on every resource")

			review := &authv1.SelfSubjectAccessReview{
				Spec: authv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authv1.ResourceAttributes{
						Verb:     "*",
						Group:    "*",
						Resource: "*",
					},
				},
			}

			result, err := APIClient.K8sClient.AuthorizationV1().SelfSubjectAccessReviews().Create(
				context.TODO(), review, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred(), "Failed to submit a SelfSubjectAccessReview")

			Expect(result.Status.Allowed).To(BeTrue(),
				"The KUBECONFIG user is not cluster-admin: %s. The suites create namespaces, patch "+
					"cluster scoped resources and run privileged pods", result.Status.Reason)
		})

		It("Baseline: every cluster node is Ready", func() {
			var notReady []string

			for _, node := range clusterNodes {
				ready, err := node.IsReady()
				if err != nil || !ready {
					notReady = append(notReady, node.Definition.Name)
				}
			}

			Expect(notReady).To(BeEmpty(), "Nodes are not Ready: %v", notReady)
		})

		It("Baseline: node commands can be executed on every node", func() {
			By("Verifying a machine-config-daemon pod runs on every node")

			var missingDaemon []string

			for _, node := range clusterNodes {
				nodeName := node.Definition.Name

				mcdPods, err := pod.List(APIClient, RanDuTestConfig.MCONamespace, metav1.ListOptions{
					FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}).String(),
					LabelSelector: labels.SelectorFromSet(labels.Set{
						"k8s-app": RanDuTestConfig.MCOConfigDaemonName,
					}).String(),
				})
				Expect(err).ToNot(HaveOccurred(), "Failed to list %s pods on node %s",
					RanDuTestConfig.MCOConfigDaemonName, nodeName)

				running := false

				for _, mcdPod := range mcdPods {
					if mcdPod.Object.Status.Phase == corev1.PodRunning {
						running = true
					}
				}

				if !running {
					missingDaemon = append(missingDaemon, nodeName)
				}
			}

			Expect(missingDaemon).To(BeEmpty(),
				"No running %s pod in %s on nodes %v, node level commands cannot be executed there",
				RanDuTestConfig.MCOConfigDaemonName, RanDuTestConfig.MCONamespace, missingDaemon)

			By("Running a command in the host namespace of every node")

			for _, node := range clusterNodes {
				nodeName := node.Definition.Name

				output, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, "echo prereq-ok")
				Expect(err).ToNot(HaveOccurred(), "Failed to run a host command on node %s", nodeName)
				Expect(output).To(ContainSubstring("prereq-ok"),
					"Unexpected host command output on node %s: %q", nodeName, output)
			}
		})

		It("Baseline: privileged test namespace is usable", func() {
			testNamespace, err := namespace.Pull(APIClient, randuparams.TestNamespaceName)
			Expect(err).ToNot(HaveOccurred(), "Failed to pull namespace %s, BeforeSuite creates it",
				randuparams.TestNamespaceName)

			for key, value := range systemtestsparams.PrivilegedNSLabels {
				Expect(testNamespace.Object.Labels).To(HaveKeyWithValue(key, value),
					"Namespace %s is missing the privileged pod security label %s=%s",
					randuparams.TestNamespaceName, key, value)
			}
		})

		It("Baseline: reports directory is writable on the test runner", func() {
			err := prereqVerifyWritableDir(RanDuTestConfig.ReportsDirAbsPath)
			Expect(err).ToNot(HaveOccurred(),
				"Reports directory %s (ECO_REPORTS_DUMP_DIR) is not writable on the test runner",
				RanDuTestConfig.ReportsDirAbsPath)
		})

		It("Workload: create and delete scripts are present on the test runner", func() {
			if RanDuTestConfig.TestWorkload.CreateMethod != randuparams.TestWorkloadShellLaunchMethod {
				Skip(fmt.Sprintf("Workload create method is %q, not %q",
					RanDuTestConfig.TestWorkload.CreateMethod, randuparams.TestWorkloadShellLaunchMethod))
			}

			By("Checking the workload create script")

			err := prereqVerifyExecutable(RanDuTestConfig.TestWorkload.CreateShellCmd)
			Expect(err).ToNot(HaveOccurred(), "Workload create command %q is not usable on the test runner",
				RanDuTestConfig.TestWorkload.CreateShellCmd)

			By("Checking the workload delete script")

			err = prereqVerifyExecutable(RanDuTestConfig.TestWorkload.DeleteShellCmd)
			Expect(err).ToNot(HaveOccurred(), "Workload delete command %q is not usable on the test runner",
				RanDuTestConfig.TestWorkload.DeleteShellCmd)
		})

		It("Workload: a StorageClass is available for the workload volume claims", func() {
			storageClasses, err := APIClient.StorageClasses().List(context.TODO(), metav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred(), "Failed to list the cluster StorageClasses")
			Expect(storageClasses.Items).ToNot(BeEmpty(),
				"No StorageClass in the cluster, the workload StatefulSet volumeClaimTemplates cannot bind")

			defaultClasses := []string{}

			for _, storageClass := range storageClasses.Items {
				if storageClass.Annotations[prereqDefaultStorageClassAnnotation] == "true" {
					defaultClasses = append(defaultClasses, storageClass.Name)
				}
			}

			if len(defaultClasses) == 0 {
				klog.V(randuparams.RanDuLogLevel).Infof(
					"No default StorageClass, the workload manifests must name the class they use")
			}

			klog.V(randuparams.RanDuLogLevel).Infof("Cluster has %d StorageClasses, default: %v",
				len(storageClasses.Items), defaultClasses)
		})

		It("Workload: SR-IOV network operator serves policies with allocated VFs", func() {
			sriovNamespace := RanDuTestConfig.SriovOperatorNamespace

			if !namespace.NewBuilder(APIClient, sriovNamespace).Exists() {
				Skip(fmt.Sprintf("Namespace %s does not exist, the SR-IOV network operator is not installed",
					sriovNamespace))
			}

			By("Listing the SR-IOV node policies and networks")

			// The reboot suites list both object types up front and fail when the API is unavailable.
			policies, err := sriov.ListPolicy(APIClient, sriovNamespace)
			Expect(err).ToNot(HaveOccurred(), "Failed to list SriovNetworkNodePolicy objects in %s", sriovNamespace)

			networks, err := sriov.List(APIClient, sriovNamespace)
			Expect(err).ToNot(HaveOccurred(), "Failed to list SriovNetwork objects in %s", sriovNamespace)

			klog.V(randuparams.RanDuLogLevel).Infof("Found %d SriovNetworkNodePolicy and %d SriovNetwork objects in %s",
				len(policies), len(networks), sriovNamespace)

			if len(policies) == 0 {
				Skip(fmt.Sprintf("No SriovNetworkNodePolicy in %s, the workload cannot request VFs", sriovNamespace))
			}

			By("Verifying the policies produced virtual functions on the nodes")

			nodeStates, err := sriov.ListNetworkNodeState(APIClient, sriovNamespace)
			Expect(err).ToNot(HaveOccurred(), "Failed to list SriovNetworkNodeState objects in %s", sriovNamespace)

			allocatedVFs := 0

			for _, nodeState := range nodeStates {
				for _, iface := range nodeState.Objects.Status.Interfaces {
					allocatedVFs += iface.NumVfs
				}
			}

			Expect(allocatedVFs).To(BeNumerically(">", 0),
				"%d SriovNetworkNodePolicy objects are defined but no VF is allocated on any node, "+
					"the workload pod network annotations cannot be satisfied", len(policies))
		})

		It("WorkloadForceCleanup: the workload namespace holds Guaranteed QoS pods", func() {
			workloadNamespace := RanDuTestConfig.TestWorkload.Namespace

			if !namespace.NewBuilder(APIClient, workloadNamespace).Exists() {
				Skip(fmt.Sprintf("Workload namespace %s does not exist, deploy the workload to validate its QoS",
					workloadNamespace))
			}

			workloadPods, err := pod.List(APIClient, workloadNamespace, metav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred(), "Failed to list pods in %s", workloadNamespace)

			guaranteed := 0

			for _, workloadPod := range workloadPods {
				if workloadPod.Object.Status.QOSClass == corev1.PodQOSGuaranteed {
					guaranteed++
				}
			}

			Expect(guaranteed).To(BeNumerically(">", 0),
				"None of the %d pods in %s is Guaranteed QoS, WorkloadForceCleanup would delete nothing "+
					"and pass vacuously. Guaranteed pods need the CPU manager static policy and integer "+
					"CPU requests equal to limits", len(workloadPods), workloadNamespace)

			klog.V(randuparams.RanDuLogLevel).Infof("Namespace %s holds %d Guaranteed QoS pods out of %d",
				workloadNamespace, guaranteed, len(workloadPods))
		})

		It("Stability: the stability output path is writable on the test runner", func() {
			err := prereqVerifyWritableDir(RanDuTestConfig.StabilityOutputPath)
			Expect(err).ToNot(HaveOccurred(),
				"Stability output path %s (ECO_RANDU_STABILITY_OUTPUT_PATH) is not writable on the test runner",
				RanDuTestConfig.StabilityOutputPath)
		})

		It("ZTPPoliciesCompliance: ACM policies exist in the cluster namespace", func() {
			clusterName, err := platform.GetOCPClusterName(APIClient)
			Expect(err).ToNot(HaveOccurred(), "Failed to resolve the cluster name from the Infrastructure CR")

			policies, err := ocm.ListPoliciesInAllNamespaces(APIClient,
				runtimeclient.ListOptions{Namespace: clusterName})
			if err != nil {
				// Without ACM the compliance suite cannot run at all, but the stability suite only needs
				// policies when its policy sampling is enabled.
				Expect(RanDuTestConfig.StabilityPoliciesCheck).To(BeFalse(),
					"ECO_RANDU_STABILITY_POLICIES_CHECK is enabled but policies cannot be listed: %v", err)

				Skip(fmt.Sprintf("ACM policies cannot be listed, ZTPPoliciesCompliance cannot run here: %v", err))
			}

			Expect(policies).ToNot(BeEmpty(),
				"No Policy found in namespace %q. ZTPPoliciesCompliance asserts nothing and passes "+
					"vacuously unless the managed cluster namespace matches the Infrastructure name", clusterName)

			klog.V(randuparams.RanDuLogLevel).Infof("Found %d policies in namespace %s", len(policies), clusterName)
		})

		It("SriovFecNodeConfigStatus: every node has a configured SriovFecNodeConfig", func() {
			fecNamespace := RanDuTestConfig.SriovFecOperatorNamespace

			if !namespace.NewBuilder(APIClient, fecNamespace).Exists() {
				Skip(fmt.Sprintf("Namespace %s does not exist, the SR-IOV FEC operator is not installed",
					fecNamespace))
			}

			var unconfigured []string

			for _, node := range clusterNodes {
				nodeName := node.Definition.Name

				fecConfig, err := sriovfec.Pull(APIClient, nodeName, fecNamespace)
				Expect(err).ToNot(HaveOccurred(), "No SriovFecNodeConfig named %s in %s", nodeName, fecNamespace)

				configured := false

				for _, condition := range fecConfig.Object.Status.Conditions {
					if condition.Type == "Configured" && condition.Status == "True" {
						configured = true
					}
				}

				if !configured {
					unconfigured = append(unconfigured, nodeName)
				}
			}

			Expect(unconfigured).To(BeEmpty(),
				"SriovFecNodeConfig is not Configured on nodes %v", unconfigured)
		})

		It("SoftReboot/HardReboot/KernelCrashKdump: the openshift-apiserver deployment exists", func() {
			apiServer, err := deployment.Pull(APIClient, prereqAPIServerDeployment, prereqAPIServerNamespace)
			Expect(err).ToNot(HaveOccurred(), "Failed to pull the %s deployment in %s, the reboot and kdump "+
				"suites wait for it to become Available after every node event",
				prereqAPIServerDeployment, prereqAPIServerNamespace)

			Expect(apiServer.IsReady(time.Minute)).To(BeTrue(),
				"Deployment %s in %s is not Available", prereqAPIServerDeployment, prereqAPIServerNamespace)
		})

		It("HardReboot: worker nodes expose a local IPMI interface", func() {
			Expect(RanDuTestConfig.IpmiToolImage).ToNot(BeEmpty(),
				"ECO_SYSTEM_TESTS_IPMITOOL_IMAGE is empty, the power cycle pod has no image to run")

			Expect(workerNodes).ToNot(BeEmpty(),
				"No node labelled %s, HardReboot power cycles nothing", prereqWorkerNodeLabel)

			var missingIPMI []string

			for _, node := range workerNodes {
				nodeName := node.Definition.Name

				// The power cycle runs ipmitool with no host arguments, so it needs the in-band device.
				output, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName,
					"test -c /dev/ipmi0 && echo present || echo missing")
				Expect(err).ToNot(HaveOccurred(), "Failed to check /dev/ipmi0 on node %s", nodeName)

				if !strings.Contains(output, "present") {
					missingIPMI = append(missingIPMI, nodeName)
				}
			}

			Expect(missingIPMI).To(BeEmpty(),
				"/dev/ipmi0 is missing on nodes %v, in-band ipmitool cannot power cycle them. Load "+
					"ipmi_devintf or run NMIKernelCrashKdump instead", missingIPMI)
		})

		It("KernelCrashKdump: kdump writes a local vmcore on every node", func() {
			var (
				inactive     []string
				noCrashKmem  []string
				remoteTarget []string
			)

			for _, node := range clusterNodes {
				nodeName := node.Definition.Name

				By(fmt.Sprintf("Reading the kdump configuration of node %s", nodeName))

				state, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, "systemctl is-active kdump.service || true")
				Expect(err).ToNot(HaveOccurred(), "Failed to read the kdump service state on node %s", nodeName)

				if strings.TrimSpace(state) != "active" {
					inactive = append(inactive, fmt.Sprintf("%s (%s)", nodeName, strings.TrimSpace(state)))
				}

				reserved, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, "cat /sys/kernel/kexec_crash_size")
				Expect(err).ToNot(HaveOccurred(), "Failed to read the crash kernel size on node %s", nodeName)

				crashSize, err := strconv.Atoi(strings.TrimSpace(reserved))
				if err != nil || crashSize == 0 {
					noCrashKmem = append(noCrashKmem, nodeName)
				}

				kdumpConf, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, "cat /etc/kdump.conf")
				Expect(err).ToNot(HaveOccurred(), "Failed to read /etc/kdump.conf on node %s", nodeName)

				dumpPath, targetErr := prereqKdumpLocalPath(kdumpConf)
				if targetErr != nil {
					remoteTarget = append(remoteTarget, fmt.Sprintf("%s (%v)", nodeName, targetErr))
				} else if dumpPath != prereqKdumpDefaultPath {
					remoteTarget = append(remoteTarget, fmt.Sprintf("%s (path %s)", nodeName, dumpPath))
				}
			}

			Expect(inactive).To(BeEmpty(), "kdump.service is not active on nodes %v", inactive)
			Expect(noCrashKmem).To(BeEmpty(), "No crash kernel memory is reserved on nodes %v", noCrashKmem)
			Expect(remoteTarget).To(BeEmpty(),
				"kdump does not write to the local %s on nodes %v, the suites read the vmcore from there",
				prereqKdumpDefaultPath, remoteTarget)
		})

		It("NMIKernelCrashKdump: BMC credentials cover every node", func() {
			if len(RanDuTestConfig.NodesCredentialsMap) == 0 {
				Skip("ECO_RANDU_NODES_CREDENTIALS_MAP is empty, NMIKernelCrashKdump skips cleanly")
			}

			var incomplete []string

			for _, node := range clusterNodes {
				nodeName := node.Definition.Name

				credentials, found := RanDuTestConfig.NodesCredentialsMap[nodeName]
				if !found {
					incomplete = append(incomplete, fmt.Sprintf("%s (no entry)", nodeName))

					continue
				}

				if credentials.Username == "" || credentials.Password == "" || credentials.BMCAddress == "" {
					incomplete = append(incomplete, fmt.Sprintf("%s (incomplete entry)", nodeName))
				}
			}

			Expect(incomplete).To(BeEmpty(),
				"ECO_RANDU_NODES_CREDENTIALS_MAP is set but does not cover %v. The suite fails on a node "+
					"it has no Redfish credentials for", incomplete)
		})

		It("PTP: the PTP operator runs a linuxptp daemon and a PtpConfig applies", func() {
			skipUnlessPTPEnabled()

			By("Verifying the linuxptp daemon pods are running")

			ptpPods, err := pod.List(APIClient, sysptp.Namespace, metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(labels.Set{
					sysptp.DaemonPodLabelKey: sysptp.DaemonPodLabelValueLinuxpt,
				}).String(),
			})
			Expect(err).ToNot(HaveOccurred(), "Failed to list the linuxptp daemon pods")
			Expect(ptpPods).ToNot(BeEmpty(), "No linuxptp daemon pod in %s", sysptp.Namespace)

			var notRunning []string

			for _, daemonPod := range ptpPods {
				if daemonPod.Object.Status.Phase != corev1.PodRunning {
					notRunning = append(notRunning, daemonPod.Definition.Name)
				}
			}

			Expect(notRunning).To(BeEmpty(), "linuxptp daemon pods are not Running: %v", notRunning)

			By("Verifying a PtpConfig profile applies to every PTP node")

			for _, nodeName := range ptpDaemonNodes() {
				profiles, err := sysptp.ProfilesForNode(APIClient, nodeName)
				Expect(err).ToNot(HaveOccurred(), "Failed to resolve the PtpConfig profiles of node %s", nodeName)
				Expect(profiles).ToNot(BeEmpty(), "No PtpConfig profile applies to node %s", nodeName)
			}
		})

		It("PTP: the cluster and PTP clocks are synchronized", func() {
			skipUnlessPTPEnabled()

			synchronized, err := sysptp.ValidatePTPStatus(APIClient, prereqPTPLogWindow)
			Expect(err).ToNot(HaveOccurred(), "Failed to validate the PTP status")
			Expect(synchronized).To(BeTrue(),
				"The PTP clock is not synchronized. The workload, reboot and stability suites validate "+
					"PTP after every step unless ECO_RANDU_PTP_ENABLED is false")
		})

		It("PTP3WPCValidation: the WPC sync interfaces resolve on every PTP node", func() {
			skipUnlessPTPEnabled()

			for _, nodeName := range ptpDaemonNodes() {
				daemonPod, err := sysptp.GetLinuxptpDaemonPodOnNode(APIClient, nodeName)
				Expect(err).ToNot(HaveOccurred(), "Failed to get the PTP daemon pod on node %s", nodeName)

				logs, err := daemonPod.GetLogsWithOptions(&corev1.PodLogOptions{
					Container: sysptp.DaemonContainerName,
					TailLines: ptr(int64(prereqDaemonLogTailLines)),
				})
				Expect(err).ToNot(HaveOccurred(), "Failed to read the PTP daemon logs on node %s", nodeName)

				plan, err := sysptp.ResolveWPCInterfaces(
					RanDuTestConfig.PtpWpcSyncInterfaces,
					RanDuTestConfig.PtpWpcPrimaryInterface,
					string(logs),
				)
				if err != nil {
					Skip(fmt.Sprintf("Node %s is not a WPC grandmaster: %v", nodeName, err))
				}

				klog.V(randuparams.RanDuLogLevel).Infof("Node %s: WPC sync interfaces %v, primary %s",
					nodeName, plan.SyncAll, plan.Primary)
			}

			By("Reporting the cases that are skipped for missing configuration")

			if RanDuTestConfig.PtpIperf3Server == "" {
				klog.V(randuparams.RanDuLogLevel).Infof(
					"ECO_RANDU_PTP_IPERF3_SERVER is empty, PTP3WPCValidation Case 06 will skip")
			}

			if RanDuTestConfig.PtpNetemInterface == "" {
				klog.V(randuparams.RanDuLogLevel).Infof(
					"ECO_RANDU_PTP_NETEM_INTERFACE is empty, PTP3WPCValidation Case 07 will skip")
			}
		})

		It("PTPGNRDBCNoHoldoverValidation: every PTP node runs a T-BC without holdover", func() {
			skipUnlessPTPEnabled()

			for _, nodeName := range ptpDaemonNodes() {
				profile, reason := sysptp.ResolveNoHoldoverTBC(APIClient, nodeName)
				if reason != "" {
					Skip(fmt.Sprintf("Node %s is not running a %s without holdover: %s",
						nodeName, sysptp.ClockTypeTBC, reason))
				}

				klog.V(randuparams.RanDuLogLevel).Infof(
					"Node %s runs %s profile %s: upstream port %s, leading interface %s, %d downstream ports",
					nodeName, sysptp.ClockTypeTBC, profile.Receiver.ReceiverProfile, profile.Receiver.UpstreamPort,
					profile.Receiver.LeadingInterface, len(profile.Transmitter.Ports))
			}
		})

		It("PTPGNRDBCNoHoldoverValidation: the upstream port Case 07 takes down carries no cluster traffic", func() {
			skipUnlessPTPEnabled()

			if !RanDuTestConfig.PtpTBCDisruptive {
				Skip("ECO_RANDU_PTP_TBC_DISRUPTIVE is false, Case 07 is opt-in and will skip")
			}

			for _, nodeName := range ptpDaemonNodes() {
				profile, reason := sysptp.ResolveNoHoldoverTBC(APIClient, nodeName)
				if reason != "" {
					Skip(fmt.Sprintf("Node %s is not running a %s without holdover: %s",
						nodeName, sysptp.ClockTypeTBC, reason))
				}

				upstreamPort := profile.Receiver.UpstreamPort

				By(fmt.Sprintf("Verifying %s on node %s holds no cluster address", upstreamPort, nodeName))

				addresses, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName,
					fmt.Sprintf("ip -br addr show dev %s", sysptp.ShellQuoteArg(upstreamPort)))
				Expect(err).ToNot(HaveOccurred(), "Failed to read the addresses of %s on node %s",
					upstreamPort, nodeName)
				Expect(prereqRoutableAddresses(addresses)).To(BeEmpty(),
					"Upstream port %s on node %s holds address(es) %v. Taking it down would cut the node off "+
						"and Case 07 could not bring it back", upstreamPort, nodeName,
					prereqRoutableAddresses(addresses))

				By(fmt.Sprintf("Verifying %s on node %s is not a br-ex member", upstreamPort, nodeName))

				brexPorts, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, "ovs-vsctl list-ports br-ex || true")
				Expect(err).ToNot(HaveOccurred(), "Failed to list the br-ex ports on node %s", nodeName)
				Expect(strings.Fields(brexPorts)).ToNot(ContainElement(upstreamPort),
					"Upstream port %s on node %s is a br-ex member", upstreamPort, nodeName)

				By(fmt.Sprintf("Verifying %s on node %s does not carry the default route", upstreamPort, nodeName))

				defaultRoute, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, "ip route show default")
				Expect(err).ToNot(HaveOccurred(), "Failed to read the default route on node %s", nodeName)
				Expect(strings.Fields(defaultRoute)).ToNot(ContainElement(upstreamPort),
					"Upstream port %s on node %s carries the default route: %s", upstreamPort, nodeName,
					strings.TrimSpace(defaultRoute))
			}
		})

		It("CertManager: the operator is installed and its ClusterIssuer is Ready", func() {
			if !namespace.NewBuilder(APIClient, certmanager.OperatorNamespace).Exists() {
				Skip(fmt.Sprintf("Namespace %s does not exist, the cert-manager operator is not installed",
					certmanager.OperatorNamespace))
			}

			By("Verifying the operator and operand pods are running")

			for _, nsName := range []string{certmanager.OperatorNamespace, certmanager.Namespace} {
				certPods, err := pod.List(APIClient, nsName, metav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred(), "Failed to list pods in %s", nsName)
				Expect(certPods).ToNot(BeEmpty(), "No pod running in %s", nsName)

				for _, certPod := range certPods {
					Expect(certPod.Object.Status.Phase).To(Equal(corev1.PodRunning),
						"Pod %s in %s is %s", certPod.Definition.Name, nsName, certPod.Object.Status.Phase)
				}
			}

			By("Verifying the cert-manager CRDs are installed")

			for _, crdName := range []string{
				"certificates.cert-manager.io", "issuers.cert-manager.io", "clusterissuers.cert-manager.io",
			} {
				_, err := APIClient.Resource(certmanager.CrdGVR).Get(context.TODO(), crdName, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred(), "CRD %s is not installed", crdName)
			}

			By("Verifying the ClusterIssuer is Ready")

			issuerName := getIssuerName()

			ready, err := certmanager.IsClusterIssuerReady(APIClient, issuerName)
			Expect(err).ToNot(HaveOccurred(), "Failed to read ClusterIssuer %s", issuerName)

			if !ready {
				Skip(fmt.Sprintf("ClusterIssuer %s is not Ready, the cert-manager suite skips cleanly", issuerName))
			}
		})

		It("CertManager: the DNS server and domains are configured", func() {
			if !namespace.NewBuilder(APIClient, certmanager.OperatorNamespace).Exists() {
				Skip(fmt.Sprintf("Namespace %s does not exist, the cert-manager operator is not installed",
					certmanager.OperatorNamespace))
			}

			requiredSettings := map[string]string{
				"ECO_RANDU_CERTMANAGER_DNS_SERVER":  RanDuTestConfig.CertManager.DNSServer,
				"ECO_RANDU_CERTMANAGER_CERT_DOMAIN": RanDuTestConfig.CertManager.CertDomain,
				"ECO_RANDU_CERTMANAGER_API_DOMAIN":  RanDuTestConfig.CertManager.APIDomain,
				"ECO_RANDU_CERTMANAGER_APPS_DOMAIN": RanDuTestConfig.CertManager.AppsDomain,
				"ECO_RANDU_CERTMANAGER_INGRESS_IP":  RanDuTestConfig.CertManager.IngressIP,
			}

			var unset []string

			for name, value := range requiredSettings {
				if value == "" {
					unset = append(unset, name)
				}
			}

			Expect(unset).To(BeEmpty(), "The cert-manager suite needs these variables set: %v", unset)
		})
	},
)

// prereqVerifyWritableDir reports whether path is a directory the test runner can write to.
func prereqVerifyWritableDir(path string) error {
	if path == "" {
		return fmt.Errorf("no directory configured")
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}

	probe, err := os.CreateTemp(path, "randu-prereq-*")
	if err != nil {
		return err
	}

	probeName := probe.Name()

	err = probe.Close()
	if err != nil {
		return err
	}

	return os.Remove(probeName)
}

// prereqVerifyExecutable reports whether the program a shell command runs exists on the test runner
// and is executable. Only the first word is inspected, the rest of the command line is arguments.
func prereqVerifyExecutable(shellCmd string) error {
	commandFields := strings.Fields(shellCmd)
	if len(commandFields) == 0 {
		return fmt.Errorf("the command is empty")
	}

	info, err := os.Stat(commandFields[0])
	if err != nil {
		return err
	}

	if info.Mode().Perm()&prereqExecutableBits == 0 {
		return fmt.Errorf("%s is not executable", commandFields[0])
	}

	return nil
}

// prereqKdumpLocalPath returns the local directory an /etc/kdump.conf writes the vmcore to. It
// errors when the dump target is remote, because the kdump suites read the vmcore from the node
// filesystem and a remote target leaves nothing behind for them to find.
func prereqKdumpLocalPath(kdumpConf string) (string, error) {
	dumpPath := prereqKdumpDefaultPath

	for _, line := range strings.Split(kdumpConf, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}

		switch fields[0] {
		case "nfs", "ssh", "raw":
			return "", fmt.Errorf("dump target is remote: %s %s", fields[0], fields[1])
		case "path":
			dumpPath = fields[1]
		}
	}

	return dumpPath, nil
}

// prereqRoutableAddresses returns the non link-local addresses an "ip -br addr show" line reports.
func prereqRoutableAddresses(ipBriefOutput string) []string {
	var addresses []string

	for _, field := range strings.Fields(ipBriefOutput) {
		if strings.Contains(field, "/") && !strings.HasPrefix(field, "fe80:") {
			addresses = append(addresses, field)
		}
	}

	return addresses
}
