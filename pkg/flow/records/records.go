// Code is generated. DO NOT EDIT

package records

import "fmt"

type recordType uint32

// RecordType codes
const (
	site         = 0
	router       = 1
	link         = 2
	controller   = 3
	listener     = 4
	connector    = 5
	flow         = 6
	process      = 7
	image        = 8
	ingress      = 9
	egress       = 10
	collector    = 11
	processGroup = 12
	host         = 13
	log          = 14
)

var recordDecoders = map[recordType]recordDecoder{
	site:         decodeSite,
	router:       decodeRouter,
	link:         decodeLink,
	controller:   decodeController,
	listener:     decodeListener,
	connector:    decodeConnector,
	flow:         decodeFlow,
	process:      decodeProcess,
	image:        decodeImage,
	ingress:      decodeIngress,
	egress:       decodeEgress,
	collector:    decodeCollector,
	processGroup: decodeProcessGroup,
	host:         decodeHost,
	log:          decodeLog,
}

type SiteRecord struct {
	BaseRecord
	Location  *string // 9
	Provider  *string // 10
	Platform  *string // 11
	Namespace *string // 12
	Version   *string // 52
	Policy    *string // 53
}

func (r SiteRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(site)
	setOpt(attributeSet, 9, r.Location)
	setOpt(attributeSet, 10, r.Provider)
	setOpt(attributeSet, 11, r.Platform)
	setOpt(attributeSet, 12, r.Namespace)
	setOpt(attributeSet, 52, r.Version)
	setOpt(attributeSet, 53, r.Policy)
	return attributeSet
}

func decodeSite(attributeSet map[any]any) (Record, error) {
	var err error
	var site SiteRecord
	if site.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return site, err
	}
	if site.Location, err = getFromAttributeSetOpt[string](attributeSet, 9); err != nil {
		return site, fmt.Errorf("error getting record Location attribute: %w", err)
	}
	if site.Provider, err = getFromAttributeSetOpt[string](attributeSet, 10); err != nil {
		return site, fmt.Errorf("error getting record Provider attribute: %w", err)
	}
	if site.Platform, err = getFromAttributeSetOpt[string](attributeSet, 11); err != nil {
		return site, fmt.Errorf("error getting record Platform attribute: %w", err)
	}
	if site.Namespace, err = getFromAttributeSetOpt[string](attributeSet, 12); err != nil {
		return site, fmt.Errorf("error getting record Namespace attribute: %w", err)
	}
	if site.Version, err = getFromAttributeSetOpt[string](attributeSet, 52); err != nil {
		return site, fmt.Errorf("error getting record Version attribute: %w", err)
	}
	if site.Policy, err = getFromAttributeSetOpt[string](attributeSet, 53); err != nil {
		return site, fmt.Errorf("error getting record Policy attribute: %w", err)
	}
	return site, nil
}

type RouterRecord struct {
	BaseRecord
	Parent       *string // 2
	Namespace    *string // 12
	Mode         *string // 13
	ImageName    *string // 20
	ImageVersion *string // 21
	Hostname     *string // 22
	Name         *string // 30
	BuildVersion *string // 32
}

func (r RouterRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(router)
	setOpt(attributeSet, 2, r.Parent)
	setOpt(attributeSet, 12, r.Namespace)
	setOpt(attributeSet, 13, r.Mode)
	setOpt(attributeSet, 20, r.ImageName)
	setOpt(attributeSet, 21, r.ImageVersion)
	setOpt(attributeSet, 22, r.Hostname)
	setOpt(attributeSet, 30, r.Name)
	setOpt(attributeSet, 32, r.BuildVersion)
	return attributeSet
}

