package ptp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/nodes"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	infraptp "github.com/rh-ecosystem-edge/eco-goinfra/pkg/ptp"
	ptpv1 "github.com/rh-ecosystem-edge/eco-goinfra/pkg/schemes/ptp/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// ptpSettings keys the PTP operator publishes on telecom boundary clock profiles.
const (
	// SettingClockType holds the clock role of a profile, "T-BC" for a telecom boundary clock.
	SettingClockType = "clockType"
	// SettingLeadingInterface holds the PHC the other cards and CLOCK_REALTIME are synchronized from.
	SettingLeadingInterface = "leadingInterface"
	// SettingUpstreamPort holds the port that receives PTP from the upstream grandmaster.
	SettingUpstreamPort = "upstreamPort"
	// ClockTypeTBC is the SettingClockType value of a telecom boundary clock.
	ClockTypeTBC = "T-BC"
)

// Intel e810/e825/e830 plugin settings that govern holdover behaviour.
const (
	// PluginSettingLocalHoldoverTimeout is the holdover window in seconds, 0 on a no holdover profile.
	PluginSettingLocalHoldoverTimeout = "LocalHoldoverTimeout"
	// PluginSettingMaxInSpecOffset is the in-spec offset threshold in nanoseconds.
	PluginSettingMaxInSpecOffset = "MaxInSpecOffset"
)

// Log marks emitted by the linuxptp daemon telecom boundary clock state machine.
const (
	// TBCStatusLogMark prefixes the composite T-BC state line.
	TBCStatusLogMark = "T-BC-STATUS"
	// BCStateLogMark prefixes the boundary clock state transitions.
	BCStateLogMark = "current BC state:"
	// SourceNotLostLogMark is logged while neither ptp4l nor the DPLL has lost the time source.
	SourceNotLostLogMark = "Source NOT LOST"
	// OnHoldoverFalseLogMark is logged by the DPLL event handler while the clock is not in holdover.
	OnHoldoverFalseLogMark = "On holdover false"
	// InSpecTrueLogMark is logged while the DPLL offset stays inside MaxInSpecOffset.
	InSpecTrueLogMark = "In spec true"
	// DPLLHoldoverLogMark is the DPLL state reported once the clock enters holdover.
	DPLLHoldoverLogMark = "DPLL_HOLDOVER"
)

// Clock states reported by linuxptp: s0 free-run, s1 holdover, s2 locked.
const (
	// StateFreeRun is the clock state of a free-running clock.
	StateFreeRun = "s0"
	// StateHoldover is the clock state of a clock running on holdover.
	StateHoldover = "s1"
	// StateLocked is the clock state of a clock locked to its time source.
	StateLocked = "s2"
)

var (
	// MasterOffsetRegexp matches ptp4l "master offset" summary lines, capturing the offset in nanoseconds.
	MasterOffsetRegexp = regexp.MustCompile(`ptp4l\[[^\]]*\]:\s*\[[^\]]*\]\s*master offset\s+(-?\d+)`)
	// RealtimeOffsetRegexp matches phc2sys CLOCK_REALTIME lines, capturing the offset in nanoseconds.
	RealtimeOffsetRegexp = regexp.MustCompile(
		`phc2sys\[[^\]]*\]:\s*\[[^\]]*\]\s*CLOCK_REALTIME phc offset\s+(-?\d+)`)

	dpllStatusRegexp = regexp.MustCompile(
		`dpll\[[^\]]*\]:\[[^\]]*\]\s*(\S+)\s+frequency_status\s+(\d+)\s+offset\s+(-?\d+)` +
			`\s+phase_status\s+(\d+)\s+pps_status\s+(\d+)\s+(s\d)`)
	tbcStatusRegexp = regexp.MustCompile(
		`T-BC\[[^\]]*\]:\[[^\]]*\]\s*(\S+)\s+offset\s+(-?\d+)\s+T-BC-STATUS\s+(s\d)`)
)

