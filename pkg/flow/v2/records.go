package v2

type SiteRecord struct {
	BaseRecord
	Location  *string `vflow:"9"`
	Provider  *string `vflow:"10"`
	Platform  *string `vflow:"11"`
	Namespace *string `vflow:"12"`
	Version   *string `vflow:"52"`
	Policy    *string `vflow:"53"`
}

type RouterRecord struct {
	BaseRecord
	Parent       *string `vflow:"2"`
	Namespace    *string `vflow:"12"`
	Mode         *string `vflow:"13"`
	ImageName    *string `vflow:"20"`
	ImageVersion *string `vflow:"21"`
	Hostname     *string `vflow:"22"`
	Name         *string `vflow:"30"`
	BuildVersion *string `vflow:"32"`
}

type LinkRecord struct {
	BaseRecord
	Parent    *string `vflow:"2"`
	Mode      *string `vflow:"13"`
	Name      *string `vflow:"30"`
	LinkCost  *uint64 `vflow:"33"`
	Direction *string `vflow:"34"`
}

type ControllerRecord struct {
	BaseRecord
	Parent       *string `vflow:"2"`
	ImageName    *string `vflow:"20"`
	ImageVersion *string `vflow:"21"`
	Hostname     *string `vflow:"22"`
	Name         *string `vflow:"30"`
	BuildVersion *string `vflow:"32"`
}

type ListenerRecord struct {
	BaseRecord
	Parent      *string `vflow:"2"`
	DestHost    *string `vflow:"15"`
	Protocol    *string `vflow:"16"`
	DestPort    *string `vflow:"18"`
	Address     *string `vflow:"19"`
	FlowCountL4 *uint64 `vflow:"40"`
	FlowCountL7 *uint64 `vflow:"41"`
	FlowRateL4  *uint64 `vflow:"42"`
	FlowRateL7  *uint64 `vflow:"43"`
}

type ConnectorRecord struct {
	BaseRecord
	Parent      *string `vflow:"2"`
	DestHost    *string `vflow:"15"`
	Protocol    *string `vflow:"16"`
	DestPort    *string `vflow:"18"`
	Address     *string `vflow:"19"`
	FlowCountL4 *uint64 `vflow:"40"`
	FlowCountL7 *uint64 `vflow:"41"`
	FlowRateL4  *uint64 `vflow:"42"`
	FlowRateL7  *uint64 `vflow:"43"`
}

type FlowRecord struct {
	BaseRecord
	Parent         *string `vflow:"2"`
	Counterflow    *string `vflow:"5"`
	SourceHost     *string `vflow:"14"`
	SourcePort     *string `vflow:"17"`
	Octets         *uint64 `vflow:"23"`
	Latency        *uint64 `vflow:"24"`
	Reason         *string `vflow:"29"`
	Trace          *string `vflow:"31"`
	OctetRate      *uint64 `vflow:"35"`
	OctetsOut      *uint64 `vflow:"36"`
	OctetsUnacked  *uint64 `vflow:"37"`
	WindowClosures *uint64 `vflow:"38"`
	WindowSize     *uint64 `vflow:"39"`
}

type ProcessRecord struct {
	BaseRecord
	Mode         *string `vflow:"13"`
	SourceHost   *string `vflow:"14"`
	ImageName    *string `vflow:"20"`
	ImageVersion *string `vflow:"21"`
	Hostname     *string `vflow:"22"`
	Name         *string `vflow:"30"`
	Group        *string `vflow:"46"`
}

type ImageRecord struct {
	BaseRecord
}

type IngressRecord struct {
	BaseRecord
}

type EgressRecord struct {
	BaseRecord
}

type CollectorRecord struct {
	BaseRecord
}

type ProcessGroupRecord struct {
	BaseRecord
}

type HostRecord struct {
	BaseRecord
	Provider *string `vflow:"10"`
	Name     *string `vflow:"30"`
}

type LogRecord struct {
	BaseRecord
	LogSeverity *uint64 `vflow:"48"`
	LogText     *string `vflow:"49"`
	SourceFile  *string `vflow:"50"`
	SourceLine  *uint64 `vflow:"51"`
}