func decodeRouter(attributeSet map[any]any) (Record, error) {
	var err error
	var router RouterRecord
	if router.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return router, err
	}
	if router.Parent, err = getFromAttributeSetOpt[string](attributeSet, 2); err != nil {
		return router, fmt.Errorf("error getting record Parent attribute: %w", err)
	}
	if router.Namespace, err = getFromAttributeSetOpt[string](attributeSet, 12); err != nil {
		return router, fmt.Errorf("error getting record Namespace attribute: %w", err)
	}
	if router.Mode, err = getFromAttributeSetOpt[string](attributeSet, 13); err != nil {
		return router, fmt.Errorf("error getting record Mode attribute: %w", err)
	}
	if router.ImageName, err = getFromAttributeSetOpt[string](attributeSet, 20); err != nil {
		return router, fmt.Errorf("error getting record ImageName attribute: %w", err)
	}
	if router.ImageVersion, err = getFromAttributeSetOpt[string](attributeSet, 21); err != nil {
		return router, fmt.Errorf("error getting record ImageVersion attribute: %w", err)
	}
	if router.Hostname, err = getFromAttributeSetOpt[string](attributeSet, 22); err != nil {
		return router, fmt.Errorf("error getting record Hostname attribute: %w", err)
	}
	if router.Name, err = getFromAttributeSetOpt[string](attributeSet, 30); err != nil {
		return router, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if router.BuildVersion, err = getFromAttributeSetOpt[string](attributeSet, 32); err != nil {
		return router, fmt.Errorf("error getting record BuildVersion attribute: %w", err)
	}
	return router, nil
}

type LinkRecord struct {
	BaseRecord
	Parent    *string // 2
	Mode      *string // 13
	Name      *string // 30
	LinkCost  *uint64 // 33
	Direction *string // 34
}

func (r LinkRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(link)
	setOpt(attributeSet, 2, r.Parent)
	setOpt(attributeSet, 13, r.Mode)
	setOpt(attributeSet, 30, r.Name)
	setOpt(attributeSet, 33, r.LinkCost)
	setOpt(attributeSet, 34, r.Direction)
	return attributeSet
}

func decodeLink(attributeSet map[any]any) (Record, error) {
	var err error
	var link LinkRecord
	if link.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return link, err
	}
	if link.Parent, err = getFromAttributeSetOpt[string](attributeSet, 2); err != nil {
		return link, fmt.Errorf("error getting record Parent attribute: %w", err)
	}
	if link.Mode, err = getFromAttributeSetOpt[string](attributeSet, 13); err != nil {
		return link, fmt.Errorf("error getting record Mode attribute: %w", err)
	}
	if link.Name, err = getFromAttributeSetOpt[string](attributeSet, 30); err != nil {
		return link, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if link.LinkCost, err = getFromAttributeSetOpt[uint64](attributeSet, 33); err != nil {
		return link, fmt.Errorf("error getting record LinkCost attribute: %w", err)
	}
	if link.Direction, err = getFromAttributeSetOpt[string](attributeSet, 34); err != nil {
		return link, fmt.Errorf("error getting record Direction attribute: %w", err)
	}
	return link, nil
}

type ControllerRecord struct {
	BaseRecord
	Parent       *string // 2
	ImageName    *string // 20
	ImageVersion *string // 21
	Hostname     *string // 22
	Name         *string // 30
	BuildVersion *string // 32
}

func (r ControllerRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(controller)
	setOpt(attributeSet, 2, r.Parent)
	setOpt(attributeSet, 20, r.ImageName)
	setOpt(attributeSet, 21, r.ImageVersion)
	setOpt(attributeSet, 22, r.Hostname)
	setOpt(attributeSet, 30, r.Name)
	setOpt(attributeSet, 32, r.BuildVersion)
	return attributeSet
}

func decodeController(attributeSet map[any]any) (Record, error) {
	var err error
	var controller ControllerRecord
	if controller.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return controller, err
	}
	if controller.Parent, err = getFromAttributeSetOpt[string](attributeSet, 2); err != nil {
		return controller, fmt.Errorf("error getting record Parent attribute: %w", err)
	}
	if controller.ImageName, err = getFromAttributeSetOpt[string](attributeSet, 20); err != nil {
		return controller, fmt.Errorf("error getting record ImageName attribute: %w", err)
	}
	if controller.ImageVersion, err = getFromAttributeSetOpt[string](attributeSet, 21); err != nil {
		return controller, fmt.Errorf("error getting record ImageVersion attribute: %w", err)
	}
	if controller.Hostname, err = getFromAttributeSetOpt[string](attributeSet, 22); err != nil {
		return controller, fmt.Errorf("error getting record Hostname attribute: %w", err)
	}
	if controller.Name, err = getFromAttributeSetOpt[string](attributeSet, 30); err != nil {
		return controller, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if controller.BuildVersion, err = getFromAttributeSetOpt[string](attributeSet, 32); err != nil {
		return controller, fmt.Errorf("error getting record BuildVersion attribute: %w", err)
	}
	return controller, nil
}

