package server

import (
	"context"
	"log/slog"
	"testing"

	"github.com/skupperproject/skupper/cmd/network-observer/internal/api"
	"github.com/skupperproject/skupper/cmd/network-observer/internal/collector"
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
	"gotest.tools/v3/assert"
)

func TestProcesses(t *testing.T) {
	tlog := slog.Default()
	stor := store.NewSyncMapStore(store.SyncMapStoreConfig{Indexers: collector.RecordIndexers()})
	graph := collector.NewGraph(stor)
	srv, c := requireTestClient(t, New(tlog, stor, graph))
	defer srv.Close()

	testcases := []collectionTestCase[api.ProcessRecord]{
		{ExpectOK: true},
		{
			Records: wrapRecords( // filters processes with no sites
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("1"), Parent: ptrTo("s1")},
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("2"), Parent: ptrTo("s2")},
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("3")},
			),
			ExpectOK:    true,
			ExpectCount: 0,
		}, {
			Records: wrapRecords(
				vanflow.SiteRecord{BaseRecord: vanflow.NewBase("s1")},
				vanflow.SiteRecord{BaseRecord: vanflow.NewBase("s2")},
				vanflow.SiteRecord{BaseRecord: vanflow.NewBase("s3")},
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("1"), Parent: ptrTo("s1")},
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("2"), Parent: ptrTo("s2")},
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("3"), Parent: ptrTo("s3")},
			),
			ExpectOK:    true,
			ExpectCount: 3,
		}, {
			Records: wrapRecords(
				vanflow.SiteRecord{BaseRecord: vanflow.NewBase("s1")},
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("1"), Parent: ptrTo("s1")},
			),
			ExpectOK:    true,
			ExpectCount: 1,
			ExpectResults: func(t *testing.T, results []api.ProcessRecord) {
				r := results[0]
				assert.DeepEqual(t, r, api.ProcessRecord{
					Identity:      "1",
					SiteId:        "s1",
					SiteName:      "unknown",
					ComponentName: "unknown",
					ComponentId:   "unknown",
					Binding:       api.Unbound,
					Name:          "unknown",
					Role:          api.External,
					SourceHost:    "unknown",
				})
			},
		}, {
			Records:     exProcessWithAddresses(),
			ExpectOK:    true,
			ExpectCount: 1,
			ExpectResults: func(t *testing.T, results []api.ProcessRecord) {
				r := results[0]
				assert.DeepEqual(t, r, api.ProcessRecord{
					Identity:      "1",
					SiteId:        "site-1",
					SiteName:      "site one",
					Services:      ptrTo([]api.AtmarkDelimitedString{api.AtmarkDelimitedString("icecream@icecream-addr-id@tcp"), api.AtmarkDelimitedString("pizza@pizza-addr-id@tcp")}),
					ComponentName: "group-one",
					ComponentId:   "group-1-id",
					Binding:       api.Unbound,
					Name:          "processone",
					Role:          api.Internal,
					SourceHost:    "10.99.99.2",
				})
			},
		}, {
			Records: append(exProcessWithAddresses(), wrapRecords(
				vanflow.ListenerRecord{
					BaseRecord: vanflow.NewBase("l1"),
					Address:    ptrTo("pizza"),
					Protocol:   ptrTo("tcp"),
				},
			)...),
			ExpectOK:    true,
			ExpectCount: 1,
			ExpectResults: func(t *testing.T, results []api.ProcessRecord) {
				r := results[0]
				assert.DeepEqual(t, r, api.ProcessRecord{
					Identity:      "1",
					SiteId:        "site-1",
					SiteName:      "site one",
					Services:      ptrTo([]api.AtmarkDelimitedString{api.AtmarkDelimitedString("icecream@icecream-addr-id@tcp"), api.AtmarkDelimitedString("pizza@pizza-addr-id@tcp")}),
					ComponentName: "group-one",
					ComponentId:   "group-1-id",
					Binding:       api.Bound,
					Name:          "processone",
					Role:          api.Internal,
					SourceHost:    "10.99.99.2",
				})
			},
		}, {
			Records: append(exProcessWithAddresses(), wrapRecords(
				vanflow.ProcessRecord{BaseRecord: vanflow.NewBase("0"), Parent: ptrTo("site-1")},
				vanflow.ListenerRecord{
					BaseRecord: vanflow.NewBase("l1"),
					Address:    ptrTo("pizza"),
					Protocol:   ptrTo("tcp"),
				},
			)...),
			ExpectOK:    true,
			ExpectCount: 2,
			Parameters: map[string][]string{
				"sortBy": {"binding.asc"},
			},
			ExpectResults: func(t *testing.T, results []api.ProcessRecord) {
				assert.Equal(t, results[0].Identity, "1")
				assert.Equal(t, results[1].Identity, "0")
			},
		}, {
			Records: exProcessWithAddresses(),
			Parameters: map[string][]string{
				"services": {"pizza-addr-id"},
			},
			ExpectOK:    true,
			ExpectCount: 1,
		}, {
			Records: exProcessWithAddresses(),
			Parameters: map[string][]string{
				"services": {"gumbo"},
			},
			ExpectOK:    true,
			ExpectCount: 0,
		},
	}

	for _, tc := range testcases {
		t.Run("", func(t *testing.T) {
			stor.Replace(tc.Records)
			graph.(reset).Reset()
			resp, err := c.ProcessesWithResponse(context.TODO(), withParameters(tc.Parameters))
			assert.Check(t, err)
			if tc.ExpectOK {
				assert.Equal(t, resp.StatusCode(), 200)
				assert.Equal(t, resp.JSON200.Count, int64(tc.ExpectCount))
				assert.Equal(t, len(resp.JSON200.Results), tc.ExpectCount)
				if tc.ExpectTimeRangeCount != 0 {
					assert.Equal(t, resp.JSON200.TimeRangeCount, int64(tc.ExpectTimeRangeCount))
				}
				if tc.ExpectResults != nil {
					tc.ExpectResults(t, resp.JSON200.Results)
				}
			} else {
				assert.Check(t, resp.JSON400 != nil)
			}
		})
	}
}

