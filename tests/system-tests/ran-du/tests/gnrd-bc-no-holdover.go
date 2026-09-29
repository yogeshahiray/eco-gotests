package ran_du_system_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	sysptp "github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/internal/ptp"
	. "github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/ran-du/internal/randuinittools"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/system-tests/ran-du/internal/randuparams"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	// gnrdBCLockedClockClass is advertised while the boundary clock is traceable to a locked grandmaster.
	gnrdBCLockedClockClass = 6
	// gnrdBCFreerunClockClass is advertised once a no holdover boundary clock loses its time source.
	gnrdBCFreerunClockClass = 248
	// gnrdBCDPLLPhaseLocked is the phase_status of a DPLL holding a high precision phase lock.
	gnrdBCDPLLPhaseLocked = 3
	// gnrdBCDPLLMinFrequencyStatus is DPLL_LOCKED, the daemon also reports 4 for DPLL_LOCKED_HO_ACQ.
	gnrdBCDPLLMinFrequencyStatus = 3
	// gnrdBCDPLLPPSValid is the pps_status of a DPLL receiving a valid 1pps signal.
	gnrdBCDPLLPPSValid = 1
	// gnrdBCPortStateMaster is the port state of a downstream port that is distributing time.
	gnrdBCPortStateMaster = "MASTER"
	// gnrdBCLogTailLines is the number of daemon log lines the non-disruptive cases inspect.
	gnrdBCLogTailLines = 2000
	// gnrdBCStateChangeTimeout bounds how long Case 07 waits for a clock state transition.
	gnrdBCStateChangeTimeout = 5 * time.Minute
	// gnrdBCStatePollInterval is how often Case 07 re-reads the daemon logs.
	gnrdBCStatePollInterval = 10 * time.Second
)

