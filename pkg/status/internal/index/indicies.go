package index

import (
	"github.com/c-kruse/vanflow"
	"github.com/c-kruse/vanflow/store"
)

const (
	ByParent  = "ByParent"
	ByAddress = "ByAddress"
	BySource  = "BySource"
	ByHost    = "ByProcessHost"
)

var (
	ByProcessHostIndexer = recordIndex(func(record *vanflow.ProcessRecord, _ store.Metadata) []string {
		var hosts []string
		if host := record.SourceHost; host != nil && *host != "" {
			hosts = append(hosts, *host)
		}
		if host := record.Hostname; host != nil && *host != "" {
			hosts = append(hosts, *host)
		}
		return hosts
	})
	ByRouterParentIndexer = recordIndex(func(record *vanflow.RouterRecord, _ store.Metadata) []string {
		if record.Parent != nil {
			return []string{*record.Parent}
		}
		return nil
	})
	ByLinkParentIndexer = recordIndex(func(record *vanflow.LinkRecord, _ store.Metadata) []string {
		if record.Parent != nil {
			return []string{*record.Parent}
		}
		return nil
	})
	ByConnectorParentIndexer = recordIndex(func(record *vanflow.ConnectorRecord, _ store.Metadata) []string {
		if record.Parent != nil {
			return []string{*record.Parent}
		}
		return nil
	})
	ByListenerParentIndexer = recordIndex(func(record *vanflow.ListenerRecord, _ store.Metadata) []string {
		if record.Parent != nil {
			return []string{*record.Parent}
		}
		return nil
	})
	ByProcessParentIndexer = recordIndex(func(record *vanflow.ProcessRecord, _ store.Metadata) []string {
		if record.Parent != nil {
			return []string{*record.Parent}
		}
		return nil
	})
	ByConnectorAddressIndexer = recordIndex(func(record *vanflow.ConnectorRecord, _ store.Metadata) []string {
		if record.Address != nil {
			return []string{*record.Address}
		}
		return nil
	})
	ByListenerAddressIndexer = recordIndex(func(record *vanflow.ListenerRecord, _ store.Metadata) []string {
		if record.Address != nil {
			return []string{*record.Address}
		}
		return nil
	})
)

func SourceIndex(e store.Entry) []string {
	return []string{e.Meta.Source.String()}
}

func recordIndex[R any](fn func(r R, meta store.Metadata) []string) store.CacheIndexer {
	return func(entry store.Entry) []string {
		record, ok := entry.Record.(R)
		if !ok {
			return nil
		}
		return fn(record, entry.Meta)
	}
}
