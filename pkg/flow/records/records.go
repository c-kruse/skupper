package records

import (
	"fmt"

	"github.com/interconnectedcloud/go-amqp"
)

// RecordTypes
const (
	Site         = iota // 0
	Router              // 1
	Link                // 2
	Controller          // 3
	Listener            // 4
	Connector           // 5
	Flow                // 6
	Process             // 7
	Image               // 8
	Ingress             // 9
	Egress              // 10
	Collector           // 11
	ProcessGroup        // 12
	Host                // 13
	LogEvent            // 14
)

// Attribute Types
const (
	TypeOfRecord    = iota //0
	Identity               // 1
	Parent                 // 2
	StartTime              // 3
	EndTime                // 4
	CounterFlow            // 5
	PeerIdentity           // 6
	ProcessIdentity        // 7
	SiblingOrdinal         // 8
	Location               // 9
	Provider               // 10
	Platform               // 11
	Namespace              // 12
	Mode                   // 13
	SourceHost             // 14
	DestHost               // 15
	Protocol               // 16
	SourcePort             // 17
	DestPort               // 18
	Address                // 19
	ImageName              // 20
	ImageVersion           // 21
	Hostname               // 22
	Octets                 // 23
	Latency                // 24
	TransitLatency         // 25
	Backlog                // 26
	Method                 // 27
	Result                 // 28
	Reason                 // 29
	Name                   // 30
	Trace                  // 31
	BuildVersion           // 32
	LinkCost               // 33
	Direction              // 34
	OctetRate              // 35
	OctetsOut              // 36
	OctetsUnacked          // 37
	WindowClosures         // 38
	WindowSize             // 39
	FlowCountL4            // 40
	FlowCountL7            // 41
	FlowRateL4             // 42
	FlowRateL7             // 43
	Duration               // 44
	ImageAttr              // 45
	Group                  // 46
	StreamIdentity         // 47
	LogSeverity            // 48
	LogText                // 49
	SourceFile             // 50
	SourceLine             // 51
	Version                // 52
	Policy                 // 53
	Target                 // 54
)

type Record interface {
	AttributeSet() map[any]any
}

type recordDecoder func(map[interface{}]interface{}) (Record, error)

var recordDecoders = map[uint32]recordDecoder{
	Site:       decodeSite,
	Router:     decodeRouter,
	Link:       decodeLink,
	Controller: decodeController,
	Listener:   decodeListener,
	Connector:  decodeConnector,
}

func decodeRecords(msg *amqp.Message) ([]Record, error) {
	var records []Record
	values, ok := msg.Value.([]interface{})
	if !ok {
		return records, fmt.Errorf("unexpected type for message Value: %T", msg.Value)
	}
	for _, value := range values {
		valueMap, ok := value.(map[interface{}]interface{})
		if !ok {
			return records, fmt.Errorf("unexpected type for record attribute set: %T", value)
		}
		typeOfRecord, err := getFromAttributeSet[uint32](valueMap, TypeOfRecord)
		if err != nil {
			return records, fmt.Errorf("error geting record type attribute: %w", err)
		}

		decode, ok := recordDecoders[typeOfRecord]
		if !ok {
			return records, fmt.Errorf("unexpected record type: %d", typeOfRecord)
		}
		record, err := decode(valueMap)
		if err != nil {
			return records, err
		}
		records = append(records, record)
	}
	return records, nil
}

func setOpt[T any](attributeSet map[any]any, codePoint uint32, opt *T) {
	if opt == nil {
		return
	}
	attributeSet[codePoint] = *opt
}
func getFromAttributeSet[T any](attributeSet map[any]any, codePoint uint32) (T, error) {
	var attribute T
	raw, ok := attributeSet[codePoint]
	if !ok {
		return attribute, fmt.Errorf("codepoint %d not set", codePoint)
	}
	attribute, ok = raw.(T)
	if !ok {
		return attribute, fmt.Errorf("expected record attribute type %T. got %T", attribute, raw)
	}

	return attribute, nil
}

func getFromAttributeSetOpt[T any](attributeSet map[any]any, codePoint uint32) (*T, error) {
	raw, ok := attributeSet[codePoint]
	if !ok {
		return nil, nil
	}
	out, ok := raw.(T)
	if !ok {
		return nil, fmt.Errorf("expected record attribute type %T. got %T", out, raw)
	}
	return &out, nil
}

type BaseRecord struct {
	Identity  string
	Parent    *string
	StartTime *uint64
	EndTime   *uint64
}