// This suite covers the GNR-D telecom boundary clock profiles that take time from an upstream
// grandmaster over a TimeReceiver port. Unlike the 3 WPC grandmaster suite in ptp-3wpc.go there is
// no GNSS receiver involved, and the profile is configured with LocalHoldoverTimeout 0, so the
// clock must drop straight to free-run rather than ride out an upstream loss on holdover.
var _ = Describe(
	"PTP GNR-D BC No Holdover Validation",
	Ordered,
	Label(randuparams.LabelPTPGNRDBCNoHoldover),
	func() {
		var (
			ptpNodes    []string
			tbcSettings map[string]sysptp.TBCSettings
			ttSettings  map[string]sysptp.TTSettings
		)

		// daemonLogsOnNode returns the linuxptp daemon pod of a node together with the tail of its log.
		daemonLogsOnNode := func(nodeName string) (*pod.Builder, string) {
			daemonPod, err := sysptp.GetLinuxptpDaemonPodOnNode(APIClient, nodeName)
			Expect(err).ToNot(HaveOccurred(), "Failed to get PTP daemon pod on node %s", nodeName)

			logs, err := daemonPod.GetLogsWithOptions(&corev1.PodLogOptions{
				Container: sysptp.DaemonContainerName,
				TailLines: ptr(int64(gnrdBCLogTailLines)),
			})
			Expect(err).ToNot(HaveOccurred(), "Failed to get PTP daemon logs on node %s", nodeName)

			return daemonPod, string(logs)
		}

		BeforeAll(func() {
			if !RanDuTestConfig.PtpEnabled {
				Skip("PTP is not enabled in RanDu configuration")
			}

			By("Listing PTP daemon pods")

			ptpPods, err := pod.List(APIClient, sysptp.Namespace, metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(labels.Set{
					sysptp.DaemonPodLabelKey: sysptp.DaemonPodLabelValueLinuxpt,
				}).String(),
			})
			Expect(err).ToNot(HaveOccurred(), "Failed to list PTP daemon pods")
			Expect(ptpPods).ToNot(BeEmpty(), "No PTP daemon pods found")

			nodeSet := make(map[string]struct{})

			for _, p := range ptpPods {
				if p.Object.Spec.NodeName != "" {
					nodeSet[p.Object.Spec.NodeName] = struct{}{}
				}
			}

			ptpNodes = make([]string, 0, len(nodeSet))
			for n := range nodeSet {
				ptpNodes = append(ptpNodes, n)
			}

			By("Verifying every PTP node really runs a boundary clock configured without holdover")

			// Gate the whole suite on the profile rather than letting individual cases fail. Against a
			// boundary clock with a holdover window the clock correctly enters s1 on upstream loss and
			// Case 07 would report that correct behaviour as a failure.
			tbcSettings = make(map[string]sysptp.TBCSettings, len(ptpNodes))
			ttSettings = make(map[string]sysptp.TTSettings, len(ptpNodes))

			for _, nodeName := range ptpNodes {
				profile, reason := sysptp.ResolveNoHoldoverTBC(APIClient, nodeName)
				if reason != "" {
					Skip(fmt.Sprintf(
						"Node %s is not running a %s without holdover: %s", nodeName, sysptp.ClockTypeTBC, reason))
				}

				tbcSettings[nodeName] = profile.Receiver
				ttSettings[nodeName] = profile.Transmitter

				klog.V(randuparams.RanDuLogLevel).Infof(
					"Node %s runs %s profile %s: upstream port %s, leading interface %s, "+
						"%s %ds, %s %dns, %d downstream ports programmed by profile %s",
					nodeName, sysptp.ClockTypeTBC, profile.Receiver.ReceiverProfile,
					profile.Receiver.UpstreamPort, profile.Receiver.LeadingInterface,
					sysptp.PluginSettingLocalHoldoverTimeout, profile.Receiver.Holdover.LocalHoldoverTimeout,
					sysptp.PluginSettingMaxInSpecOffset, profile.Receiver.Holdover.MaxInSpecOffset,
					len(profile.Transmitter.Ports), profile.Transmitter.Profile)
			}

			klog.V(randuparams.RanDuLogLevel).Infof(
				"PTP GNR-D BC no holdover validation will run on nodes: %v", ptpNodes)
		})

		// Case 01: To verify the boundary clock is locked to its upstream grandmaster
		// Test_Description: Verify the TimeReceiver port has selected an upstream grandmaster and
		// reports SLAVE, which is what replaces the GNSS lock check of a grandmaster profile.
		It("Case 01: To verify the boundary clock is locked to its upstream grandmaster",
			reportxml.ID("99981"), func() {
				for _, nodeName := range ptpNodes {
					settings := tbcSettings[nodeName]

					By(fmt.Sprintf("Verify upstream port %s reports SLAVE on node %s", settings.UpstreamPort, nodeName))

					daemonPod, logStr := daemonLogsOnNode(nodeName)

					configPath, socketPath, err := sysptp.Ptp4lInstanceForPort(daemonPod, settings.UpstreamPort)
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					portData, err := sysptp.RunPmc(daemonPod, configPath, socketPath, "GET PORT_DATA_SET")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					Expect(sysptp.PmcFieldValues(portData, "portState")).To(ContainElement("SLAVE"),
						"Node %s: upstream port %s is not SLAVE, the boundary clock has no time source: %s",
						nodeName, settings.UpstreamPort, portData)

					By("Verify the upstream grandmaster is present")

					timeStatus, err := sysptp.RunPmc(daemonPod, configPath, socketPath, "GET TIME_STATUS_NP")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					gmPresent, found := sysptp.PmcFieldValue(timeStatus, "gmPresent")
					Expect(found).To(BeTrue(), "Node %s: no gmPresent in pmc output: %s", nodeName, timeStatus)
					Expect(gmPresent).To(Equal("true"),
						"Node %s: gmPresent is %s, no upstream grandmaster is announcing", nodeName, gmPresent)

					By("Verify the TimeReceiver instance reports sync state s2 in the daemon logs")

					// Scoped to the receiver config: the other linuxptp instances on this node log
					// their own s2 lines, so a bare substring match would pass while ptp4l.1 is not synced.
					state, found := sysptp.LatestPtp4lState(logStr, configPath)
					Expect(found).To(BeTrue(),
						"Node %s: no ptp4l sync state lines for %s in logs", nodeName, configPath)
					Expect(state).To(Equal(sysptp.StateLocked),
						"Node %s: the TimeReceiver instance %s reports %s, expected %s",
						nodeName, configPath, state, sysptp.StateLocked)
				}
			})

		// Case 02: To verify grandmaster traceability is propagated to the downstream ports
		// Test_Description: Verify the boundary clock inherits the clock class of the upstream
		// grandmaster and announces the same quality to its own downstream TimeTransmitter ports.
		It("Case 02: To verify grandmaster traceability is propagated to the downstream ports",
			reportxml.ID("99982"), func() {
				for _, nodeName := range ptpNodes {
					settings := tbcSettings[nodeName]

					By(fmt.Sprintf("Verify the upstream grandmaster announces clock class %d on node %s",
						gnrdBCLockedClockClass, nodeName))

					daemonPod, _ := daemonLogsOnNode(nodeName)

					receiverConfig, receiverSocket, err := sysptp.Ptp4lInstanceForPort(daemonPod, settings.UpstreamPort)
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					parentData, err := sysptp.RunPmc(daemonPod, receiverConfig, receiverSocket, "GET PARENT_DATA_SET")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					gmClockClass, err := sysptp.PmcFieldInt(parentData, "gm.ClockClass")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)
					Expect(gmClockClass).To(Equal(gnrdBCLockedClockClass),
						"Node %s: upstream gm.ClockClass is %d, expected %d. A class above %d means the "+
							"grandmaster is not synchronized", nodeName, gmClockClass, gnrdBCLockedClockClass,
						gnrdBCLockedClockClass)

					gmIdentity, found := sysptp.PmcFieldValue(parentData, "grandmasterIdentity")
					Expect(found).To(BeTrue(), "Node %s: no grandmasterIdentity in pmc output: %s", nodeName, parentData)

					By("Verify the TimeTransmitter instance announces the same grandmaster and clock class")

					transmitterConfig, foundTransmitter := gnrdBCTransmitterConfig(daemonPod, receiverConfig)
					if !foundTransmitter {
						Skip(fmt.Sprintf(
							"Node %s runs a single ptp4l instance, it has no separate TimeTransmitter profile", nodeName))
					}

					transmitterSocket := sysptp.Ptp4lSocketForConfig(transmitterConfig)

					defaultData, err := sysptp.RunPmc(daemonPod, transmitterConfig, transmitterSocket,
						"GET DEFAULT_DATA_SET")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					downstreamClockClass, err := sysptp.PmcFieldInt(defaultData, "clockClass")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)
					Expect(downstreamClockClass).To(Equal(gnrdBCLockedClockClass),
						"Node %s: downstream clockClass is %d, expected %d. %d means the boundary clock is "+
							"free-running and is no longer traceable", nodeName, downstreamClockClass,
						gnrdBCLockedClockClass, gnrdBCFreerunClockClass)

					transmitterParent, err := sysptp.RunPmc(daemonPod, transmitterConfig, transmitterSocket,
						"GET PARENT_DATA_SET")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					Expect(sysptp.PmcFieldValues(transmitterParent, "grandmasterIdentity")).To(ContainElement(gmIdentity),
						"Node %s: downstream ports announce a different grandmaster than the upstream port %s",
						nodeName, gmIdentity)
				}
			})

		// Case 03: To verify dpll phase and frequency lock on the leading interface
		// Test_Description: Verify the DPLL of the card that drives the other cards holds a hardware
		// lock taken from the upstream PTP source rather than from a GNSS receiver.
		It("Case 03: To verify dpll phase and frequency lock on the leading interface",
			reportxml.ID("99983"), func() {
				for _, nodeName := range ptpNodes {
					settings := tbcSettings[nodeName]

					By(fmt.Sprintf("Verify the dpll state of leading interface %s on node %s",
						settings.LeadingInterface, nodeName))

					_, logStr := daemonLogsOnNode(nodeName)

					dpllStatuses := sysptp.ParseDPLLStatusLines(logStr, settings.LeadingInterface)
					Expect(dpllStatuses).ToNot(BeEmpty(),
						"Node %s: no dpll status lines for leading interface %s in logs",
						nodeName, settings.LeadingInterface)

					latest := dpllStatuses[len(dpllStatuses)-1]

					Expect(latest.PhaseStatus).To(Equal(gnrdBCDPLLPhaseLocked),
						"Node %s: dpll phase_status is %d, expected %d for a high precision hardware lock",
						nodeName, latest.PhaseStatus, gnrdBCDPLLPhaseLocked)
					Expect(latest.FrequencyStatus).To(BeNumerically(">=", gnrdBCDPLLMinFrequencyStatus),
						"Node %s: dpll frequency_status is %d, expected at least %d (DPLL_LOCKED)",
						nodeName, latest.FrequencyStatus, gnrdBCDPLLMinFrequencyStatus)
					Expect(latest.PPSStatus).To(Equal(gnrdBCDPLLPPSValid),
						"Node %s: dpll pps_status is %d, expected %d. Check the 1pps distribution to %s",
						nodeName, latest.PPSStatus, gnrdBCDPLLPPSValid, settings.LeadingInterface)
					Expect(latest.State).To(Equal(sysptp.StateLocked),
						"Node %s: dpll state is %s, expected %s", nodeName, latest.State, sysptp.StateLocked)

					Expect(logStr).To(ContainSubstring(sysptp.InSpecTrueLogMark),
						"Node %s: dpll offset is reported out of spec", nodeName)
				}
			})

		// Case 04: To verify the composite T-BC state reports locked
		// Test_Description: Verify the boundary clock state machine combines the ptp4l, ts2phc and
		// dpll states into a locked composite state with no lost time source.
		It("Case 04: To verify the composite T-BC state reports locked", reportxml.ID("99984"), func() {
			for _, nodeName := range ptpNodes {
				settings := tbcSettings[nodeName]

				By(fmt.Sprintf("Verify the T-BC composite state on node %s", nodeName))

				_, logStr := daemonLogsOnNode(nodeName)

				tbcStatuses := sysptp.ParseTBCStatusLines(logStr, settings.LeadingInterface)
				Expect(tbcStatuses).ToNot(BeEmpty(),
					"Node %s: no %s lines for leading interface %s in logs",
					nodeName, sysptp.TBCStatusLogMark, settings.LeadingInterface)

				latest := tbcStatuses[len(tbcStatuses)-1]
				Expect(latest.State).To(Equal(sysptp.StateLocked),
					"Node %s: %s is %s, expected %s", nodeName, sysptp.TBCStatusLogMark, latest.State,
					sysptp.StateLocked)

				Expect(logStr).To(ContainSubstring(
					fmt.Sprintf("%s %s", sysptp.BCStateLogMark, sysptp.StateLocked)),
					"Node %s: expected %q in logs", nodeName, sysptp.BCStateLogMark)
				Expect(logStr).To(ContainSubstring(sysptp.SourceNotLostLogMark),
					"Node %s: the boundary clock reports a lost time source", nodeName)
			}
		})

		// Case 05: To verify the clock offsets stay within the in-spec threshold
		// Test_Description: Verify both the ptp4l offset to the upstream grandmaster and the phc2sys
		// offset of CLOCK_REALTIME stay inside the MaxInSpecOffset of the profile.
		It("Case 05: To verify the clock offsets stay within the in-spec threshold",
			reportxml.ID("99985"), func() {
				for _, nodeName := range ptpNodes {
					maxOffsetNS := tbcSettings[nodeName].Holdover.MaxInSpecOffset
					if maxOffsetNS <= 0 {
						maxOffsetNS = sysptp.DefaultMaxAbsOffsetNS
					}

					By(fmt.Sprintf("Verify ptp4l and phc2sys offsets stay within ±%dns on node %s",
						maxOffsetNS, nodeName))

					_, logStr := daemonLogsOnNode(nodeName)

					samples, withinSpec, detail := sysptp.OffsetSamplesWithinNS(
						logStr, sysptp.MasterOffsetRegexp, maxOffsetNS)
					Expect(samples).To(BeNumerically(">", 0),
						"Node %s: no ptp4l master offset samples in logs", nodeName)
					Expect(withinSpec).To(BeTrue(),
						"Node %s: ptp4l offset to the grandmaster is out of spec: %s", nodeName, detail)

					samples, withinSpec, detail = sysptp.OffsetSamplesWithinNS(
						logStr, sysptp.RealtimeOffsetRegexp, maxOffsetNS)
					Expect(samples).To(BeNumerically(">", 0),
						"Node %s: no phc2sys CLOCK_REALTIME offset samples in logs", nodeName)
					Expect(withinSpec).To(BeTrue(),
						"Node %s: CLOCK_REALTIME offset is out of spec: %s", nodeName, detail)
				}
			})

		// Case 06: To verify the profile is configured for no holdover and never enters it
		// Test_Description: Verify LocalHoldoverTimeout is 0 on the applied profile and that the
		// daemon reports no holdover state while the clock is locked.
		It("Case 06: To verify the profile is configured for no holdover and never enters it",
			reportxml.ID("99986"), func() {
				for _, nodeName := range ptpNodes {
					settings := tbcSettings[nodeName]

					By(fmt.Sprintf("Verify profile %s on node %s configures no holdover",
						settings.ReceiverProfile, nodeName))

					Expect(settings.Holdover.Found).To(BeTrue(),
						"Node %s: profile %s carries no Intel plugin holdover settings",
						nodeName, settings.ReceiverProfile)
					Expect(settings.Holdover.LocalHoldoverTimeout).To(Equal(0),
						"Node %s: profile %s configures a %ds holdover window, this suite covers the "+
							"no holdover profile", nodeName, settings.ReceiverProfile,
						settings.Holdover.LocalHoldoverTimeout)

					By("Verify the daemon reports no holdover state while the clock is locked")

					_, logStr := daemonLogsOnNode(nodeName)

					Expect(logStr).To(ContainSubstring(sysptp.OnHoldoverFalseLogMark),
						"Node %s: the dpll never reported %q", nodeName, sysptp.OnHoldoverFalseLogMark)
					Expect(logStr).ToNot(ContainSubstring(sysptp.DPLLHoldoverLogMark),
						"Node %s: the dpll entered holdover on a no holdover profile", nodeName)

					for _, status := range sysptp.ParseTBCStatusLines(logStr, "") {
						Expect(status.State).ToNot(Equal(sysptp.StateHoldover),
							"Node %s: interface %s reported holdover state %s on a no holdover profile",
							nodeName, status.Interface, status.State)
					}
				}
			})

		// Case 08: To verify every downstream port is programmed to distribute time
		// Test_Description: Verify ptp4l instantiated every port the TimeTransmitter profile declares,
		// that each one belongs to the boundary clock and carries the announce, sync and delay request
		// settings of the profile, and that every cabled port has taken the MASTER role.
		It("Case 08: To verify every downstream port is programmed to distribute time",
			reportxml.ID("99988"), func() {
				for _, nodeName := range ptpNodes {
					transmitter := ttSettings[nodeName]

					By(fmt.Sprintf("Verify ptp4l instantiated the %d ports profile %s declares on node %s",
						len(transmitter.Ports), transmitter.Profile, nodeName))

					daemonPod, err := sysptp.GetLinuxptpDaemonPodOnNode(APIClient, nodeName)
					Expect(err).ToNot(HaveOccurred(), "Failed to get PTP daemon pod on node %s", nodeName)

					receiverConfig, _, err := sysptp.Ptp4lInstanceForPort(daemonPod, tbcSettings[nodeName].UpstreamPort)
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					transmitterConfig, found := gnrdBCTransmitterConfig(daemonPod, receiverConfig)
					Expect(found).To(BeTrue(),
						"Node %s: profile %s declares %d downstream ports but no TimeTransmitter ptp4l "+
							"instance is running", nodeName, transmitter.Profile, len(transmitter.Ports))

					portData, err := sysptp.RunPmc(daemonPod, transmitterConfig,
						sysptp.Ptp4lSocketForConfig(transmitterConfig), "GET PORT_DATA_SET")
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					dataSets := sysptp.ParsePmcPortDataSets(portData)
					Expect(dataSets).To(HaveLen(len(transmitter.Ports)),
						"Node %s: profile %s declares %d downstream ports but ptp4l instantiated %d. "+
							"A missing port usually means the interface does not exist on the host",
						nodeName, transmitter.Profile, len(transmitter.Ports), len(dataSets))

					By("Verify every port belongs to the boundary clock with a contiguous port number")

					// ptp4l numbers ports in config declaration order, so entry N is transmitter.Ports[N].
					clockIdentity := dataSets[0].ClockIdentity

					for idx, dataSet := range dataSets {
						portName := transmitter.Ports[idx]

						Expect(dataSet.ClockIdentity).To(Equal(clockIdentity),
							"Node %s: port %s reports clock identity %s, expected %s. Every downstream port "+
								"must announce the same boundary clock",
							nodeName, portName, dataSet.ClockIdentity, clockIdentity)
						Expect(dataSet.PortNumber).To(Equal(idx+1),
							"Node %s: port %s has number %d, expected %d. ptp4l numbers ports in "+
								"declaration order, a gap means a port failed to initialize",
							nodeName, portName, dataSet.PortNumber, idx+1)
					}

					By("Verify every port carries the announce, sync and delay request settings of the profile")

					for idx, dataSet := range dataSets {
						portName := transmitter.Ports[idx]

						// Without this the loop below would assert nothing if the profile were parsed
						// into an empty settings map, and the case would pass while checking nothing.
						Expect(transmitter.PortSettings[portName]).ToNot(BeEmpty(),
							"Node %s: profile %s programs no announce, sync or delay request settings "+
								"for port %s", nodeName, transmitter.Profile, portName)

						for field, expected := range transmitter.PortSettings[portName] {
							actual, reported := dataSet.Fields[field]
							Expect(reported).To(BeTrue(),
								"Node %s: port %s does not report %s", nodeName, portName, field)
							Expect(actual).To(Equal(expected),
								"Node %s: port %s is programmed with %s %d but profile %s declares %d. "+
									"The port would distribute time at the wrong rate",
								nodeName, portName, field, actual, transmitter.Profile, expected)
						}
					}

					By("Verify every cabled port has taken the MASTER role")

					carriers, err := sysptp.CarrierStates(APIClient, nodeName)
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					var cabled, uncabled []string

					for idx, dataSet := range dataSets {
						portName := transmitter.Ports[idx]

						if !carriers[portName] {
							uncabled = append(uncabled, portName)

							continue
						}

						cabled = append(cabled, portName)

						Expect(dataSet.State).To(Equal(gnrdBCPortStateMaster),
							"Node %s: port %s has carrier but reports %s, expected %s. A cabled downstream "+
								"port that is not %s is not distributing time",
							nodeName, portName, dataSet.State, gnrdBCPortStateMaster, gnrdBCPortStateMaster)
					}

					klog.V(randuparams.RanDuLogLevel).Infof(
						"Node %s: %d/%d downstream ports are cabled and MASTER: %v. Ports without carrier "+
							"(programmed but not distributing time): %v",
						nodeName, len(cabled), len(dataSets), cabled, uncabled)
				}
			})

		// Case 07: To verify the clock free-runs instead of holding over when the upstream is lost
		// Test_Description: Take the upstream TimeReceiver port down and verify the clock drops
		// straight to free-run and advertises clock class 248, then recovers once the port is back.
		It("Case 07: To verify the clock free-runs instead of holding over when the upstream is lost",
			reportxml.ID("99987"), func() {
				if !RanDuTestConfig.PtpTBCDisruptive {
					Skip("Case 07 is disruptive, set ptp_tbc_disruptive (ECO_RANDU_PTP_TBC_DISRUPTIVE) to enable it")
				}

				for _, nodeName := range ptpNodes {
					nodeName := nodeName
					settings := tbcSettings[nodeName]
					upstreamPort := settings.UpstreamPort

					By(fmt.Sprintf("Verify the clock state after upstream port %s is lost on node %s",
						upstreamPort, nodeName))

					DeferCleanup(func() {
						By("Restoring the upstream port")

						restoreCmd := fmt.Sprintf("ip link set dev %s up", sysptp.ShellQuoteArg(upstreamPort))
						if _, restoreErr := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, restoreCmd); restoreErr != nil {
							klog.Errorf("Failed to restore upstream port %s on node %s: %v",
								upstreamPort, nodeName, restoreErr)
						}
					})

					By("Taking the upstream port down")

					lossTime := time.Now()

					downCmd := fmt.Sprintf("ip link set dev %s down", sysptp.ShellQuoteArg(upstreamPort))
					_, err := sysptp.ExecCmdOnNodeHost(APIClient, nodeName, downCmd)
					Expect(err).ToNot(HaveOccurred(),
						"Failed to take upstream port %s down on node %s", upstreamPort, nodeName)

					By("Verifying the clock drops to free-run")

					var lossLogs string

					err = wait.PollUntilContextTimeout(
						context.TODO(), gnrdBCStatePollInterval, gnrdBCStateChangeTimeout, true,
						func(ctx context.Context) (bool, error) {
							logs, logErr := gnrdBCLogsSince(nodeName, lossTime)
							if logErr != nil {
								return false, nil
							}

							lossLogs = logs

							for _, status := range sysptp.ParseTBCStatusLines(logs, settings.LeadingInterface) {
								if status.State == sysptp.StateFreeRun {
									return true, nil
								}
							}

							return false, nil
						})
					Expect(err).ToNot(HaveOccurred(),
						"Node %s: timeout waiting for state %s after upstream loss on %s",
						nodeName, sysptp.StateFreeRun, upstreamPort)

					By("Verifying the clock never passed through holdover")

					Expect(lossLogs).ToNot(ContainSubstring(sysptp.DPLLHoldoverLogMark),
						"Node %s: the dpll entered holdover, expected free-run on a no holdover profile", nodeName)

					for _, status := range sysptp.ParseTBCStatusLines(lossLogs, "") {
						Expect(status.State).ToNot(Equal(sysptp.StateHoldover),
							"Node %s: interface %s reported holdover state after upstream loss, expected free-run",
							nodeName, status.Interface)
					}

					By("Verifying the downstream ports advertise the free-run clock class")

					daemonPod, err := sysptp.GetLinuxptpDaemonPodOnNode(APIClient, nodeName)
					Expect(err).ToNot(HaveOccurred(), "Failed to get PTP daemon pod on node %s", nodeName)

					receiverConfig, _, err := sysptp.Ptp4lInstanceForPort(daemonPod, upstreamPort)
					Expect(err).ToNot(HaveOccurred(), "Node %s: %v", nodeName, err)

					if transmitterConfig, found := gnrdBCTransmitterConfig(daemonPod, receiverConfig); found {
						Eventually(func() (int, error) {
							defaultData, pmcErr := sysptp.RunPmc(daemonPod, transmitterConfig,
								sysptp.Ptp4lSocketForConfig(transmitterConfig), "GET DEFAULT_DATA_SET")
							if pmcErr != nil {
								return 0, pmcErr
							}

							return sysptp.PmcFieldInt(defaultData, "clockClass")
						}).WithTimeout(gnrdBCStateChangeTimeout).WithPolling(gnrdBCStatePollInterval).
							Should(Equal(gnrdBCFreerunClockClass),
								"Node %s: downstream clockClass did not fall back to %d after upstream loss",
								nodeName, gnrdBCFreerunClockClass)
					}

					By("Restoring the upstream port")

					restoreTime := time.Now()

					upCmd := fmt.Sprintf("ip link set dev %s up", sysptp.ShellQuoteArg(upstreamPort))
					_, err = sysptp.ExecCmdOnNodeHost(APIClient, nodeName, upCmd)
					Expect(err).ToNot(HaveOccurred(),
						"Failed to restore upstream port %s on node %s", upstreamPort, nodeName)

					By("Verifying the clock locks back to the upstream grandmaster")

					// The most recent T-BC-STATUS of the leading interface is the only reliable signal
					// here. Other cards keep logging s2 through the whole outage, so searching the log
					// for a bare state mark would report a re-lock that has not happened.
					err = wait.PollUntilContextTimeout(
						context.TODO(), gnrdBCStatePollInterval, gnrdBCStateChangeTimeout, true,
						func(ctx context.Context) (bool, error) {
							logs, logErr := gnrdBCLogsSince(nodeName, restoreTime)
							if logErr != nil {
								return false, nil
							}

							state, found := sysptp.LatestTBCState(logs, settings.LeadingInterface)

							return found && state == sysptp.StateLocked, nil
						})
					Expect(err).ToNot(HaveOccurred(),
						"Node %s: timeout waiting for sync state %s after upstream port %s was restored",
						nodeName, sysptp.StateLocked, upstreamPort)

					By("Verifying the whole outage passed without a holdover state")

					// lossLogs only covers up to the moment free-run was detected. Re-read the full
					// episode so the no holdover claim covers every sample from loss to re-lock.
					episodeLogs, err := gnrdBCLogsSince(nodeName, lossTime)
					Expect(err).ToNot(HaveOccurred(), "Failed to get PTP daemon logs on node %s", nodeName)

					Expect(episodeLogs).ToNot(ContainSubstring(sysptp.DPLLHoldoverLogMark),
						"Node %s: the dpll entered holdover during the outage, expected free-run", nodeName)

					for _, status := range sysptp.ParseTBCStatusLines(episodeLogs, "") {
						Expect(status.State).ToNot(Equal(sysptp.StateHoldover),
							"Node %s: interface %s reported holdover during the outage, expected free-run",
							nodeName, status.Interface)
					}
				}
			})
	})

// gnrdBCTransmitterConfig returns the ptp4l config of the TimeTransmitter instance, the one that is
// not the TimeReceiver instance identified by receiverConfig.
func gnrdBCTransmitterConfig(daemonPod *pod.Builder, receiverConfig string) (string, bool) {
	configs, err := sysptp.Ptp4lConfigs(daemonPod)
	if err != nil {
		return "", false
	}

	for _, config := range configs {
		if config != receiverConfig {
			return config, true
		}
	}

	return "", false
}

// gnrdBCLogsSince returns the linuxptp daemon log of a node written after since.
func gnrdBCLogsSince(nodeName string, since time.Time) (string, error) {
	daemonPod, err := sysptp.GetLinuxptpDaemonPodOnNode(APIClient, nodeName)
	if err != nil {
		return "", err
	}

	logs, err := daemonPod.GetLogsWithOptions(&corev1.PodLogOptions{
		Container: sysptp.DaemonContainerName,
		SinceTime: &metav1.Time{Time: since},
	})
	if err != nil {
		return "", err
	}

	return string(logs), nil
}