// LatestPtp4lState returns the sync state of the most recent ptp4l summary line belonging to
// configPath. Other linuxptp instances log their own states in the same stream, so callers must
// scope the lookup to one config rather than searching the log for a bare state mark.
func LatestPtp4lState(logText, configPath string) (string, bool) {
	stateRegexp := regexp.MustCompile(
		`ptp4l\[[^\]]*\]:\s*\[` + regexp.QuoteMeta(pathBase(configPath)) +
			`:\d+\]\s*master offset\s+-?\d+\s+(s\d)`)

	matches := stateRegexp.FindAllStringSubmatch(logText, -1)
	if len(matches) == 0 {
		return "", false
	}

	return matches[len(matches)-1][1], true
}

// pathBase returns the file name of a path, linuxptp logs configs by base name.
func pathBase(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}

	return path
}

// TTSettings describes the TimeTransmitter profile applied to a node, the profile that programs
// the downstream ports distributing time to the network.
type TTSettings struct {
	// Profile is the name of the TimeTransmitter profile.
	Profile string
	// Ports are the interface sections declared in ptp4lConf, in declaration order. ptp4l numbers
	// its ports in that same order, so index i corresponds to port number i+1.
	Ports []string
	// PortSettings maps each declared port to the pmc PORT_DATA_SET fields the profile programs,
	// with the [global] values applied first and any per-port section overriding them.
	PortSettings map[string]map[string]int
}

// ptp4lConfToPmcField maps the ptp4l config keys that program a port to their PORT_DATA_SET names.
var ptp4lConfToPmcField = map[string]string{
	"logAnnounceInterval":     "logAnnounceInterval",
	"logSyncInterval":         "logSyncInterval",
	"logMinDelayReqInterval":  "logMinDelayReqInterval",
	"logMinPdelayReqInterval": "logMinPdelayReqInterval",
	"announceReceiptTimeout":  "announceReceiptTimeout",
	"delay_mechanism":         "delayMechanism",
}

// delayMechanismValues maps the ptp4l delay_mechanism names to the integers pmc reports.
var delayMechanismValues = map[string]int{"E2E": 1, "P2P": 2}

// GetTTSettings resolves the TimeTransmitter profile of a node. It is the boundary clock profile
// that declares no upstreamPort, the downstream counterpart of the profile GetTBCSettings returns.
func GetTTSettings(apiClient *clients.Settings, nodeName string) (TTSettings, error) {
	var settings TTSettings

	profiles, err := ProfilesForNode(apiClient, nodeName)
	if err != nil {
		return settings, err
	}

	for i := range profiles {
		profile := &profiles[i]

		if strings.TrimSpace(profile.PtpSettings[SettingUpstreamPort]) != "" {
			continue
		}

		if profile.Ptp4lConf == nil {
			continue
		}

		ports, portSettings := parsePtp4lConfPorts(*profile.Ptp4lConf)
		if len(ports) == 0 {
			continue
		}

		settings.Ports = ports
		settings.PortSettings = portSettings

		if profile.Name != nil {
			settings.Profile = *profile.Name
		}

		return settings, nil
	}

	return settings, fmt.Errorf(
		"no PtpConfig profile applied to node %s declares downstream ports without an %q ptpSetting",
		nodeName, SettingUpstreamPort)
}

// parsePtp4lConfPorts returns the interface sections of a ptp4l config together with the
// PORT_DATA_SET values each one is programmed with. Commented sections are ignored, so a port
// parked behind a '#' is not counted as declared.
func parsePtp4lConfPorts(conf string) ([]string, map[string]map[string]int) {
	var (
		ports         []string
		globalValues  = make(map[string]int)
		sectionValues = make(map[string]map[string]int)
		section       string
	)

	for _, raw := range strings.Split(conf, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section != "global" && section != "unicast_master_table" {
				ports = append(ports, section)
				sectionValues[section] = make(map[string]int)
			}

			continue
		}

		key, value, ok := ptp4lConfSetting(line)
		if !ok {
			continue
		}

		switch {
		case section == "global":
			globalValues[key] = value
		case sectionValues[section] != nil:
			sectionValues[section][key] = value
		}
	}

	portSettings := make(map[string]map[string]int, len(ports))

	for _, port := range ports {
		merged := make(map[string]int, len(globalValues))
		for key, value := range globalValues {
			merged[key] = value
		}

		for key, value := range sectionValues[port] {
			merged[key] = value
		}

		portSettings[port] = merged
	}

	return ports, portSettings
}

