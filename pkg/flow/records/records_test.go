// Code is generated. DO NOT EDIT

package records

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
		Map        map[any]any
	}{
		{
			Name:       "SiteRecord",
			RecordCode: site,
			Map: map[any]any{
				typeOfRecord: uint64(site), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(9):  "location-val",
				uint32(10): "provider-val",
				uint32(11): "platform-val",
				uint32(12): "namespace-val",
				uint32(52): "version-val",
				uint32(53): "policy-val",
			},
		},
		{
			Name:       "RouterRecord",
			RecordCode: router,
			Map: map[any]any{
				typeOfRecord: uint64(router), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(2):  "parent-val",
				uint32(12): "namespace-val",
				uint32(13): "mode-val",
				uint32(20): "imageName-val",
				uint32(21): "imageVersion-val",
				uint32(22): "hostname-val",
				uint32(30): "name-val",
				uint32(32): "buildVersion-val",
			},
		},
		{
			Name:       "LinkRecord",
			RecordCode: link,
			Map: map[any]any{
				typeOfRecord: uint64(link), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(2):  "parent-val",
				uint32(13): "mode-val",
				uint32(30): "name-val",
				uint32(33): uint64(133),
				uint32(34): "direction-val",
			},
		},
		{
			Name:       "ControllerRecord",
			RecordCode: controller,
			Map: map[any]any{
				typeOfRecord: uint64(controller), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(2):  "parent-val",
				uint32(20): "imageName-val",
				uint32(21): "imageVersion-val",
				uint32(22): "hostname-val",
				uint32(30): "name-val",
				uint32(32): "buildVersion-val",
			},
		},
		{
			Name:       "ListenerRecord",
			RecordCode: listener,
			Map: map[any]any{
				typeOfRecord: uint64(listener), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(2):  "parent-val",
				uint32(15): "destHost-val",
				uint32(16): "protocol-val",
				uint32(18): "destPort-val",
				uint32(19): "address-val",
				uint32(40): uint64(140),
				uint32(41): uint64(141),
				uint32(42): uint64(142),
				uint32(43): uint64(143),
			},
		},
		{
			Name:       "ConnectorRecord",
			RecordCode: connector,
			Map: map[any]any{
				typeOfRecord: uint64(connector), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(2):  "parent-val",
				uint32(15): "destHost-val",
				uint32(16): "protocol-val",
				uint32(18): "destPort-val",
				uint32(19): "address-val",
				uint32(40): uint64(140),
				uint32(41): uint64(141),
				uint32(42): uint64(142),
				uint32(43): uint64(143),
			},
		},
		{
			Name:       "FlowRecord",
			RecordCode: flow,
			Map: map[any]any{
				typeOfRecord: uint64(flow), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(2):  "parent-val",
				uint32(5):  "counterflow-val",
				uint32(14): "sourceHost-val",
				uint32(17): "sourcePort-val",
				uint32(23): uint64(123),
				uint32(24): uint64(124),
				uint32(29): "reason-val",
				uint32(31): "trace-val",
				uint32(35): uint64(135),
				uint32(36): uint64(136),
				uint32(37): uint64(137),
				uint32(38): uint64(138),
				uint32(39): uint64(139),
			},
		},
		{
			Name:       "ProcessRecord",
			RecordCode: process,
			Map: map[any]any{
				typeOfRecord: uint64(process), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(13): "mode-val",
				uint32(14): "sourceHost-val",
				uint32(20): "imageName-val",
				uint32(21): "imageVersion-val",
				uint32(22): "hostname-val",
				uint32(30): "name-val",
				uint32(46): "group-val",
			},
		},
		{
			Name:       "ImageRecord",
			RecordCode: image,
			Map: map[any]any{
				typeOfRecord: uint64(image), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "IngressRecord",
			RecordCode: ingress,
			Map: map[any]any{
				typeOfRecord: uint64(ingress), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "EgressRecord",
			RecordCode: egress,
			Map: map[any]any{
				typeOfRecord: uint64(egress), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "CollectorRecord",
			RecordCode: collector,
			Map: map[any]any{
				typeOfRecord: uint64(collector), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "ProcessGroupRecord",
			RecordCode: processGroup,
			Map: map[any]any{
				typeOfRecord: uint64(processGroup), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
			},
		},
		{
			Name:       "HostRecord",
			RecordCode: host,
			Map: map[any]any{
				typeOfRecord: uint64(host), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(10): "provider-val",
				uint32(30): "name-val",
			},
		},
		{
			Name:       "LogRecord",
			RecordCode: log,
			Map: map[any]any{
				typeOfRecord: uint64(log), identityAttr: "id", startTimeAttr: uint64(2), endTimeAttr: uint64(3),
				uint32(48): uint64(148),
				uint32(49): "logText-val",
				uint32(50): "sourceFile-val",
				uint32(51): uint64(151),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			r, err := recordDecoders[tc.RecordCode](tc.Map)
			assert.Assert(t, err)
			attrSet := make(map[any]any)
			for k, v := range r.Encode() {
				attrSet[k] = v
			}
			assert.DeepEqual(t, tc.Map, attrSet)
		})
	}
}