func exProcessWithAddresses() []store.Entry {
	return wrapRecords(
		vanflow.ProcessRecord{
			Parent:     ptrTo("site-1"),
			Name:       ptrTo("processone"),
			BaseRecord: vanflow.NewBase("1"),
			Group:      ptrTo("group-one"),
			SourceHost: ptrTo("10.99.99.2"),
			Mode:       ptrTo("internal"),
		},
		collector.ProcessGroupRecord{ID: "group-1-id", Name: "group-one"},
		collector.AddressRecord{ID: "pizza-addr-id", Name: "pizza", Protocol: "tcp"},
		vanflow.ConnectorRecord{
			BaseRecord: vanflow.NewBase("c1"),
			Address:    ptrTo("pizza"),
			Protocol:   ptrTo("tcp"),
			ProcessID:  ptrTo("1"),
		},
		collector.AddressRecord{ID: "icecream-addr-id", Name: "icecream", Protocol: "tcp"},
		vanflow.RouterRecord{BaseRecord: vanflow.NewBase("router-1"), Parent: ptrTo("site-1")},
		vanflow.SiteRecord{BaseRecord: vanflow.NewBase("site-1"), Name: ptrTo("site one")},
		vanflow.ConnectorRecord{
			Parent:     ptrTo("router-1"),
			BaseRecord: vanflow.NewBase("c2"),
			Address:    ptrTo("icecream"),
			Protocol:   ptrTo("tcp"),
			DestHost:   ptrTo("10.99.99.2"),
		},
		vanflow.ConnectorRecord{
			Parent:     ptrTo("router-1"),
			BaseRecord: vanflow.NewBase("c3"),
			Address:    ptrTo("icecream"),
			Protocol:   ptrTo("tcp"),
			DestHost:   ptrTo("10.99.99.2"),
		},
	)
}