// ptp4lConfSetting parses one "key value" line into its PORT_DATA_SET field name and value.
func ptp4lConfSetting(line string) (string, int, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}

	field, tracked := ptp4lConfToPmcField[fields[0]]
	if !tracked {
		return "", 0, false
	}

	if mechanism, isMechanism := delayMechanismValues[strings.ToUpper(fields[1])]; isMechanism {
		return field, mechanism, true
	}

	value, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, false
	}

	return field, value, true
}

// NodeTBCProfile bundles the two halves of the boundary clock configuration applied to a node.
type NodeTBCProfile struct {
	// Receiver is the profile owning the upstream port that takes time from the grandmaster.
	Receiver TBCSettings
	// Transmitter is the profile programming the downstream ports that distribute time.
	Transmitter TTSettings
}

// ResolveNoHoldoverTBC returns the boundary clock profiles applied to a node together with the
// reason the node is not running a telecom boundary clock without holdover. The reason is empty
// when it is, and is written to be shown directly in a skip message.
//
// Callers should treat a non-empty reason as "this suite does not apply here" rather than as a
// failure: a profile with a holdover window is a legitimate configuration, just not this one, and
// asserting no holdover behaviour against it would report false failures.
func ResolveNoHoldoverTBC(apiClient *clients.Settings, nodeName string) (NodeTBCProfile, string) {
	var profile NodeTBCProfile

	receiver, err := GetTBCSettings(apiClient, nodeName)
	if err != nil {
		return profile, fmt.Sprintf("no boundary clock PtpConfig profile applies to it (%v)", err)
	}

	profile.Receiver = receiver

	if reason := receiverNoHoldoverReason(receiver); reason != "" {
		return profile, reason
	}

	// A clock with an upstream port but no downstream ports is a time receiver, not a boundary clock.
	transmitter, err := GetTTSettings(apiClient, nodeName)
	if err != nil {
		return profile, fmt.Sprintf(
			"no profile declares downstream ports, so it is a time receiver rather than a boundary "+
				"clock (%v)", err)
	}

	profile.Transmitter = transmitter

	return profile, transmitterBoundaryClockReason(transmitter)
}

// receiverNoHoldoverReason reports why a receiver profile is not a telecom boundary clock
// configured without holdover, or an empty string when it is.
func receiverNoHoldoverReason(receiver TBCSettings) string {
	if receiver.ClockType != ClockTypeTBC {
		return fmt.Sprintf("profile %s declares clockType %q, not %q",
			receiver.ReceiverProfile, receiver.ClockType, ClockTypeTBC)
	}

	if strings.TrimSpace(receiver.UpstreamPort) == "" {
		return fmt.Sprintf("profile %s declares no %s, so it takes time from no upstream clock",
			receiver.ReceiverProfile, SettingUpstreamPort)
	}

	// Distinguish "explicitly configured for no holdover" from "never configured at all". Both
	// leave LocalHoldoverTimeout at 0, but only the first is the profile this suite validates.
	if !receiver.Holdover.Found {
		return fmt.Sprintf(
			"profile %s carries no Intel plugin %s setting, so no holdover behaviour is not configured, "+
				"only unset", receiver.ReceiverProfile, PluginSettingLocalHoldoverTimeout)
	}

	if receiver.Holdover.LocalHoldoverTimeout != 0 {
		return fmt.Sprintf(
			"profile %s configures a %ds holdover window, this suite validates the no holdover profile",
			receiver.ReceiverProfile, receiver.Holdover.LocalHoldoverTimeout)
	}

	return ""
}

// transmitterBoundaryClockReason reports why a transmitter profile does not distribute time,
// or an empty string when it does.
func transmitterBoundaryClockReason(transmitter TTSettings) string {
	if len(transmitter.Ports) == 0 {
		return fmt.Sprintf(
			"profile %s declares no downstream ports, so it distributes time to nothing", transmitter.Profile)
	}

	return ""
}