type ListenerRecord struct {
	BaseRecord
	Parent      *string // 2
	DestHost    *string // 15
	Protocol    *string // 16
	DestPort    *string // 18
	Address     *string // 19
	FlowCountL4 *uint64 // 40
	FlowCountL7 *uint64 // 41
	FlowRateL4  *uint64 // 42
	FlowRateL7  *uint64 // 43
}

func (r ListenerRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(listener)
	setOpt(attributeSet, 2, r.Parent)
	setOpt(attributeSet, 15, r.DestHost)
	setOpt(attributeSet, 16, r.Protocol)
	setOpt(attributeSet, 18, r.DestPort)
	setOpt(attributeSet, 19, r.Address)
	setOpt(attributeSet, 40, r.FlowCountL4)
	setOpt(attributeSet, 41, r.FlowCountL7)
	setOpt(attributeSet, 42, r.FlowRateL4)
	setOpt(attributeSet, 43, r.FlowRateL7)
	return attributeSet
}

func decodeListener(attributeSet map[any]any) (Record, error) {
	var err error
	var listener ListenerRecord
	if listener.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return listener, err
	}
	if listener.Parent, err = getFromAttributeSetOpt[string](attributeSet, 2); err != nil {
		return listener, fmt.Errorf("error getting record Parent attribute: %w", err)
	}
	if listener.DestHost, err = getFromAttributeSetOpt[string](attributeSet, 15); err != nil {
		return listener, fmt.Errorf("error getting record DestHost attribute: %w", err)
	}
	if listener.Protocol, err = getFromAttributeSetOpt[string](attributeSet, 16); err != nil {
		return listener, fmt.Errorf("error getting record Protocol attribute: %w", err)
	}
	if listener.DestPort, err = getFromAttributeSetOpt[string](attributeSet, 18); err != nil {
		return listener, fmt.Errorf("error getting record DestPort attribute: %w", err)
	}
	if listener.Address, err = getFromAttributeSetOpt[string](attributeSet, 19); err != nil {
		return listener, fmt.Errorf("error getting record Address attribute: %w", err)
	}
	if listener.FlowCountL4, err = getFromAttributeSetOpt[uint64](attributeSet, 40); err != nil {
		return listener, fmt.Errorf("error getting record FlowCountL4 attribute: %w", err)
	}
	if listener.FlowCountL7, err = getFromAttributeSetOpt[uint64](attributeSet, 41); err != nil {
		return listener, fmt.Errorf("error getting record FlowCountL7 attribute: %w", err)
	}
	if listener.FlowRateL4, err = getFromAttributeSetOpt[uint64](attributeSet, 42); err != nil {
		return listener, fmt.Errorf("error getting record FlowRateL4 attribute: %w", err)
	}
	if listener.FlowRateL7, err = getFromAttributeSetOpt[uint64](attributeSet, 43); err != nil {
		return listener, fmt.Errorf("error getting record FlowRateL7 attribute: %w", err)
	}
	return listener, nil
}

type ConnectorRecord struct {
	BaseRecord
	Parent      *string // 2
	DestHost    *string // 15
	Protocol    *string // 16
	DestPort    *string // 18
	Address     *string // 19
	FlowCountL4 *uint64 // 40
	FlowCountL7 *uint64 // 41
	FlowRateL4  *uint64 // 42
	FlowRateL7  *uint64 // 43
}

