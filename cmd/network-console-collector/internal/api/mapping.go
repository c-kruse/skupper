package api

import (
	"github.com/skupperproject/skupper/pkg/vanflow"
	"github.com/skupperproject/skupper/pkg/vanflow/store"
)

func toProcessRecord(in store.Entry) ProcessRecord {
	process := in.Record.(vanflow.ProcessRecord)
	return ProcessRecord{
		BaseRecord: toBase(process.BaseRecord, process.Parent, in.Source.ID),
		Name:       process.Name,
		GroupName:  process.Group,
		ImageName:  process.ImageName,
	}
}
func toRouterRecord(in store.Entry) RouterRecord {
	router := in.Record.(vanflow.RouterRecord)
	return RouterRecord{
		BaseRecord:   toBase(router.BaseRecord, router.Parent, in.Source.ID),
		Name:         dref(router.Name),
		Namespace:    router.Namespace,
		BuildVersion: dref(router.BuildVersion),
		ImageName:    dref(router.ImageName),
		ImageVersion: dref(router.ImageVersion),
		Mode:         dref(router.Mode),
	}
}

func toSiteRecord(in store.Entry) SiteRecord {
	site := in.Record.(vanflow.SiteRecord)
	return SiteRecord{
		BaseRecord:  toBase(site.BaseRecord, nil, in.Source.ID),
		Location:    dref(site.Location),
		Name:        dref(site.Name),
		NameSpace:   dref(site.Namespace),
		Platform:    dref(site.Platform),
		Policy:      dref(site.Policy),
		Provider:    dref(site.Provider),
		SiteVersion: dref(site.Version),
	}
}

func toBase(in vanflow.BaseRecord, parent *string, source string) BaseRecord {
	var out BaseRecord
	out.Identity = in.ID
	if in.StartTime != nil {
		out.StartTime = uint64(in.StartTime.UnixMicro())
	}
	if in.EndTime != nil {
		out.EndTime = uint64(in.EndTime.UnixMicro())
	}
	if parent != nil {
		out.Parent = *parent
	}
	out.Source = &source
	return out
}

func dref[T any](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}