func (r BaseRecord) AttributeSet() map[any]any {
	attributeSet := make(map[any]any)
	attributeSet[Identity] = r.Identity
	setOpt(attributeSet, Parent, r.Parent)
	setOpt(attributeSet, StartTime, r.StartTime)
	setOpt(attributeSet, EndTime, r.EndTime)
	return attributeSet
}

func decodeBase(attributeSet map[any]any) (BaseRecord, error) {
	var base BaseRecord
	id, err := getFromAttributeSet[string](attributeSet, Identity)
	if err != nil {
		return base, fmt.Errorf("error getting record identity attribute: %w", err)
	}

	base.Identity = id

	if base.Parent, err = getFromAttributeSetOpt[string](attributeSet, Parent); err != nil {
		return base, fmt.Errorf("error getting record parent attribute: %w", err)
	}
	if base.StartTime, err = getFromAttributeSetOpt[uint64](attributeSet, StartTime); err != nil {
		return base, fmt.Errorf("error getting record starttime attribute: %w", err)
	}
	if base.EndTime, err = getFromAttributeSetOpt[uint64](attributeSet, EndTime); err != nil {
		return base, fmt.Errorf("error getting record endtime attribute: %w", err)
	}
	return base, nil
}

type SiteRecord struct {
	BaseRecord
	Location  *string
	Provider  *string
	Platform  *string
	Name      *string
	Namespace *string
	Version   *string // unspeced
	Policy    *string // unspeced
}

func (r SiteRecord) AttributeSet() map[any]any {
	attributeSet := r.BaseRecord.AttributeSet()
	setOpt(attributeSet, Location, r.Location)
	setOpt(attributeSet, Provider, r.Provider)
	setOpt(attributeSet, Platform, r.Platform)
	setOpt(attributeSet, Name, r.Name)
	setOpt(attributeSet, Namespace, r.Namespace)
	setOpt(attributeSet, Version, r.Version)
	setOpt(attributeSet, Policy, r.Policy)
	return attributeSet
}

func decodeSite(attributeSet map[any]any) (Record, error) {
	var err error
	var site SiteRecord
	if site.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return site, err
	}
	if site.Location, err = getFromAttributeSetOpt[string](attributeSet, Location); err != nil {
		return site, fmt.Errorf("error getting record Location attribute: %w", err)
	}
	if site.Provider, err = getFromAttributeSetOpt[string](attributeSet, Provider); err != nil {
		return site, fmt.Errorf("error getting record Provider attribute: %w", err)
	}
	if site.Platform, err = getFromAttributeSetOpt[string](attributeSet, Platform); err != nil {
		return site, fmt.Errorf("error getting record Platform attribute: %w", err)
	}
	if site.Name, err = getFromAttributeSetOpt[string](attributeSet, Name); err != nil {
		return site, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if site.Namespace, err = getFromAttributeSetOpt[string](attributeSet, Namespace); err != nil {
		return site, fmt.Errorf("error getting record Namespace attribute: %w", err)
	}
	if site.Version, err = getFromAttributeSetOpt[string](attributeSet, Version); err != nil {
		return site, fmt.Errorf("error getting record Version attribute: %w", err)
	}
	if site.Policy, err = getFromAttributeSetOpt[string](attributeSet, Policy); err != nil {
		return site, fmt.Errorf("error getting record Policy attribute: %w", err)
	}
	return site, nil

}

type RouterRecord struct {
	BaseRecord
	Name         *string
	Namespace    *string
	Mode         *string
	ImageName    *string
	ImageVersion *string
	Hostname     *string
	BuildVersion *string
}

func (r RouterRecord) AttributeSet() map[any]any {
	attributeSet := r.BaseRecord.AttributeSet()
	setOpt(attributeSet, Name, r.Name)
	setOpt(attributeSet, Namespace, r.Namespace)
	setOpt(attributeSet, Mode, r.Mode)
	setOpt(attributeSet, ImageName, r.ImageName)
	setOpt(attributeSet, ImageVersion, r.ImageVersion)
	setOpt(attributeSet, Hostname, r.Hostname)
	setOpt(attributeSet, BuildVersion, r.BuildVersion)
	return attributeSet
}