// PmcPortDataSet is a single PORT_DATA_SET response returned by pmc.
type PmcPortDataSet struct {
	// PortIdentity is the raw identity, "<clockIdentity>-<portNumber>".
	PortIdentity string
	// ClockIdentity is the clock the port belongs to.
	ClockIdentity string
	// PortNumber is the 1-based index ptp4l assigned to the port.
	PortNumber int
	// State is the port state, MASTER, SLAVE, FAULTY and so on.
	State string
	// Fields holds the numeric PORT_DATA_SET values keyed by pmc field name.
	Fields map[string]int
}

// ParsePmcPortDataSets splits a pmc GET PORT_DATA_SET output into one entry per port.
func ParsePmcPortDataSets(pmcOutput string) []PmcPortDataSet {
	var (
		dataSets []PmcPortDataSet
		current  *PmcPortDataSet
	)

	for _, raw := range strings.Split(pmcOutput, "\n") {
		if strings.Contains(raw, "RESPONSE MANAGEMENT PORT_DATA_SET") {
			dataSets = append(dataSets, PmcPortDataSet{Fields: make(map[string]int)})
			current = &dataSets[len(dataSets)-1]

			continue
		}

		if current == nil {
			continue
		}

		fields := strings.Fields(raw)
		if len(fields) != 2 {
			continue
		}

		switch fields[0] {
		case "portIdentity":
			current.PortIdentity = fields[1]
			current.ClockIdentity, current.PortNumber = splitPortIdentity(fields[1])
		case "portState":
			current.State = fields[1]
		default:
			if value, err := strconv.Atoi(fields[1]); err == nil {
				current.Fields[fields[0]] = value
			}
		}
	}

	return dataSets
}

// splitPortIdentity splits "<clockIdentity>-<portNumber>" into its two parts.
func splitPortIdentity(identity string) (string, int) {
	idx := strings.LastIndex(identity, "-")
	if idx < 0 {
		return identity, 0
	}

	number, err := strconv.Atoi(identity[idx+1:])
	if err != nil {
		return identity[:idx], 0
	}

	return identity[:idx], number
}

// CarrierStates reports, for every interface on the node host, whether it currently has carrier.
// A port without carrier cannot reach a downstream clock however correctly it is programmed.
func CarrierStates(apiClient *clients.Settings, nodeName string) (map[string]bool, error) {
	out, err := ExecCmdOnNodeHost(apiClient, nodeName, "grep -H . /sys/class/net/*/carrier 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("failed to read carrier states on node %s: %w", nodeName, err)
	}

	carriers := make(map[string]bool)

	for _, line := range strings.Split(out, "\n") {
		// Each line reads "/sys/class/net/<iface>/carrier:<0|1>".
		path, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}

		iface := strings.TrimSuffix(strings.TrimPrefix(path, "/sys/class/net/"), "/carrier")
		if iface == "" || iface == path {
			continue
		}

		carriers[iface] = strings.TrimSpace(value) == "1"
	}

	return carriers, nil
}

// LatestTBCState returns the state of the most recent T-BC-STATUS line reported for iface.
func LatestTBCState(logText, iface string) (string, bool) {
	statuses := ParseTBCStatusLines(logText, iface)
	if len(statuses) == 0 {
		return "", false
	}

	return statuses[len(statuses)-1].State, true
}

// TBCSettings describes the telecom boundary clock topology of the PtpConfig profiles applied to a node.
type TBCSettings struct {
	// ReceiverProfile is the profile that owns the upstream TimeReceiver port.
	ReceiverProfile string
	// ClockType is the profile clockType, ClockTypeTBC for a telecom boundary clock.
	ClockType string
	// LeadingInterface is the PHC that drives the other cards and CLOCK_REALTIME.
	LeadingInterface string
	// UpstreamPort receives PTP from the upstream grandmaster.
	UpstreamPort string
	// Holdover carries the Intel plugin holdover settings of the receiver profile.
	Holdover HoldoverSettings
}

