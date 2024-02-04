// Code is generated. DO NOT EDIT

package v1

import (
	"testing"

	"gotest.tools/assert"
)

// TestRecordDecoders checks that a record that has been decoded then reencoded
// is identical to the original
func TestRecordDecoders(t *testing.T) {
	testCases := []struct {
		Name       string
		RecordCode recordType
		Map        map[uint32]any
	}{
		{
			Name:       "SiteRecord",
			RecordCode: site,
			Map: map[uint32]any{
				typeOfRecord: uint32(site), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				9:  "location-val",
				10: "provider-val",
				11: "platform-val",
				12: "namespace-val",
				52: "version-val",
				53: "policy-val",
			},
		},
		{
			Name:       "RouterRecord",
			RecordCode: router,
			Map: map[uint32]any{
				typeOfRecord: uint32(router), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				2:  "parent-val",
				12: "namespace-val",
				13: "mode-val",
				20: "imageName-val",
				21: "imageVersion-val",
				22: "hostname-val",
				30: "name-val",
				32: "buildVersion-val",
			},
		},
		{
			Name:       "LinkRecord",
			RecordCode: link,
			Map: map[uint32]any{
				typeOfRecord: uint32(link), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				2:  "parent-val",
				13: "mode-val",
				30: "name-val",
				33: uint64(133),
				34: "direction-val",
			},
		},
		{
			Name:       "ControllerRecord",
			RecordCode: controller,
			Map: map[uint32]any{
				typeOfRecord: uint32(controller), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				2:  "parent-val",
				20: "imageName-val",
				21: "imageVersion-val",
				22: "hostname-val",
				30: "name-val",
				32: "buildVersion-val",
			},
		},
		{
			Name:       "ListenerRecord",
			RecordCode: listener,
			Map: map[uint32]any{
				typeOfRecord: uint32(listener), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				2:  "parent-val",
				15: "destHost-val",
				16: "protocol-val",
				18: "destPort-val",
				19: "address-val",
				40: uint64(140),
				41: uint64(141),
				42: uint64(142),
				43: uint64(143),
			},
		},
		{
			Name:       "ConnectorRecord",
			RecordCode: connector,
			Map: map[uint32]any{
				typeOfRecord: uint32(connector), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				2:  "parent-val",
				15: "destHost-val",
				16: "protocol-val",
				18: "destPort-val",
				19: "address-val",
				40: uint64(140),
				41: uint64(141),
				42: uint64(142),
				43: uint64(143),
			},
		},
		{
			Name:       "FlowRecord",
			RecordCode: flow,
			Map: map[uint32]any{
				typeOfRecord: uint32(flow), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				2:  "parent-val",
				5:  "counterflow-val",
				14: "sourceHost-val",
				17: "sourcePort-val",
				23: uint64(123),
				24: uint64(124),
				29: "reason-val",
				31: "trace-val",
				35: uint64(135),
				36: uint64(136),
				37: uint64(137),
				38: uint64(138),
				39: uint64(139),
			},
		},
		{
			Name:       "ProcessRecord",
			RecordCode: process,
			Map: map[uint32]any{
				typeOfRecord: uint32(process), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				13: "mode-val",
				14: "sourceHost-val",
				20: "imageName-val",
				21: "imageVersion-val",
				22: "hostname-val",
				30: "name-val",
				46: "group-val",
			},
		},
		{
			Name:       "ImageRecord",
			RecordCode: image,
			Map: map[uint32]any{
				typeOfRecord: uint32(image), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "IngressRecord",
			RecordCode: ingress,
			Map: map[uint32]any{
				typeOfRecord: uint32(ingress), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "EgressRecord",
			RecordCode: egress,
			Map: map[uint32]any{
				typeOfRecord: uint32(egress), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "CollectorRecord",
			RecordCode: collector,
			Map: map[uint32]any{
				typeOfRecord: uint32(collector), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "ProcessGroupRecord",
			RecordCode: processGroup,
			Map: map[uint32]any{
				typeOfRecord: uint32(processGroup), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "HostRecord",
			RecordCode: host,
			Map: map[uint32]any{
				typeOfRecord: uint32(host), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				10: "provider-val",
				30: "name-val",
			},
		},
		{
			Name:       "LogRecord",
			RecordCode: log,
			Map: map[uint32]any{
				typeOfRecord: uint32(log), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				48: uint64(148),
				49: "logText-val",
				50: "sourceFile-val",
				51: uint64(151),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			r, err := recordDecoders[tc.RecordCode](tc.Map)
			assert.Assert(t, err)
			attrSet := make(map[uint32]any)
			for k, v := range r.Encode() {
				attrSet[k] = v
			}
			assert.DeepEqual(t, tc.Map, attrSet)
		})
	}
}