func (r ConnectorRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(connector)
	setOpt(attributeSet, 2, r.Parent)
	setOpt(attributeSet, 15, r.DestHost)
	setOpt(attributeSet, 16, r.Protocol)
	setOpt(attributeSet, 18, r.DestPort)
	setOpt(attributeSet, 19, r.Address)
	setOpt(attributeSet, 40, r.FlowCountL4)
	setOpt(attributeSet, 41, r.FlowCountL7)
	setOpt(attributeSet, 42, r.FlowRateL4)
	setOpt(attributeSet, 43, r.FlowRateL7)
	return attributeSet
}

func decodeConnector(attributeSet map[any]any) (Record, error) {
	var err error
	var connector ConnectorRecord
	if connector.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return connector, err
	}
	if connector.Parent, err = getFromAttributeSetOpt[string](attributeSet, 2); err != nil {
		return connector, fmt.Errorf("error getting record Parent attribute: %w", err)
	}
	if connector.DestHost, err = getFromAttributeSetOpt[string](attributeSet, 15); err != nil {
		return connector, fmt.Errorf("error getting record DestHost attribute: %w", err)
	}
	if connector.Protocol, err = getFromAttributeSetOpt[string](attributeSet, 16); err != nil {
		return connector, fmt.Errorf("error getting record Protocol attribute: %w", err)
	}
	if connector.DestPort, err = getFromAttributeSetOpt[string](attributeSet, 18); err != nil {
		return connector, fmt.Errorf("error getting record DestPort attribute: %w", err)
	}
	if connector.Address, err = getFromAttributeSetOpt[string](attributeSet, 19); err != nil {
		return connector, fmt.Errorf("error getting record Address attribute: %w", err)
	}
	if connector.FlowCountL4, err = getFromAttributeSetOpt[uint64](attributeSet, 40); err != nil {
		return connector, fmt.Errorf("error getting record FlowCountL4 attribute: %w", err)
	}
	if connector.FlowCountL7, err = getFromAttributeSetOpt[uint64](attributeSet, 41); err != nil {
		return connector, fmt.Errorf("error getting record FlowCountL7 attribute: %w", err)
	}
	if connector.FlowRateL4, err = getFromAttributeSetOpt[uint64](attributeSet, 42); err != nil {
		return connector, fmt.Errorf("error getting record FlowRateL4 attribute: %w", err)
	}
	if connector.FlowRateL7, err = getFromAttributeSetOpt[uint64](attributeSet, 43); err != nil {
		return connector, fmt.Errorf("error getting record FlowRateL7 attribute: %w", err)
	}
	return connector, nil
}

type FlowRecord struct {
	BaseRecord
	Parent         *string // 2
	Counterflow    *string // 5
	SourceHost     *string // 14
	SourcePort     *string // 17
	Octets         *uint64 // 23
	Latency        *uint64 // 24
	Reason         *string // 29
	Trace          *string // 31
	OctetRate      *uint64 // 35
	OctetsOut      *uint64 // 36
	OctetsUnacked  *uint64 // 37
	WindowClosures *uint64 // 38
	WindowSize     *uint64 // 39
}

func (r FlowRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(flow)
	setOpt(attributeSet, 2, r.Parent)
	setOpt(attributeSet, 5, r.Counterflow)
	setOpt(attributeSet, 14, r.SourceHost)
	setOpt(attributeSet, 17, r.SourcePort)
	setOpt(attributeSet, 23, r.Octets)
	setOpt(attributeSet, 24, r.Latency)
	setOpt(attributeSet, 29, r.Reason)
	setOpt(attributeSet, 31, r.Trace)
	setOpt(attributeSet, 35, r.OctetRate)
	setOpt(attributeSet, 36, r.OctetsOut)
	setOpt(attributeSet, 37, r.OctetsUnacked)
	setOpt(attributeSet, 38, r.WindowClosures)
	setOpt(attributeSet, 39, r.WindowSize)
	return attributeSet
}

