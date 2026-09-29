//go:build unit_test

package ptp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// noHoldoverReceiver is a receiver profile that satisfies every precondition of the suite.
func noHoldoverReceiver() TBCSettings {
	return TBCSettings{
		ReceiverProfile:  "bc-no-holdover-tr",
		ClockType:        ClockTypeTBC,
		LeadingInterface: "eno1np0",
		UpstreamPort:     "eno2np3",
		Holdover:         HoldoverSettings{LocalHoldoverTimeout: 0, MaxInSpecOffset: 100, Found: true},
	}
}

func TestReceiverNoHoldoverReason(t *testing.T) {
	testCases := []struct {
		name     string
		mutate   func(*TBCSettings)
		accepted bool
		contains string
	}{
		{
			name:     "telecom boundary clock without holdover is accepted",
			mutate:   func(*TBCSettings) {},
			accepted: true,
		},
		{
			name:     "grandmaster clock type is rejected",
			mutate:   func(s *TBCSettings) { s.ClockType = "T-GM" },
			accepted: false,
			contains: "clockType",
		},
		{
			name:     "empty clock type is rejected",
			mutate:   func(s *TBCSettings) { s.ClockType = "" },
			accepted: false,
			contains: "clockType",
		},
		{
			name:     "missing upstream port is rejected",
			mutate:   func(s *TBCSettings) { s.UpstreamPort = "  " },
			accepted: false,
			contains: SettingUpstreamPort,
		},
		{
			// The decisive case: an unconfigured profile also reports a 0 timeout, so a bare
			// check on the value would accept a profile that never opted into no holdover.
			name:     "unset holdover plugin is rejected even though the timeout reads zero",
			mutate:   func(s *TBCSettings) { s.Holdover = HoldoverSettings{LocalHoldoverTimeout: 0, Found: false} },
			accepted: false,
			contains: "only unset",
		},
		{
			name:     "configured holdover window is rejected",
			mutate:   func(s *TBCSettings) { s.Holdover = HoldoverSettings{LocalHoldoverTimeout: 30, Found: true} },
			accepted: false,
			contains: "30s holdover window",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			receiver := noHoldoverReceiver()
			testCase.mutate(&receiver)

			reason := receiverNoHoldoverReason(receiver)

			if testCase.accepted {
				assert.Empty(t, reason)

				return
			}

			assert.NotEmpty(t, reason)
			assert.Contains(t, reason, testCase.contains)
		})
	}
}

func TestTransmitterBoundaryClockReason(t *testing.T) {
	assert.Empty(t, transmitterBoundaryClockReason(TTSettings{
		Profile: "bc-no-holdover-tt",
		Ports:   []string{"eno1np1", "eno1np2"},
	}))

	reason := transmitterBoundaryClockReason(TTSettings{Profile: "bc-no-holdover-tt"})
	assert.NotEmpty(t, reason)
	assert.Contains(t, reason, "no downstream ports")
}

func TestParsePtp4lConfPorts(t *testing.T) {
	conf := `
[global]
logAnnounceInterval -3
logSyncInterval -4
logMinDelayReqInterval -4
announceReceiptTimeout 3
delay_mechanism E2E
network_transport L2
# A port parked behind a comment is not declared.
#[eno1np0]
[eno1np1]
masterOnly 1
[eno1np2]
masterOnly 1
logSyncInterval -2
`

	ports, settings := parsePtp4lConfPorts(conf)

	assert.Equal(t, []string{"eno1np1", "eno1np2"}, ports, "commented sections must not be counted")

	assert.Equal(t, -4, settings["eno1np1"]["logSyncInterval"], "global value should apply")
	assert.Equal(t, -2, settings["eno1np2"]["logSyncInterval"], "per-port value should override global")
	assert.Equal(t, -3, settings["eno1np1"]["logAnnounceInterval"])
	assert.Equal(t, 3, settings["eno1np1"]["announceReceiptTimeout"])
	assert.Equal(t, 1, settings["eno1np1"]["delayMechanism"], "E2E maps to the 1 pmc reports")
	assert.NotContains(t, settings["eno1np1"], "network_transport", "untracked keys are ignored")
}

func TestParsePmcPortDataSets(t *testing.T) {
	pmcOutput := `sending: GET PORT_DATA_SET
	b47af1.fffe.d9437b-1 seq 0 RESPONSE MANAGEMENT PORT_DATA_SET
		portIdentity            b47af1.fffe.d9437b-1
		portState               FAULTY
		logAnnounceInterval     -3
		logSyncInterval         -4
		delayMechanism          1
	b47af1.fffe.d9437b-2 seq 0 RESPONSE MANAGEMENT PORT_DATA_SET
		portIdentity            b47af1.fffe.d9437b-2
		portState               MASTER
		logAnnounceInterval     -3
		logSyncInterval         -4
		delayMechanism          1
`

	dataSets := ParsePmcPortDataSets(pmcOutput)

	assert.Len(t, dataSets, 2)
	assert.Equal(t, "b47af1.fffe.d9437b", dataSets[0].ClockIdentity)
	assert.Equal(t, 1, dataSets[0].PortNumber)
	assert.Equal(t, "FAULTY", dataSets[0].State)
	assert.Equal(t, 2, dataSets[1].PortNumber)
	assert.Equal(t, "MASTER", dataSets[1].State)
	assert.Equal(t, -4, dataSets[1].Fields["logSyncInterval"])
}

func TestLatestPtp4lStateIsScopedToOneConfig(t *testing.T) {
	// Both instances log into the same stream. A bare " s2" search would report the receiver as
	// synced while it is still in s0.
	logText := `
ptp4l[100.1]: [ptp4l.1.config:6] master offset -2 s0 freq +8 path delay 212
ptp4l[100.2]: [ptp4l.0.config:6] master offset 1 s2 freq +9 path delay 210
ts2phc[100.3]: [ts2phc.1.config:6] enp108s0f0np0 offset 0 s2 freq +0
`

	state, found := LatestPtp4lState(logText, "/var/run/ptp4l.1.config")
	assert.True(t, found)
	assert.Equal(t, StateFreeRun, state, "receiver state must not be taken from the other instance")

	state, found = LatestPtp4lState(logText, "/var/run/ptp4l.0.config")
	assert.True(t, found)
	assert.Equal(t, StateLocked, state)

	_, found = LatestPtp4lState(logText, "/var/run/ptp4l.9.config")
	assert.False(t, found, "an instance that logged nothing must not be reported as synced")
}