func decodeRouter(attributeSet map[any]any) (Record, error) {
	var err error
	var router RouterRecord
	if router.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return router, err
	}

	if router.Name, err = getFromAttributeSetOpt[string](attributeSet, Name); err != nil {
		return router, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if router.Namespace, err = getFromAttributeSetOpt[string](attributeSet, Namespace); err != nil {
		return router, fmt.Errorf("error getting record Namespace attribute: %w", err)
	}
	if router.Mode, err = getFromAttributeSetOpt[string](attributeSet, Mode); err != nil {
		return router, fmt.Errorf("error getting record Mode attribute: %w", err)
	}
	if router.ImageName, err = getFromAttributeSetOpt[string](attributeSet, ImageName); err != nil {
		return router, fmt.Errorf("error getting record ImageName attribute: %w", err)
	}
	if router.ImageVersion, err = getFromAttributeSetOpt[string](attributeSet, ImageVersion); err != nil {
		return router, fmt.Errorf("error getting record ImageVersion attribute: %w", err)
	}
	if router.Hostname, err = getFromAttributeSetOpt[string](attributeSet, Hostname); err != nil {
		return router, fmt.Errorf("error getting record Hostname attribute: %w", err)
	}
	if router.BuildVersion, err = getFromAttributeSetOpt[string](attributeSet, BuildVersion); err != nil {
		return router, fmt.Errorf("error getting record BuildVersion attribute: %w", err)
	}
	return router, nil

}

type LinkRecord struct {
	BaseRecord
	Mode      *string
	Name      *string
	LinkCost  *uint64
	Direction *string
}

func (r LinkRecord) AttributeSet() map[any]any {
	attributeSet := r.BaseRecord.AttributeSet()
	setOpt(attributeSet, Name, r.Name)
	setOpt(attributeSet, Mode, r.Mode)
	setOpt(attributeSet, LinkCost, r.LinkCost)
	setOpt(attributeSet, Direction, r.Direction)
	return attributeSet
}

func decodeLink(attributeSet map[any]any) (Record, error) {
	var err error
	var link LinkRecord
	if link.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return link, err
	}

	if link.Name, err = getFromAttributeSetOpt[string](attributeSet, Name); err != nil {
		return link, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if link.Mode, err = getFromAttributeSetOpt[string](attributeSet, Mode); err != nil {
		return link, fmt.Errorf("error getting record Mode attribute: %w", err)
	}
	if link.LinkCost, err = getFromAttributeSetOpt[uint64](attributeSet, LinkCost); err != nil {
		return link, fmt.Errorf("error getting record LinkCost attribute: %w", err)
	}
	if link.Direction, err = getFromAttributeSetOpt[string](attributeSet, Direction); err != nil {
		return link, fmt.Errorf("error getting record Direction attribute: %w", err)
	}
	return link, nil

}

type ControllerRecord struct {
	BaseRecord
	ImageName    *string
	ImageVersion *string
	Hostname     *string
	Name         *string
	BuildVersion *string
}

func (r ControllerRecord) AttributeSet() map[any]any {
	attributeSet := r.BaseRecord.AttributeSet()
	setOpt(attributeSet, Name, r.Name)
	setOpt(attributeSet, ImageName, r.ImageName)
	setOpt(attributeSet, ImageVersion, r.ImageVersion)
	setOpt(attributeSet, Hostname, r.Hostname)
	setOpt(attributeSet, BuildVersion, r.BuildVersion)
	return attributeSet
}

func decodeController(attributeSet map[any]any) (Record, error) {
	var err error
	var controller ControllerRecord
	if controller.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return controller, err
	}

	if controller.Name, err = getFromAttributeSetOpt[string](attributeSet, Name); err != nil {
		return controller, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if controller.ImageName, err = getFromAttributeSetOpt[string](attributeSet, ImageName); err != nil {
		return controller, fmt.Errorf("error getting record ImageName attribute: %w", err)
	}
	if controller.ImageVersion, err = getFromAttributeSetOpt[string](attributeSet, ImageVersion); err != nil {
		return controller, fmt.Errorf("error getting record ImageVersion attribute: %w", err)
	}
	if controller.Hostname, err = getFromAttributeSetOpt[string](attributeSet, Hostname); err != nil {
		return controller, fmt.Errorf("error getting record Hostname attribute: %w", err)
	}
	if controller.BuildVersion, err = getFromAttributeSetOpt[string](attributeSet, BuildVersion); err != nil {
		return controller, fmt.Errorf("error getting record BuildVersion attribute: %w", err)
	}
	return controller, nil
}