// HoldoverSettings holds the Intel e810/e825/e830 plugin settings that govern holdover behaviour.
type HoldoverSettings struct {
	// LocalHoldoverTimeout is the holdover window in seconds, 0 on a no holdover profile.
	LocalHoldoverTimeout int
	// MaxInSpecOffset is the in-spec offset threshold in nanoseconds.
	MaxInSpecOffset int
	// Found reports whether an Intel plugin with a settings block was present on the profile.
	Found bool
}

// DPLLStatus is a single dpll status line reported by the linuxptp daemon for a PHC interface.
type DPLLStatus struct {
	Interface       string
	FrequencyStatus int
	Offset          int
	PhaseStatus     int
	PPSStatus       int
	State           string
}

// TBCStatus is a single T-BC-STATUS line reported by the telecom boundary clock state machine.
type TBCStatus struct {
	Interface string
	Offset    int
	State     string
}

// ProfilesForNode returns every PtpConfig profile applied to nodeName, status match list first.
func ProfilesForNode(apiClient *clients.Settings, nodeName string) ([]ptpv1.PtpProfile, error) {
	ptpConfigs, err := infraptp.ListPtpConfigs(apiClient)
	if err != nil {
		return nil, fmt.Errorf("failed to list PtpConfigs: %w", err)
	}

	nodeBuilder, err := nodes.Pull(apiClient, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to pull node %s: %w", nodeName, err)
	}

	nodeLabels := nodeBuilder.Object.Labels

	var profiles []ptpv1.PtpProfile

	for _, cfg := range ptpConfigs {
		ptpConfig := cfg.Object
		if ptpConfig == nil {
			ptpConfig = cfg.Definition
		}

		if ptpConfig == nil {
			continue
		}

		for _, name := range profileNamesForNode(ptpConfig, nodeName, nodeLabels) {
			for i := range ptpConfig.Spec.Profile {
				if profile := ptpConfig.Spec.Profile[i]; profile.Name != nil && *profile.Name == name {
					profiles = append(profiles, profile)
				}
			}
		}
	}

	if len(profiles) == 0 {
		return nil, fmt.Errorf("no PtpConfig profile applies to node %s", nodeName)
	}

	return profiles, nil
}

// profileNamesForNode collects the profile names of a PtpConfig that apply to a node.
func profileNamesForNode(ptpConfig *ptpv1.PtpConfig, nodeName string, nodeLabels map[string]string) []string {
	var names []string

	seen := make(map[string]struct{})

	add := func(name string) {
		if _, ok := seen[name]; ok {
			return
		}

		seen[name] = struct{}{}
		names = append(names, name)
	}

	for _, match := range ptpConfig.Status.MatchList {
		if match.NodeName != nil && *match.NodeName == nodeName && match.Profile != nil {
			add(*match.Profile)
		}
	}

	for _, rec := range sortedRecommendations(ptpConfig) {
		if rec.Profile == nil {
			continue
		}

		for _, rule := range rec.Match {
			if rule.NodeName != nil && *rule.NodeName == nodeName {
				add(*rec.Profile)
			}

			if rule.NodeLabel != nil && nodeLabelMatches(*rule.NodeLabel, nodeLabels) {
				add(*rec.Profile)
			}
		}
	}

	return names
}

// GetTBCSettings resolves the boundary clock settings from the PtpConfig profiles applied to nodeName.
// The receiver profile is the one declaring an upstreamPort, the transmitter profile carries no such setting.
func GetTBCSettings(apiClient *clients.Settings, nodeName string) (TBCSettings, error) {
	var settings TBCSettings

	profiles, err := ProfilesForNode(apiClient, nodeName)
	if err != nil {
		return settings, err
	}

	for i := range profiles {
		profile := &profiles[i]

		upstreamPort := strings.TrimSpace(profile.PtpSettings[SettingUpstreamPort])
		if upstreamPort == "" {
			continue
		}

		holdover, err := PluginHoldoverSettings(profile.Plugins)
		if err != nil {
			return settings, err
		}

		settings.UpstreamPort = upstreamPort
		settings.ClockType = strings.TrimSpace(profile.PtpSettings[SettingClockType])
		settings.LeadingInterface = strings.TrimSpace(profile.PtpSettings[SettingLeadingInterface])
		settings.Holdover = holdover

		if profile.Name != nil {
			settings.ReceiverProfile = *profile.Name
		}

		return settings, nil
	}

	return settings, fmt.Errorf(
		"no PtpConfig profile applied to node %s declares the %q ptpSetting", nodeName, SettingUpstreamPort)
}

