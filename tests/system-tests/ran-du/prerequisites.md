# RAN DU System Tests — Prerequisites

What each test suite under `tests/system-tests/ran-du` needs from the cluster before it can run: operators,
hardware, storage, networking, node access and configuration. Every requirement below is derived from the test
code, not from the general RAN DU product documentation.

Suites skip or fail loudly when a prerequisite is missing. Where a suite skips cleanly, that is stated
explicitly; everything else fails.

## Contents

- [Baseline — required by every suite](#baseline--required-by-every-suite)
- [Suite matrix](#suite-matrix)
- [Per-suite prerequisites](#per-suite-prerequisites)
- [Appendix: quick prerequisite checks](#appendix-quick-prerequisite-checks)

---

## Baseline — required by every suite

These apply to the whole suite regardless of which label you select.

| Requirement | Detail |
|---|---|
| Cluster | OpenShift ≥ 4.13 with the RAN DU profile applied. SNO and multi-node are both supported; a few suites behave differently on SNO (noted per suite). |
| Credentials | `KUBECONFIG` pointing at the cluster, with **cluster-admin**. The suite creates and deletes namespaces, patches cluster-scoped resources and runs privileged pods. |
| Privileged workloads | `BeforeSuite` creates the namespace `ran-du-system-tests` labelled for privileged pod security. The namespace is deleted in `AfterSuite`. Pod Security admission must permit privileged pods there. |
| Node command execution | Most node-level commands are executed by `exec`-ing into the `machine-config-daemon` pods in `openshift-machine-config-operator` and running `chroot /rootfs`. Those pods must be running and healthy on every node under test. |
| Test runner host | The Ginkgo binary runs outside the cluster. Workload create/delete commands (`shell.ExecuteCmd`) run **on the runner host**, not in the cluster — the scripts they point at must exist there. |
| Feature selection | `ECO_TEST_FEATURES="ran-du"`, plus `ECO_TEST_LABELS` to narrow to one suite. |
| Reports | `ECO_REPORTS_DUMP_DIR` (default `/tmp/reports`) must be writable on the runner host. |

Suites that pull images onto cluster nodes (hard reboot) additionally need the nodes to reach the image
registry; see that suite below.

---

## Suite matrix

| Suite | Label | Disruptive | Headline prerequisite |
|---|---|---|---|
| [prerequisites.go](tests/prerequisites.go) | `Prerequisites` | no | Nothing — it is the read-only preflight for this document |
| [du-ztp-policies-compliance.go](tests/du-ztp-policies-compliance.go) | `ZTPPoliciesCompliance` | no | ACM/GitOps-ZTP `Policy` CRs in the cluster namespace |
| [sriovfecnodeconfig-status.go](tests/sriovfecnodeconfig-status.go) | `SriovFecNodeConfigStatus` | no | SR-IOV FEC operator + accelerator card on every node |
| [launch-workload.go](tests/launch-workload.go) | `launch-workload` | workload only | VDU workload emulator scripts + everything the workload needs |
| [launch-workload-multiple-iter-loadavg.go](tests/launch-workload-multiple-iter-loadavg.go) | `LaunchWorkloadMultipleIterations` | workload only | Same as above, repeated N times |
| [workload-guaranteed-force-delete.go](tests/workload-guaranteed-force-delete.go) | `WorkloadForceCleanup` | workload only | Workload must contain Guaranteed-QoS pods |
| [stability-workload.go](tests/stability-workload.go) | `StabilityWorkload` | no | Workload + long run window (default 30 min) |
| [stability-no-workload.go](tests/stability-no-workload.go) | `StabilityNoWorkload` | no | Long run window; no workload |
| [soft-reboot.go](tests/soft-reboot.go) | `SoftReboot` | **reboots all nodes** | Workload + SR-IOV; reboots every node, N iterations |
| [hard-reboot.go](tests/hard-reboot.go) | `HardReboot` | **power-cycles workers** | In-band IPMI on the node + ipmitool image |
| [kernel-crash-kdump.go](tests/kernel-crash-kdump.go) | `KernelCrashKdump` | **crashes all nodes** | kdump configured to write `/var/crash` |
| [nmi-kernel-crash-kdump.go](tests/nmi-kernel-crash-kdump.go) | `NMIKernelCrashKdump` | **crashes all nodes** | Redfish BMC credentials per node |
| [ptp-3wpc.go](tests/ptp-3wpc.go) | `ptp-3wpc` | case 07 only | 3 WPC (E810) cards with GNSS antennas, T-GM profile |
| [gnrd-bc-no-holdover.go](tests/gnrd-bc-no-holdover.go) | `PTPGNRDBCNoHoldoverValidation` | case 07 only (opt-in) | GNR-D T-BC profile with holdover disabled, upstream GM |
| [cert-manager.go](tests/cert-manager.go) | `cert-manager` | **replaces API/ingress certs** | cert-manager operator, ready ACME ClusterIssuer, DNS-01 |

"Workload only" means the suite deletes and re-creates the test workload namespace but does not touch the
cluster or node state.

---

## Per-suite prerequisites

### ZTP policy compliance — `ZTPPoliciesCompliance`

Asserts every ACM `Policy` is `Compliant`.

- **Cluster resources**: Open Cluster Management / RHACM `Policy` CRs (`policy.open-cluster-management.io`).
  The suite reads the cluster name from the `Infrastructure` CR (infrastructure name minus its last dash-separated
  segment) and lists policies in the namespace of that name — the managed-cluster namespace must follow that
  naming, or the list comes back empty and the test passes vacuously.
- **Network / storage**: none.
- **Configuration**: none.
- **Note**: this suite has no `reportxml.ID`, so it produces no Polarion test-case entry.

### SR-IOV FEC node config — `SriovFecNodeConfigStatus`

Waits (5 min, 30 s interval) until every node's `SriovFecNodeConfig` reports `Configured=True`.

- **Cluster resources**: SR-IOV FEC operator installed, with one `SriovFecNodeConfig` named after **each** node
  returned by a full node list — control-plane nodes included.
- **Hardware**: a supported vRAN accelerator (ACC100 / ACC200) on each of those nodes.
- **Configuration**: `ECO_SRIOV_FEC_OPERATOR_NAMESPACE` (default `vran-acceleration-operators`) must match where
  the operator is installed.
- **Fails, does not skip**, when the CR is missing on any node.
- **Note**: no `reportxml.ID`.

### Workload launch — `launch-workload` (ID 55465)

Deletes any existing workload, re-creates it, waits for all Deployments, StatefulSets and pods to be Ready, then
optionally validates PTP.

- **Runner host**: the create and delete scripts must exist and be executable there. Defaults:
  `/opt/vdu-workload-emulator/add_test-deployments.sh` and `.../delete_test-deployments.sh`. They run via
  `bash -c` with the runner's own `KUBECONFIG`.
- **Cluster resources**: whatever those manifests declare. For the standard VDU emulator that means:
  - a **StorageClass** able to bind the StatefulSet `volumeClaimTemplates` (a default StorageClass, or the one
    named in the manifests, with capacity available);
  - **SR-IOV networks and node policies** — `SriovNetwork` / `SriovNetworkNodePolicy` with VFs actually allocated
    on the physical functions the policies name, so the pods' `k8s.v1.cni.cncf.io/networks` annotations can be
    satisfied;
  - **hugepages** and **CPU manager** static policy on the target nodes if the pods request them;
  - the workload namespace (default `test`) must be creatable and deletable by the scripts.
- **Timing**: readiness timeout is 900 s per wait step. With `ECO_RANDU_PTP_ENABLED=true` the test additionally
  sleeps 6 minutes and then validates PTP, adding ~6 min.
- **PTP validation path** (shared by the reboot and stability suites) needs: `openshift-ptp` namespace with
  `linuxptp-daemon` pods, the daemon metrics endpoint on `localhost:9091` reporting
  `openshift_ptp_clock_state{iface="CLOCK_REALTIME",process="phc2sys"} 1`, and `timedatectl` on the node reporting
  `System clock synchronized: yes`. Set `ECO_RANDU_PTP_ENABLED=false` on clusters without PTP.

### Workload launch, multiple iterations — `LaunchWorkloadMultipleIterations` (ID 45698)

Same prerequisites as `launch-workload`, plus:

- **Iterations**: `ECO_RANDU_LAUNCH_WL_ITER` (default 5) full delete/create cycles. With PTP enabled each
  iteration includes a 6-minute wait, so budget ≈ 5 × (deploy time + 6 min) plus 5 min of load sampling.
- **Node load**: after the last iteration it samples `/proc/loadavg` 30 times at 10 s intervals on every node and
  requires the 1-minute load average to stay **below 100**. The sampling uses a privileged pod on each node.

### Guaranteed-QoS force delete — `WorkloadForceCleanup` (ID 74462)

Force-deletes every Guaranteed-QoS pod in the workload namespace, three times, and waits for recovery.

- Everything `launch-workload` needs, and in addition:
- **At least one pod with `status.qosClass == Guaranteed`** in the workload namespace. That requires integer CPU
  requests equal to limits and equal memory requests/limits — in practice the **CPU manager static policy** and a
  performance profile on the node. Without such pods the test deletes nothing and passes vacuously.
- **Recovery**: after each round all pods must become Ready again within 2 minutes, so the workload must be
  managed by Deployments/StatefulSets that reschedule immediately (sufficient free CPU/memory/hugepages to place
  the replacement pods).

### Stability with workload — `StabilityWorkload` (ID 42744)

Deploys the workload, then samples PTP status, policy compliance, pod restarts in `openshift-etcd` and
`openshift-apiserver`, and tuned restarts, for the configured duration; fails if any value changes.

- Everything `launch-workload` needs.
- **Output directory**: `ECO_RANDU_STABILITY_OUTPUT_PATH` (default `/tmp/reports`) must exist and be writable on
  the runner host — the suite writes several `.log` files there and re-reads them at the end.
- **Policy checks**: with `ECO_RANDU_STABILITY_POLICIES_CHECK=true` (default) the ACM policy prerequisite from
  `ZTPPoliciesCompliance` applies. Set it to `false` on clusters without ACM.
- **PTP**: sampled only when `ECO_RANDU_PTP_ENABLED=true`.
- **Duration**: `ECO_RANDU_STAB_W_DUR_MINS` (default 30) with `ECO_RANDU_STAB_W_INT_MINS` (default 5). The suite
  occupies the cluster for the whole window.

### Stability without workload — `StabilityNoWorkload` (ID 74522)

Identical to the above, minus the workload. Note it still calls the **delete** script in `BeforeAll` if the
workload namespace exists, so that script must be present on the runner host even though no workload is deployed.

### Soft reboot — `SoftReboot` (ID 42738)

`systemctl reboot` on every node, N iterations, validating workload, SR-IOV device passthrough and PTP after each.

- Everything `launch-workload` needs.
- **Node access**: `machine-config-daemon` pods available for the `chroot /rootfs systemctl reboot` call.
- **Cluster resources**:
  - the `apiserver` Deployment in `openshift-apiserver` must exist (pulled up front; must return to `Available`
    within 8 min after each reboot);
  - the **SR-IOV network operator** in `openshift-sriov-network-operator` — the test lists
    `SriovNetworkNodePolicy` and `SriovNetwork` objects and fails if that API is unavailable;
  - for every workload pod with `vfio-pci` network attachments, the count of devices under `/dev/vfio` inside the
    pod must be ≥ the number of attachments, so the VFs must be really bound to `vfio-pci`.
- **Iterations and timing**: `ECO_RANDU_SOFT_REBOOT_ITERATIONS` (default 5) × number of nodes. Each node costs a
  reboot + `ECO_RANDU_RECOVERY_TIME` minutes (default 2) + 6 min PTP wait when PTP is enabled. On a multi-node
  cluster this is a multi-hour run.
- **Disruption**: reboots **all** nodes, including control plane. Do not run against a shared cluster.

### Hard reboot — `HardReboot` (ID 42736)

`ipmitool chassis power cycle` on worker nodes, N iterations, with the same post-reboot checks as soft reboot.

- Everything `SoftReboot` needs, and in addition:
- **In-band IPMI**: the power cycle is issued from a privileged pod scheduled on the target node, running
  `ipmitool` with no host/user arguments — it uses the node's **local BMC interface**, so the `ipmi_devintf` /
  `/dev/ipmi0` kernel path must be available on the node. This is *not* the out-of-band credential map used by
  the NMI suite.
- **Image pull**: the ipmitool image (`ECO_SYSTEM_TESTS_IPMITOOL_IMAGE`, default a `quay.io/ocp-edge-qe` digest)
  must be pullable by the nodes; the suite also runs `podman rmi -f` for it on the node first.
- **Privileged SCC**: the default ServiceAccount in `ran-du-system-tests` is granted the privileged SCC by the
  helper.
- **Node selection**: only nodes labelled `node-role.kubernetes.io/worker`. On SNO the single node carries that
  label and is power-cycled.
- **Iterations**: `ECO_RANDU_HARD_REBOOT_ITERATIONS` (default 5).

### Kernel crash kdump — `KernelCrashKdump` (ID 56216)

Clears `/var/crash`, triggers `echo c > /proc/sysrq-trigger` on every node, waits for recovery, and asserts a dump
appeared.

- **kdump must be enabled and configured** on every node, with the crash kernel reserved and the dump target set
  to the local path `/var/crash`. A remote/NFS dump target makes the assertion fail.
- **sysrq**: the test enables it itself (`echo 1 > /proc/sys/kernel/sysrq`).
- **Node access**: `machine-config-daemon` pods, plus `oc debug`-style exec to read `/var/crash` afterwards.
- **Cluster resources**: the `apiserver` Deployment in `openshift-apiserver` must return to `Available` within
  5 min of each crash.
- **Timing**: iterates over **all** nodes, each costing a crash + dump write + `ECO_RANDU_RECOVERY_TIME` minutes.
- **Disruption**: hard-crashes every node in the cluster, one at a time.

### NMI kernel crash kdump — `NMIKernelCrashKdump` (ID 85975)

Same intent, but the crash is triggered out-of-band via Redfish NMI.

- **BMC credentials**: `ECO_RANDU_NODES_CREDENTIALS_MAP` must contain an entry for **every** node, format
  `node,user,pass,https://bmc:443` separated by `;`. **The suite skips cleanly when the map is empty**, but fails
  if it is set and a node is missing from it.
- **Redfish**: the BMC must expose the Redfish NMI action and be reachable from the **test runner host** (the HTTP
  call originates there, not from the cluster).
- **kdump**: same node-side requirement as the sysrq variant — `/var/crash` cleaned and then repopulated.
- **Timing**: per node, up to 25 min to go down and 25 min to come back, plus 5 min to verify the vmcore.
- **SNO**: detected automatically; the "node unavailable" check is adapted.

### PTP 3×WPC T-GM validation — `ptp-3wpc` (IDs 99991–99997)

Seven cases against a grandmaster built on three Westport Channel cards.

- **Skips the whole suite** when `ECO_RANDU_PTP_ENABLED=false`.
- **Hardware**: three E810 WPC NICs with GNSS antennas connected and locked, SMA/PPS cabling between the cards.
- **Cluster resources**: PTP operator in `openshift-ptp` with `linuxptp-daemon` pods; a `PtpConfig` using the
  Intel e810/e825/e830 plugin (case 05 skips without it, since it simulates GNSS loss through the plugin);
  `ts2phc`, `ptp4l` and `phc2sys` all running.
- **Node access**: the helpers exec into the `linuxptp-daemon` container and run host commands through a
  privileged pod (`nsenter --mount=/proc/1/ns/mnt`), so privileged pod creation must be allowed.
- **Configuration**:
  - `ECO_RANDU_PTP_WPC_SYNC_INTERFACES` — comma-separated PHC interfaces (inferred from the daemon log when empty);
  - `ECO_RANDU_PTP_WPC_PRIMARY_IFACE` — optional, defaults to the last interface above;
  - `ECO_RANDU_PTP_IPERF3_SERVER` — **case 06 skips without it**; needs a reachable iperf3 server, optionally with
    `ECO_RANDU_PTP_IPERF3_CLIENT_BIND` and `ECO_RANDU_PTP_IPERF3_DURATION_SEC` (default 300);
  - `ECO_RANDU_PTP_NETEM_INTERFACE` — **case 07 skips without it**; the named interface gets `tc netem` packet
    loss applied, so it must not carry cluster management traffic;
  - `ECO_RANDU_PTP_LOCKED_WAIT_SEC` — optional settling time before cases 06/07 (production runs use 1800+).

### GNR-D T-BC no-holdover validation — `PTPGNRDBCNoHoldoverValidation` (IDs 99981–99988)

Eight cases against a telecom boundary clock that has no GNSS and no holdover window.

- **Skips the whole suite** when `ECO_RANDU_PTP_ENABLED=false`.
- **Profile gate** — `BeforeAll` resolves the `PtpConfig` for each node and **skips** unless all of the following
  hold. This is deliberate: against a boundary clock that *does* have holdover, case 07 would report correct
  behaviour as a failure.
  - a TimeReceiver profile with `ptpSettings.clockType: T-BC`, a non-empty `upstreamPort` and a `leadingInterface`;
  - the Intel e825 plugin block present with `LocalHoldoverTimeout: 0` (an absent plugin block is rejected too — it
    also reads as zero but never opted into no-holdover);
  - a TimeTransmitter profile (no `upstreamPort`) whose `ptp4lConf` declares at least one downstream port.
- **Upstream**: a real PTP grandmaster reachable on the upstream port, delivering clock class 6 (locked and
  traceable) — cases 01, 02 and 07 assert against it.
- **Hardware**: GNR-D platform with an e825-based NIC; the leading interface must expose DPLL state
  (`phase_status`, `frequency_status`, `pps_status`) in the daemon log.
- **Downstream ports (case 08)**: `ptp4l` must instantiate exactly as many ports as the TT profile declares, with a
  shared clock identity and the profile's announce/sync/delay intervals. Ports without carrier are reported but
  not required to be `MASTER`, so uncabled downstream ports are acceptable.
- **Case 07 is opt-in and disruptive**: set `ECO_RANDU_PTP_TBC_DISRUPTIVE=true` to enable it. It takes the
  upstream PTP port administratively **down** for up to ~5 minutes to prove the clock free-runs instead of
  entering holdover, then brings it back up via `DeferCleanup`.
  **Verify first that the upstream port does not carry cluster traffic** — it must not be a `br-ex` member and must
  not hold the node IP or default route, or the node is cut off and cleanup cannot reach it.
- **Node access**: privileged pod creation, same as the 3WPC suite.
- **IDs 99981–99988 are placeholders** pending real Polarion IDs.

### cert-manager — `cert-manager` (IDs 89041–89048)

Operator verification, DNS-01 ACME issuance, Prometheus alert escalation/resolution, and API server plus ingress
certificate replacement and renewal.

- **Operators**: cert-manager operator installed, with pods running in `cert-manager-operator` and `cert-manager`,
  and the `Certificate` / `Issuer` / `ClusterIssuer` CRDs present.
- **Issuer**: a **Ready** `ClusterIssuer` named by `ECO_RANDU_CERTMANAGER_ISSUER_NAME` (default `acme-issuer`)
  configured for DNS-01. **The suite skips cleanly if it is not Ready.**
- **DNS**: a DNS server (`ECO_RANDU_CERTMANAGER_DNS_SERVER`) that the ACME solver can write TXT records to and
  that the runner can query to confirm cleanup; real ACME reachability from the cluster.
- **Domains**, all required for the full run:
  - `ECO_RANDU_CERTMANAGER_CERT_DOMAIN` — test certificate;
  - `ECO_RANDU_CERTMANAGER_API_DOMAIN` — API server certificate (must resolve to the API VIP);
  - `ECO_RANDU_CERTMANAGER_APPS_DOMAIN` — wildcard ingress certificate;
  - `ECO_RANDU_CERTMANAGER_INGRESS_IP` — used to connect to the router and inspect the served certificate.
- **Monitoring**: the suite labels the `cert-manager` namespace `openshift.io/cluster-monitoring=true`, creates a
  `PrometheusRule`, and queries Thanos through a ServiceAccount + ClusterRoleBinding it creates
  (`randu-prometheus-querier`). Cluster monitoring and user-workload metrics scraping must be functional.
- **Disruption**: it patches the `APIServer` and `IngressController` cluster configuration to use the issued
  certificates and waits for kube-apiserver and router rollouts, then restores them in `AfterAll`. Expect API and
  ingress restarts. **Do not run on a shared or production cluster.**
- **Timing**: certificate issuance plus several control-plane rollouts; budget well over an hour.

---

## Appendix: quick prerequisite checks

Most of what follows is automated by the `Prerequisites` suite
([tests/prerequisites.go](tests/prerequisites.go)), which reads cluster state only and changes
nothing:

```bash
export KUBECONFIG=/path/to/kubeconfig
export ECO_TEST_FEATURES="ran-du"
export ECO_TEST_LABELS="randu && Prerequisites"
make run-tests
```

Export the same `ECO_RANDU_*` variables the real run will use. The suite reports a missing
prerequisite the way the suite needing it behaves: a prerequisite whose absence makes a suite skip
cleanly is a skip here too, everything else is a failure. It cannot cover the workload manifests
themselves (hugepages, CPU manager, the `k8s.v1.cni.cncf.io/networks` annotations a given emulator
declares), the ACME and BMC endpoints reachable only at run time, or the Guaranteed-QoS check
before the workload is deployed.

The equivalent manual checks:

```bash
export KUBECONFIG=/path/to/kubeconfig

# Baseline: node count, readiness, MCO daemon pods used for node command execution
oc get nodes
oc -n openshift-machine-config-operator get pods -l k8s-app=machine-config-daemon

# Storage for workload StatefulSets
oc get storageclass
oc get pv

# SR-IOV: operator, policies, and VFs actually allocated
oc -n openshift-sriov-network-operator get sriovnetworknodepolicy,sriovnetwork
oc get sriovnetworknodestate -n openshift-sriov-network-operator -o yaml | grep -A3 numVfs

# SR-IOV FEC
oc get sriovfecnodeconfig -A

# PTP operator and profiles
oc -n openshift-ptp get pods -l app=linuxptp-daemon
oc -n openshift-ptp get ptpconfig -o yaml

# T-BC no-holdover gate (expect clockType T-BC and LocalHoldoverTimeout 0)
oc -n openshift-ptp get ptpconfig -o yaml | grep -E "clockType|upstreamPort|leadingInterface|LocalHoldoverTimeout"

# ACM policies for the ZTP compliance and stability suites
oc get policy -A

# cert-manager
oc get clusterissuer
oc -n cert-manager get pods

# Guaranteed-QoS pods in the workload namespace (for WorkloadForceCleanup)
oc -n test get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.qosClass}{"\n"}{end}'

# Runner-host side: workload scripts and report directory
ls -l /opt/vdu-workload-emulator/add_test-deployments.sh /opt/vdu-workload-emulator/delete_test-deployments.sh
ls -ld "${ECO_REPORTS_DUMP_DIR:-/tmp/reports}"
```

Before a disruptive PTP run, confirm the port that case 07 will take down is not carrying cluster traffic:

```bash
NODE=<node>; PORT=<upstreamPort from the PtpConfig>
oc debug node/$NODE -- chroot /host ip -br addr show dev $PORT
oc debug node/$NODE -- chroot /host ovs-vsctl list-ports br-ex
oc debug node/$NODE -- chroot /host ip route show default
```

The port must have no cluster IP, must not be a `br-ex` member, and must not appear in the default route.

---

See the [README](README.md) for the full environment-variable reference and example invocations, and
[CLAUDE.md](CLAUDE.md) for suite layout and conventions.