type ListenerRecord struct {
	BaseRecord
	Name        *string
	DestHost    *string
	DestPort    *string
	Protocol    *string
	Address     *string
	FlowCountL4 *uint64
	FlowRateL4  *uint64
	FlowCountL7 *uint64
	FlowRateL7  *uint64
}

func (r ListenerRecord) AttributeSet() map[any]any {
	attributeSet := r.BaseRecord.AttributeSet()
	setOpt(attributeSet, Name, r.Name)
	setOpt(attributeSet, DestHost, r.DestHost)
	setOpt(attributeSet, DestPort, r.DestPort)
	setOpt(attributeSet, Protocol, r.Protocol)
	setOpt(attributeSet, Address, r.Address)
	return attributeSet
}

func decodeListener(attributeSet map[any]any) (Record, error) {
	var err error
	var listener ListenerRecord
	if listener.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return listener, err
	}

	if listener.Name, err = getFromAttributeSetOpt[string](attributeSet, Name); err != nil {
		return listener, fmt.Errorf("error getting record Name attribute: %w", err)
	}
	if listener.DestHost, err = getFromAttributeSetOpt[string](attributeSet, DestHost); err != nil {
		return listener, fmt.Errorf("error getting record DestHost attribute: %w", err)
	}
	if listener.DestPort, err = getFromAttributeSetOpt[string](attributeSet, DestPort); err != nil {
		return listener, fmt.Errorf("error getting record DestPort attribute: %w", err)
	}
	if listener.Protocol, err = getFromAttributeSetOpt[string](attributeSet, Protocol); err != nil {
		return listener, fmt.Errorf("error getting record Protocol attribute: %w", err)
	}
	if listener.Address, err = getFromAttributeSetOpt[string](attributeSet, Address); err != nil {
		return listener, fmt.Errorf("error getting record Address attribute: %w", err)
	}
	return listener, nil

}

type ConnectorRecord struct {
	BaseRecord
	DestHost    *string
	DestPort    *string
	Protocol    *string
	Address     *string
	FlowCountL4 *uint64
	FlowRateL4  *uint64
	FlowCountL7 *uint64
	FlowRateL7  *uint64
}

func (r ConnectorRecord) AttributeSet() map[any]any {
	attributeSet := r.BaseRecord.AttributeSet()
	setOpt(attributeSet, DestHost, r.DestHost)
	setOpt(attributeSet, DestPort, r.DestPort)
	setOpt(attributeSet, Protocol, r.Protocol)
	setOpt(attributeSet, Address, r.Address)
	return attributeSet
}

func decodeConnector(attributeSet map[any]any) (Record, error) {
	var err error
	var connector ConnectorRecord
	if connector.BaseRecord, err = decodeBase(attributeSet); err != nil {
		return connector, err
	}

	if connector.DestHost, err = getFromAttributeSetOpt[string](attributeSet, DestHost); err != nil {
		return connector, fmt.Errorf("error getting record DestHost attribute: %w", err)
	}
	if connector.DestPort, err = getFromAttributeSetOpt[string](attributeSet, DestPort); err != nil {
		return connector, fmt.Errorf("error getting record DestPort attribute: %w", err)
	}
	if connector.Protocol, err = getFromAttributeSetOpt[string](attributeSet, Protocol); err != nil {
		return connector, fmt.Errorf("error getting record Protocol attribute: %w", err)
	}
	if connector.Address, err = getFromAttributeSetOpt[string](attributeSet, Address); err != nil {
		return connector, fmt.Errorf("error getting record Address attribute: %w", err)
	}
	return connector, nil

}

type FlowRecord struct {
	BaseRecord
	SourceHost     *string
	SourcePort     *string
	CounterFlow    *string
	Trace          *string
	Latency        *uint64
	Octets         *uint64
	OctetsOut      *uint64
	OctetsUnacked  *uint64
	WindowClosures *uint64
	WindowSize     *uint64
	Reason         *string
	Method         *string
	Result         *string
	StreamIdentity *uint64
	Process        *string
}

type ProcessRecord struct {
	BaseRecord
	Name       *string
	ImageName  *string
	Hostname   *string
	Group      *string
	SourceHost *string // unspeced
	Mode       *string // unspeced
}

type LogEventRecord struct {
	BaseRecord
	LogSeverity *uint64
	LogText     *string
	SourceFile  *string
	SourceLine  *uint64
}

// Unspeced
type HostRecord struct {
	BaseRecord
	Name     *string
	Provider *string
	Platform *string
}