// PluginHoldoverSettings reads the holdover settings of the Intel plugin carried by a profile.
func PluginHoldoverSettings(plugins map[string]*apiextensions.JSON) (HoldoverSettings, error) {
	var settings HoldoverSettings

	for _, name := range []string{PluginNameE825, PluginNameE830, PluginNameE810} {
		raw, ok := plugins[name]
		if !ok || raw == nil {
			continue
		}

		var decoded struct {
			Settings map[string]json.Number `json:"settings"`
		}

		if err := json.Unmarshal(raw.Raw, &decoded); err != nil {
			return settings, fmt.Errorf("failed to decode %s plugin settings: %w", name, err)
		}

		if len(decoded.Settings) == 0 {
			continue
		}

		holdoverTimeout, found := numberSetting(decoded.Settings, PluginSettingLocalHoldoverTimeout)
		maxInSpecOffset, _ := numberSetting(decoded.Settings, PluginSettingMaxInSpecOffset)

		settings.LocalHoldoverTimeout = holdoverTimeout
		settings.MaxInSpecOffset = maxInSpecOffset
		settings.Found = found

		return settings, nil
	}

	return settings, nil
}

func numberSetting(settings map[string]json.Number, key string) (int, bool) {
	value, ok := settings[key]
	if !ok {
		return 0, false
	}

	parsed, err := value.Int64()
	if err != nil {
		return 0, false
	}

	return int(parsed), true
}

// Ptp4lConfigs returns the config paths of every ptp4l instance running inside the linuxptp daemon.
func Ptp4lConfigs(daemonPod *pod.Builder) ([]string, error) {
	buf, err := daemonPod.ExecCommand(
		[]string{"sh", "-c", "ls -1 /var/run/ptp4l.*.config 2>/dev/null || true"},
		DaemonContainerName,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list ptp4l configs: %w", err)
	}

	var configs []string

	for _, line := range strings.Split(buf.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			configs = append(configs, line)
		}
	}

	if len(configs) == 0 {
		return nil, fmt.Errorf("no ptp4l instance is running in the linuxptp daemon")
	}

	return configs, nil
}

// Ptp4lInstanceForPort returns the config and management socket of the ptp4l instance configured with portName.
func Ptp4lInstanceForPort(daemonPod *pod.Builder, portName string) (string, string, error) {
	if strings.TrimSpace(portName) == "" {
		return "", "", fmt.Errorf("portName cannot be empty")
	}

	// -F keeps the [portName] section header a literal, it is a character class to a regexp.
	lookup := fmt.Sprintf("grep -lF %s /var/run/ptp4l.*.config 2>/dev/null || true",
		ShellQuoteArg("["+portName+"]"))

	buf, err := daemonPod.ExecCommand([]string{"sh", "-c", lookup}, DaemonContainerName)
	if err != nil {
		return "", "", fmt.Errorf("failed to look up the ptp4l instance owning port %s: %w", portName, err)
	}

	configPath := strings.TrimSpace(strings.Split(strings.TrimSpace(buf.String()), "\n")[0])
	if configPath == "" {
		return "", "", fmt.Errorf("no ptp4l instance in the linuxptp daemon is configured with port %s", portName)
	}

	return configPath, Ptp4lSocketForConfig(configPath), nil
}

// Ptp4lSocketForConfig returns the management socket that belongs to a ptp4l config path.
func Ptp4lSocketForConfig(configPath string) string {
	return strings.TrimSuffix(configPath, ".config") + ".socket"
}