func decodeFlow(attributeSet map[any]any) (Record, error) {
	var err error
	var flow FlowRecord
	if flow.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return flow, err
	}
	if flow.Parent, err = getFromAttributeSetOpt[string](attributeSet, 2); err != nil {
		return flow, fmt.Errorf("error getting record Parent attribute: %w", err)
	}
	if flow.Counterflow, err = getFromAttributeSetOpt[string](attributeSet, 5); err != nil {
		return flow, fmt.Errorf("error getting record Counterflow attribute: %w", err)
	}
	if flow.SourceHost, err = getFromAttributeSetOpt[string](attributeSet, 14); err != nil {
		return flow, fmt.Errorf("error getting record SourceHost attribute: %w", err)
	}
	if flow.SourcePort, err = getFromAttributeSetOpt[string](attributeSet, 17); err != nil {
		return flow, fmt.Errorf("error getting record SourcePort attribute: %w", err)
	}
	if flow.Octets, err = getFromAttributeSetOpt[uint64](attributeSet, 23); err != nil {
		return flow, fmt.Errorf("error getting record Octets attribute: %w", err)
	}
	if flow.Latency, err = getFromAttributeSetOpt[uint64](attributeSet, 24); err != nil {
		return flow, fmt.Errorf("error getting record Latency attribute: %w", err)
	}
	if flow.Reason, err = getFromAttributeSetOpt[string](attributeSet, 29); err != nil {
		return flow, fmt.Errorf("error getting record Reason attribute: %w", err)
	}
	if flow.Trace, err = getFromAttributeSetOpt[string](attributeSet, 31); err != nil {
		return flow, fmt.Errorf("error getting record Trace attribute: %w", err)
	}
	if flow.OctetRate, err = getFromAttributeSetOpt[uint64](attributeSet, 35); err != nil {
		return flow, fmt.Errorf("error getting record OctetRate attribute: %w", err)
	}
	if flow.OctetsOut, err = getFromAttributeSetOpt[uint64](attributeSet, 36); err != nil {
		return flow, fmt.Errorf("error getting record OctetsOut attribute: %w", err)
	}
	if flow.OctetsUnacked, err = getFromAttributeSetOpt[uint64](attributeSet, 37); err != nil {
		return flow, fmt.Errorf("error getting record OctetsUnacked attribute: %w", err)
	}
	if flow.WindowClosures, err = getFromAttributeSetOpt[uint64](attributeSet, 38); err != nil {
		return flow, fmt.Errorf("error getting record WindowClosures attribute: %w", err)
	}
	if flow.WindowSize, err = getFromAttributeSetOpt[uint64](attributeSet, 39); err != nil {
		return flow, fmt.Errorf("error getting record WindowSize attribute: %w", err)
	}
	return flow, nil
}

type ProcessRecord struct {
	BaseRecord
	Mode         *string // 13
	SourceHost   *string // 14
	ImageName    *string // 20
	ImageVersion *string // 21
	Hostname     *string // 22
	Name         *string // 30
	Group        *string // 46
}

func (r ProcessRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(process)
	setOpt(attributeSet, 13, r.Mode)
	setOpt(attributeSet, 14, r.SourceHost)
	setOpt(attributeSet, 20, r.ImageName)
	setOpt(attributeSet, 21, r.ImageVersion)
	setOpt(attributeSet, 22, r.Hostname)
	setOpt(attributeSet, 30, r.Name)
	setOpt(attributeSet, 46, r.Group)
	return attributeSet
}

func decodeProcess(attributeSet map[any]any) (Record, error) {
	var err error
	var process ProcessRecord
	if process.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return process, err
	}
	if process.Mode, err = getFromAttributeSetOpt[string](attributeSet, 13); err != nil {
		return process, fmt.Errorf("error getting record Mode attribute: %w", err)
	}
	if process.SourceHost, err = getFromAttributeSetOpt[string](attributeSet, 14); err != nil {
		return process, fmt.Errorf("error getting record SourceHost attribute: %w", err)
	}
	if process.ImageName, err = getFromAttributeSetOpt[string](attributeSet, 20); err != nil {
		return process, fmt.Errorf("error getting record ImageName attribute: %w", err)
	}
	if process.ImageVersion, err = getFromAttributeSetOpt[string](attributeSet, 21); err != nil {
		return process, fmt.Errorf("error getting record ImageVersion attribute: %w", err)
	}
	if process.Hostname, err = getFromAttributeSetOpt[string](attributeSet, 22); err != nil {
		return process, fmt.Errorf("error getting record Hostname attribute: %w", err)
	}
	if process.Name, err = getFromAttributeSetOpt[string](attributeSet, 30); err != nil {
		return process, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if process.Group, err = getFromAttributeSetOpt[string](attributeSet, 46); err != nil {
		return process, fmt.Errorf("error getting record Group attribute: %w", err)
	}
	return process, nil
}

