package v2

import "time"

type BaseRecord struct {
	Identity  string     `vflow:"1,identity"`
	StartTime *time.Time `vflow:"3"`
	EndTime   *time.Time `vflow:"4"`
}

func init() {
	MustRegisterRecord(0, SiteRecord{})
	MustRegisterRecord(1, RouterRecord{})
	MustRegisterRecord(2, LinkRecord{})
	MustRegisterRecord(3, ControllerRecord{})
	MustRegisterRecord(4, ListenerRecord{})
	MustRegisterRecord(5, ConnectorRecord{})
	MustRegisterRecord(6, FlowRecord{})
	MustRegisterRecord(7, ProcessRecord{})
	MustRegisterRecord(8, ImageRecord{})
	MustRegisterRecord(9, IngressRecord{})
	MustRegisterRecord(10, EgressRecord{})
	MustRegisterRecord(11, CollectorRecord{})
	MustRegisterRecord(12, ProcessGroupRecord{})
	MustRegisterRecord(13, HostRecord{})
	MustRegisterRecord(14, LogRecord{})
}