// RunPmc sends a pmc management query to one ptp4l instance of the linuxptp daemon.
// configPath supplies the domainNumber, without it pmc queries domain 0 and ptp4l never answers.
func RunPmc(daemonPod *pod.Builder, configPath, socketPath, query string) (string, error) {
	pmcCmd := fmt.Sprintf("pmc -u -b 0 -f %s -s %s %s 2>&1",
		ShellQuoteArg(configPath), ShellQuoteArg(socketPath), ShellQuoteArg(query))

	buf, err := daemonPod.ExecCommand([]string{"sh", "-c", pmcCmd}, DaemonContainerName)
	if err != nil {
		return "", fmt.Errorf("failed to run pmc %q on %s: %w", query, socketPath, err)
	}

	output := buf.String()
	if !strings.Contains(output, "RESPONSE MANAGEMENT") {
		return output, fmt.Errorf("pmc %q on %s returned no management response: %s", query, socketPath, output)
	}

	return output, nil
}

// PmcFieldValues returns every value reported for field in pmc output, in the order pmc printed them.
func PmcFieldValues(pmcOutput, field string) []string {
	var values []string

	for _, line := range strings.Split(pmcOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != field {
			continue
		}

		values = append(values, fields[1])
	}

	return values
}

// PmcFieldValue returns the first value reported for field in pmc output.
func PmcFieldValue(pmcOutput, field string) (string, bool) {
	values := PmcFieldValues(pmcOutput, field)
	if len(values) == 0 {
		return "", false
	}

	return values[0], true
}

// PmcFieldInt returns the first value reported for field in pmc output, parsed as an integer.
func PmcFieldInt(pmcOutput, field string) (int, error) {
	value, ok := PmcFieldValue(pmcOutput, field)
	if !ok {
		return 0, fmt.Errorf("field %s not found in pmc output", field)
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("failed to parse pmc field %s value %q: %w", field, value, err)
	}

	return parsed, nil
}

// OffsetSamplesWithinNS reports the number of offsets matched by offsetRe in logText and whether
// every one of them is within [-maxAbsNS, maxAbsNS].
func OffsetSamplesWithinNS(logText string, offsetRe *regexp.Regexp, maxAbsNS int) (int, bool, string) {
	if maxAbsNS < 0 {
		return 0, false, "invalid maxAbsNS: must be non-negative"
	}

	matches := offsetRe.FindAllStringSubmatch(logText, -1)
	if len(matches) == 0 {
		return 0, false, "no matching offset samples found in the PTP daemon logs"
	}

	for _, match := range matches {
		offset, err := strconv.Atoi(match[1])
		if err != nil {
			return len(matches), false, fmt.Sprintf("failed to parse offset value %q: %v", match[1], err)
		}

		if offset < -maxAbsNS || offset > maxAbsNS {
			return len(matches), false, fmt.Sprintf(
				"offset %d is outside ±%dns over %d samples", offset, maxAbsNS, len(matches))
		}
	}

	return len(matches), true, ""
}

// ParseDPLLStatusLines returns the dpll status lines of logText, restricted to iface when it is not empty.
func ParseDPLLStatusLines(logText, iface string) []DPLLStatus {
	var statuses []DPLLStatus

	for _, match := range dpllStatusRegexp.FindAllStringSubmatch(logText, -1) {
		if iface != "" && match[1] != iface {
			continue
		}

		frequencyStatus, phaseStatus, ppsStatus, offset := atoiOrSkip(match[2]), atoiOrSkip(match[4]),
			atoiOrSkip(match[5]), atoiOrSkip(match[3])

		statuses = append(statuses, DPLLStatus{
			Interface:       match[1],
			FrequencyStatus: frequencyStatus,
			Offset:          offset,
			PhaseStatus:     phaseStatus,
			PPSStatus:       ppsStatus,
			State:           match[6],
		})
	}

	return statuses
}

// ParseTBCStatusLines returns the T-BC-STATUS lines of logText, restricted to iface when it is not empty.
func ParseTBCStatusLines(logText, iface string) []TBCStatus {
	var statuses []TBCStatus

	for _, match := range tbcStatusRegexp.FindAllStringSubmatch(logText, -1) {
		if iface != "" && match[1] != iface {
			continue
		}

		statuses = append(statuses, TBCStatus{
			Interface: match[1],
			Offset:    atoiOrSkip(match[2]),
			State:     match[3],
		})
	}

	return statuses
}

func atoiOrSkip(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}

	return parsed
}