type ImageRecord struct {
	BaseRecord
}

func (r ImageRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(image)
	return attributeSet
}

func decodeImage(attributeSet map[any]any) (Record, error) {
	var err error
	var image ImageRecord
	if image.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return image, err
	}
	return image, nil
}

type IngressRecord struct {
	BaseRecord
}

func (r IngressRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(ingress)
	return attributeSet
}

func decodeIngress(attributeSet map[any]any) (Record, error) {
	var err error
	var ingress IngressRecord
	if ingress.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return ingress, err
	}
	return ingress, nil
}

type EgressRecord struct {
	BaseRecord
}

func (r EgressRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(egress)
	return attributeSet
}

func decodeEgress(attributeSet map[any]any) (Record, error) {
	var err error
	var egress EgressRecord
	if egress.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return egress, err
	}
	return egress, nil
}

type CollectorRecord struct {
	BaseRecord
}

func (r CollectorRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(collector)
	return attributeSet
}

func decodeCollector(attributeSet map[any]any) (Record, error) {
	var err error
	var collector CollectorRecord
	if collector.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return collector, err
	}
	return collector, nil
}

type ProcessGroupRecord struct {
	BaseRecord
}

func (r ProcessGroupRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(processGroup)
	return attributeSet
}

func decodeProcessGroup(attributeSet map[any]any) (Record, error) {
	var err error
	var processGroup ProcessGroupRecord
	if processGroup.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return processGroup, err
	}
	return processGroup, nil
}

type HostRecord struct {
	BaseRecord
	Provider *string // 10
	Name     *string // 30
}

func (r HostRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(host)
	setOpt(attributeSet, 10, r.Provider)
	setOpt(attributeSet, 30, r.Name)
	return attributeSet
}

func decodeHost(attributeSet map[any]any) (Record, error) {
	var err error
	var host HostRecord
	if host.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return host, err
	}
	if host.Provider, err = getFromAttributeSetOpt[string](attributeSet, 10); err != nil {
		return host, fmt.Errorf("error getting record Provider attribute: %w", err)
	}
	if host.Name, err = getFromAttributeSetOpt[string](attributeSet, 30); err != nil {
		return host, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	return host, nil
}

type LogRecord struct {
	BaseRecord
	LogSeverity *uint64 // 48
	LogText     *string // 49
	SourceFile  *string // 50
	SourceLine  *uint64 // 51
}

func (r LogRecord) Encode() map[uint32]any {
	attributeSet := r.BaseRecord.Encode()
	attributeSet[typeOfRecord] = uint64(log)
	setOpt(attributeSet, 48, r.LogSeverity)
	setOpt(attributeSet, 49, r.LogText)
	setOpt(attributeSet, 50, r.SourceFile)
	setOpt(attributeSet, 51, r.SourceLine)
	return attributeSet
}

func decodeLog(attributeSet map[any]any) (Record, error) {
	var err error
	var log LogRecord
	if log.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return log, err
	}
	if log.LogSeverity, err = getFromAttributeSetOpt[uint64](attributeSet, 48); err != nil {
		return log, fmt.Errorf("error getting record LogSeverity attribute: %w", err)
	}
	if log.LogText, err = getFromAttributeSetOpt[string](attributeSet, 49); err != nil {
		return log, fmt.Errorf("error getting record LogText attribute: %w", err)
	}
	if log.SourceFile, err = getFromAttributeSetOpt[string](attributeSet, 50); err != nil {
		return log, fmt.Errorf("error getting record SourceFile attribute: %w", err)
	}
	if log.SourceLine, err = getFromAttributeSetOpt[uint64](attributeSet, 51); err != nil {
		return log, fmt.Errorf("error getting record SourceLine attribute: %w", err)
	}
	return log, nil
}
