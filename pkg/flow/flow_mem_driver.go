package flow

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

func (fc *FlowCollector) inferGatewaySite(siteId string) error {
	if _, ok := fc.Sites[siteId]; !ok {
		parts := strings.Split(siteId, "_")
		if len(parts) > 1 {
			if parts[0] == "gateway" {
				if _, ok := fc.Sites[siteId]; !ok {
					name := parts[1]
					namespace := parts[0] + "-" + parts[1]
					site := SiteRecord{
						Base: Base{
							RecType:   recordNames[Site],
							Identity:  siteId,
							StartTime: uint64(time.Now().UnixNano()) / uint64(time.Microsecond),
						},
						Name:      &name,
						NameSpace: &namespace,
					}
					log.Printf("FLOW_LOG: %s\n", prettyPrint(site))
					fc.Sites[siteId] = &site
				}
			}
		}
	}
	return nil
}

func (fc *FlowCollector) inferGatewayProcess(siteId string, host string) error {
	if site, ok := fc.Sites[siteId]; ok {
		groupName := *site.NameSpace + "-" + host
		groupIdentity := ""
		for _, pg := range fc.ProcessGroups {
			if *pg.Name == groupName {
				groupIdentity = pg.Identity
				break
			}
		}
		if groupIdentity == "" {
			groupIdentity = uuid.New().String()
			fc.ProcessGroups[groupIdentity] = &ProcessGroupRecord{
				Base: Base{
					RecType:   recordNames[ProcessGroup],
					Identity:  groupIdentity,
					StartTime: uint64(time.Now().UnixNano()) / uint64(time.Microsecond),
				},
				Name:             &groupName,
				ProcessGroupRole: &External,
			}
		}
		processName := *site.Name + "-" + host
		procFound := false
		for _, proc := range fc.Processes {
			if *proc.Name == processName {
				procFound = true
				break
			}
		}
		if !procFound {
			log.Printf("COLLECTOR: Inferring gateway process %s \n", host)
			procIdentity := uuid.New().String()
			fc.Processes[procIdentity] = &ProcessRecord{
				Base: Base{
					RecType:   recordNames[Process],
					Identity:  procIdentity,
					Parent:    siteId,
					StartTime: uint64(time.Now().UnixNano()) / uint64(time.Microsecond),
				},
				Name:          &processName,
				GroupName:     &groupName,
				GroupIdentity: &groupIdentity,
				HostName:      site.Name,
				SourceHost:    &host,
				ProcessRole:   &External,
			}
		}
	}
	return nil
}

func (c *FlowCollector) isGatewaySite(siteId string) bool {
	if site, ok := c.Sites[siteId]; ok {
		if site.NameSpace != nil {
			parts := strings.Split(*site.NameSpace, "-")
			if len(parts) > 1 {
				if parts[0] == "gateway" {
					return true
				}
			}
		}
	}
	return false
}

func (c *FlowCollector) getRecordSiteId(record interface{}) string {
	if record == nil {
		return ""
	}
	switch record.(type) {
	case SiteRecord:
		if site, ok := record.(SiteRecord); ok {
			return site.Identity
		}
	case RouterRecord:
		if router, ok := record.(RouterRecord); ok {
			return router.Parent
		}
	case LinkRecord:
		if link, ok := record.(LinkRecord); ok {
			if router, ok := c.Routers[link.Parent]; ok {
				return router.Parent
			}
		}
	case ListenerRecord:
		if listener, ok := record.(ListenerRecord); ok {
			if router, ok := c.Routers[listener.Parent]; ok {
				return router.Parent
			}
		}
	case ConnectorRecord:
		if connector, ok := record.(ConnectorRecord); ok {
			if router, ok := c.Routers[connector.Parent]; ok {
				return router.Parent
			}
		}
	case ProcessRecord:
		if process, ok := record.(ProcessRecord); ok {
			return process.Parent
		}
	case HostRecord:
		if host, ok := record.(HostRecord); ok {
			return host.Parent
		}
	default:
		return ""
	}
	return ""
}

func prettyPrint(i interface{}) string {
	s, _ := json.Marshal(i)
	return string(s)
}

func (fc *FlowCollector) addRecord(record interface{}) error {
	if record == nil {
		return fmt.Errorf("No record to add")
	}
	log.Printf("FLOW_LOG: %s\n", prettyPrint(record))

	switch record.(type) {
	case *SiteRecord:
		if site, ok := record.(*SiteRecord); ok {
			fc.Sites[site.Identity] = site
		}
	case *HostRecord:
		if host, ok := record.(*HostRecord); ok {
			fc.Hosts[host.Identity] = host
		}
	case *RouterRecord:
		if router, ok := record.(*RouterRecord); ok {
			fc.Routers[router.Identity] = router
		}
	case *LinkRecord:
		if link, ok := record.(*LinkRecord); ok {
			fc.Links[link.Identity] = link
		}
	case *ListenerRecord:
		if listener, ok := record.(*ListenerRecord); ok {
			fc.Listeners[listener.Identity] = listener
		}
	case *ConnectorRecord:
		if connector, ok := record.(*ConnectorRecord); ok {
			fc.Connectors[connector.Identity] = connector
		}
	case *ProcessRecord:
		if process, ok := record.(*ProcessRecord); ok {
			fc.Processes[process.Identity] = process
		}
	case *ProcessGroupRecord:
		if processGroup, ok := record.(*ProcessGroupRecord); ok {
			fc.ProcessGroups[processGroup.Identity] = processGroup
		}
	case *FlowAggregateRecord:
		if aggregate, ok := record.(*FlowAggregateRecord); ok {
			fc.FlowAggregates[aggregate.Identity] = aggregate
		}
	case *VanAddressRecord:
		if va, ok := record.(*VanAddressRecord); ok {
			fc.VanAddresses[va.Identity] = va
		}
	case *FlowRecord:
	default:
		return fmt.Errorf("Unknown record type to add")
	}
	return nil
}

func (fc *FlowCollector) deleteRecord(record interface{}) error {
	if record == nil {
		return fmt.Errorf("No record to delete")
	}
	log.Printf("FLOW_LOG: %s\n", prettyPrint(record))
	switch record.(type) {
	case *FlowRecord:
	case *SiteRecord:
		if site, ok := record.(*SiteRecord); ok {
			delete(fc.Sites, site.Identity)
		}
	case *HostRecord:
		if host, ok := record.(*HostRecord); ok {
			delete(fc.Hosts, host.Identity)
		}
	case *RouterRecord:
		if router, ok := record.(*RouterRecord); ok {
			delete(fc.Routers, router.Identity)
		}
	case *LinkRecord:
		if link, ok := record.(*LinkRecord); ok {
			delete(fc.Links, link.Identity)
		}
	case *ListenerRecord:
		if listener, ok := record.(*ListenerRecord); ok {
			delete(fc.Listeners, listener.Identity)
		}
	case *ConnectorRecord:
		if connector, ok := record.(*ConnectorRecord); ok {
			// keep around for flows that will be terminated
			fc.recentConnectors[connector.Identity] = connector
			delete(fc.Connectors, connector.Identity)
		}
	case *ProcessRecord:
		if process, ok := record.(*ProcessRecord); ok {
			delete(fc.Processes, process.Identity)
		}
	case *ProcessGroupRecord:
		if processGroup, ok := record.(*ProcessGroupRecord); ok {
			delete(fc.ProcessGroups, processGroup.Identity)
		}
	case *FlowAggregateRecord:
		if aggregate, ok := record.(*FlowAggregateRecord); ok {
			delete(fc.FlowAggregates, aggregate.Identity)
		}
	case *VanAddressRecord:
		if va, ok := record.(*VanAddressRecord); ok {
			delete(fc.VanAddresses, va.Identity)
		}
	default:
		return fmt.Errorf("Unknown record type to delete")
	}
	return nil
}

func (fc *FlowCollector) updateLastHeard(source string) error {
	if eventsource, ok := fc.eventSources[source]; ok {
		eventsource.LastHeard = uint64(time.Now().UnixNano()) / uint64(time.Microsecond)
		eventsource.Messages++
	}
	return nil
}

func (fc *FlowCollector) updateRecord(record interface{}) error {
	switch record.(type) {
	case HeartbeatRecord:
		if heartbeat, ok := record.(HeartbeatRecord); ok {
			if eventsource, ok := fc.eventSources[heartbeat.Identity]; ok {
				eventsource.LastHeard = uint64(time.Now().UnixNano()) / uint64(time.Microsecond)
				eventsource.Heartbeats++
				eventsource.Messages++
			}
			if pending, ok := fc.pendingFlush[heartbeat.Source]; ok {
				pending.heartbeat = true
			}
		}
	case FlowRecord:
	case SiteRecord:
		if site, ok := record.(SiteRecord); ok {
			if current, ok := fc.Sites[site.Identity]; !ok {
				if site.StartTime > 0 && site.EndTime == 0 {
					fc.addRecord(&site)
				}
			} else {
				if site.EndTime > 0 {
					current.EndTime = site.EndTime
					for _, aggregate := range fc.FlowAggregates {
						if aggregate.PairType == recordNames[Site] {
							if current.Identity == *aggregate.SourceId || current.Identity == *aggregate.DestinationId {
								aggregate.EndTime = current.EndTime
								fc.deleteRecord(aggregate)
							}
						}
					}
					fc.deleteRecord(current)
				} else {
					if site.Policy != nil {
						current.Policy = site.Policy
					}
				}
			}
			fc.updateLastHeard(site.Source)
		}
	case HostRecord:
		if host, ok := record.(HostRecord); ok {
			if current, ok := fc.Hosts[host.Identity]; !ok {
				if host.StartTime > 0 && host.EndTime == 0 {
					fc.addRecord(&host)
				}
			} else {
				if host.EndTime > 0 {
					current.EndTime = host.EndTime
					fc.deleteRecord(current)
				} else {
					*current = host
				}
			}
			fc.updateLastHeard(host.Source)
		}
	case RouterRecord:
		if router, ok := record.(RouterRecord); ok {
			if current, ok := fc.Routers[router.Identity]; !ok {
				if router.StartTime > 0 && router.EndTime == 0 {
					fc.addRecord(&router)
					if router.Parent != "" {
						fc.inferGatewaySite(router.Parent)
					}
				}
			} else {
				if router.EndTime > 0 {
					current.EndTime = router.EndTime
					fc.deleteRecord(current)
				} else if router.Parent != "" && current.Parent == "" {
					current.Parent = router.Parent
					if _, ok := fc.Sites[current.Parent]; !ok {
						fc.inferGatewaySite(current.Parent)
					}
				}
			}
			fc.updateLastHeard(router.Source)
		}
	case LogEventRecord:
		if logEvent, ok := record.(LogEventRecord); ok {
			log.Printf("LOG_EVENT: %s \n", prettyPrint(logEvent))
		}
	case LinkRecord:
		if link, ok := record.(LinkRecord); ok {
			if current, ok := fc.Links[link.Identity]; !ok {
				if link.StartTime > 0 && link.EndTime == 0 {
					fc.addRecord(&link)
				}
			} else {
				if link.EndTime > 0 {
					current.EndTime = link.EndTime
					fc.deleteRecord(current)
				}
			}
			fc.updateLastHeard(link.Source)
		}
	case ListenerRecord:
		if listener, ok := record.(ListenerRecord); ok {
			if current, ok := fc.Listeners[listener.Identity]; !ok {
				if listener.StartTime > 0 && listener.EndTime == 0 {
					if listener.Address != nil {
						var va *VanAddressRecord
						for _, y := range fc.VanAddresses {
							if y.Name == *listener.Address {
								va = y
								break
							}
						}
						if va == nil {
							va = &VanAddressRecord{
								Base: Base{
									RecType:   recordNames[Address],
									Identity:  uuid.New().String(),
									StartTime: listener.StartTime,
								},
								Name:     *listener.Address,
								Protocol: *listener.Protocol,
							}
							fc.addRecord(va)
						}
						listener.AddressId = &va.Identity
					}
					fc.addRecord(&listener)
				}
			} else {
				if listener.EndTime > 0 {
					current.EndTime = listener.EndTime
					if current.AddressId != nil {
						count := 0
						for id, l := range fc.Listeners {
							if id != current.Identity && l.AddressId != nil && *l.AddressId == *current.AddressId {
								count++
							}
						}
						if count == 0 {
							if va, ok := fc.VanAddresses[*current.AddressId]; ok {
								va.EndTime = listener.EndTime
								fc.deleteRecord(va)
							}
						}
					}
					fc.deleteRecord(current)
				} else {
					if current.Parent == "" && listener.Parent != "" {
						current.Parent = listener.Parent
					}
					if listener.FlowCountL4 != nil {
						current.FlowCountL4 = listener.FlowCountL4
					}
					if listener.FlowRateL4 != nil {
						current.FlowRateL4 = listener.FlowRateL4
					}
					if listener.FlowCountL7 != nil {
						current.FlowCountL7 = listener.FlowCountL7
					}
					if listener.FlowRateL7 != nil {
						current.FlowRateL7 = listener.FlowRateL7
					}
					if current.Address == nil && listener.Address != nil {
						current.Address = listener.Address
						var va *VanAddressRecord
						for _, y := range fc.VanAddresses {
							if y.Name == *listener.Address {
								va = y
								break
							}
						}
						if va == nil {
							t := time.Now()
							va = &VanAddressRecord{
								Base: Base{
									RecType:   recordNames[Address],
									Identity:  uuid.New().String(),
									StartTime: uint64(t.UnixNano()) / uint64(time.Microsecond),
								},
								Name:     *listener.Address,
								Protocol: *listener.Protocol,
							}
							fc.addRecord(va)
						}
						current.AddressId = &va.Identity
					}
				}
			}
			fc.updateLastHeard(listener.Source)
		}
	case ConnectorRecord:
		if connector, ok := record.(ConnectorRecord); ok {
			if current, ok := fc.Connectors[connector.Identity]; !ok {
				if connector.StartTime > 0 && connector.EndTime == 0 {
					if connector.Parent != "" {
						if connector.Address != nil {
							var va *VanAddressRecord
							for _, y := range fc.VanAddresses {
								if y.Name == *connector.Address {
									va = y
									break
								}
							}
							if va == nil {
								t := time.Now()
								va = &VanAddressRecord{
									Base: Base{
										RecType:   recordNames[Address],
										Identity:  uuid.New().String(),
										StartTime: uint64(t.UnixNano()) / uint64(time.Microsecond),
									},
									Name:     *connector.Address,
									Protocol: *connector.Protocol,
								}
								fc.VanAddresses[va.Identity] = va
								fc.addRecord(va)
							}
							connector.AddressId = &va.Identity
						}
						siteId := fc.getRecordSiteId(connector)
						if fc.isGatewaySite(siteId) && connector.DestHost != nil {
							fc.inferGatewayProcess(siteId, *connector.DestHost)
						}
					}
					fc.addRecord(&connector)
					fc.connectorsToReconcile[connector.Identity] = connector.Identity
				}
			} else {
				if connector.EndTime > 0 {
					current.EndTime = connector.EndTime
					if current.ProcessId != nil {
						if process, ok := fc.Processes[*current.ProcessId]; ok {
							process.connector = nil
							process.ProcessBinding = &Unbound
						}
					}
					// Note a new connector can create an address but does not delete it
					// removal of last listener will delete the address
					fc.deleteRecord(current)
				} else {
					if current.Parent == "" && connector.Parent != "" {
						current.Parent = connector.Parent
						siteId := fc.getRecordSiteId(current)
						if fc.isGatewaySite(siteId) && current.DestHost != nil {
							fc.inferGatewayProcess(siteId, *current.DestHost)
						}

					}
					if current.DestHost == nil && connector.DestHost != nil {
						current.DestHost = connector.DestHost
					}
					if connector.FlowCountL4 != nil {
						current.FlowCountL4 = connector.FlowCountL4
					}
					if connector.FlowRateL4 != nil {
						current.FlowRateL4 = connector.FlowRateL4
					}
					if connector.FlowCountL7 != nil {
						current.FlowCountL7 = connector.FlowCountL7
					}
					if connector.FlowRateL7 != nil {
						current.FlowRateL7 = connector.FlowRateL7
					}
					if current.Address == nil && connector.Address != nil {
						current.Address = connector.Address
						var va *VanAddressRecord
						for _, y := range fc.VanAddresses {
							if y.Name == *connector.Address {
								va = y
								break
							}
						}
						if va == nil {
							t := time.Now()
							va = &VanAddressRecord{
								Base: Base{
									RecType:   recordNames[Address],
									Identity:  uuid.New().String(),
									StartTime: uint64(t.UnixNano()) / uint64(time.Microsecond),
								},
								Name:     *connector.Address,
								Protocol: *connector.Protocol,
							}
							fc.addRecord(va)
						}
						current.AddressId = &va.Identity
					}
				}
			}
			fc.updateLastHeard(connector.Source)
		}
	case ProcessRecord:
		if process, ok := record.(ProcessRecord); ok {
			if current, ok := fc.Processes[process.Identity]; !ok {
				if process.StartTime > 0 && process.EndTime == 0 {
					if site, ok := fc.Sites[process.Parent]; ok {
						if site.Name != nil {
							process.ParentName = site.Name
						}
					}
					process.ProcessBinding = &Unbound
					for _, pg := range fc.ProcessGroups {
						if pg.EndTime == 0 && *process.GroupName == *pg.Name {
							process.GroupIdentity = &pg.Identity
							break
						}
					}
					if process.GroupIdentity == nil && process.GroupName != nil {
						pg := &ProcessGroupRecord{
							Base: Base{
								RecType:   recordNames[ProcessGroup],
								Identity:  uuid.New().String(),
								StartTime: uint64(time.Now().UnixNano()) / uint64(time.Microsecond),
							},
							Name:             process.GroupName,
							ProcessGroupRole: process.ProcessRole,
						}
						fc.updateRecord(*pg)
						process.GroupIdentity = &pg.Identity
					}
					fc.addRecord(&process)
				}
			} else {
				if process.EndTime > 0 {
					current.EndTime = process.EndTime
					// check if there are any process pairs active
					for _, aggregate := range fc.FlowAggregates {
						if aggregate.PairType == recordNames[Process] {
							if current.Identity == *aggregate.SourceId || current.Identity == *aggregate.DestinationId {
								aggregate.EndTime = current.EndTime
								fc.deleteRecord(aggregate)
							}
						}
					}
					if current.GroupIdentity != nil {
						count := 0
						for id, p := range fc.Processes {
							if id != current.Identity && *p.GroupIdentity == *current.GroupIdentity {
								count++
							}
						}
						if count == 0 {
							if processGroup, ok := fc.ProcessGroups[*current.GroupIdentity]; ok {
								processGroup.EndTime = process.EndTime
								fc.updateRecord(*processGroup)
							}
						}
					}
					fc.deleteRecord(current)
				}
			}
			fc.updateLastHeard(process.Source)
		}
	case ProcessGroupRecord:
		if processGroup, ok := record.(ProcessGroupRecord); ok {
			if current, ok := fc.ProcessGroups[processGroup.Identity]; !ok {
				if processGroup.StartTime > 0 && processGroup.EndTime == 0 {
					fc.addRecord(&processGroup)
				}
			} else {
				if processGroup.EndTime > 0 {
					current.EndTime = processGroup.EndTime
					// check if there are an processgroup pairs active
					for _, aggregate := range fc.FlowAggregates {
						if aggregate.PairType == recordNames[ProcessGroup] {
							if current.Identity == *aggregate.SourceId || current.Identity == *aggregate.DestinationId {
								aggregate.EndTime = current.EndTime
								fc.deleteRecord(aggregate)
							}
						}
					}
					fc.deleteRecord(current)
				}
			}
			fc.updateLastHeard(processGroup.Source)
		}
	default:
		return fmt.Errorf("Unrecognized record type %T", record)
	}

	return nil
}

type linkResponseHandler struct {
	siteByRouterID   map[string]string
	siteByRouterName map[string]string
}

func newLinkResponseHandler(sites map[string]*SiteRecord, routers map[string]*RouterRecord) linkResponseHandler {
	builder := linkResponseHandler{
		siteByRouterID:   make(map[string]string, len(routers)),
		siteByRouterName: make(map[string]string, len(routers)),
	}
	for _, router := range routers {
		_, ok := sites[router.Parent]
		if !ok || router.Name == nil {
			continue
		}
		// router names are prefixed with a routing area - so far always `0/`
		normalizedName := *router.Name
		if delim := strings.IndexRune(normalizedName, '/'); delim > -1 {
			normalizedName = normalizedName[delim+1:]
		}
		builder.siteByRouterName[normalizedName] = router.Parent
		builder.siteByRouterID[router.Identity] = router.Parent
	}
	return builder
}

func (b linkResponseHandler) handle(l LinkRecord) (linkRecordResponse, bool) {
	var (
		ok   bool
		resp = linkRecordResponse{LinkRecord: l}
	)
	if l.Name == nil || l.Direction == nil {
		return resp, false
	}

	if resp.SourceSiteId, ok = b.siteByRouterID[l.Parent]; !ok {
		return resp, false
	}

	if resp.DestinationSiteId, ok = b.siteByRouterName[*l.Name]; !ok {
		return resp, false
	}
	if *l.Direction == Incoming {
		resp.SourceSiteId, resp.DestinationSiteId = resp.DestinationSiteId, resp.SourceSiteId
	}

	return resp, true
}

func (fc *FlowCollector) retrieve(request ApiRequest) (*string, error) {
	vars := mux.Vars(request.Request)
	url := request.Request.URL
	queryParams := getQueryParams(url)
	var retrieveError error = nil

	p := Payload{
		Results:        nil,
		Status:         "",
		Count:          0,
		TimeRangeCount: 0,
		TotalCount:     0,
		timestamp:      uint64(time.Now().UnixNano()) / uint64(time.Microsecond),
		elapsed:        0,
	}

	switch request.RecordType {
	case Site:
		switch request.HandlerName {
		case "list":
			sites := []SiteRecord{}
			for _, site := range fc.Sites {
				if filterRecord(*site, queryParams) && site.Base.TimeRangeValid(queryParams) {
					sites = append(sites, *site)
				}
			}
			p.TotalCount = len(fc.Sites)
			retrieveError = sortAndSlice(sites, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if site, ok := fc.Sites[id]; ok {
					p.Count = 1
					p.Results = site
				}
			}
		case "processes":
			processes := []ProcessRecord{}
			if id, ok := vars["id"]; ok {
				if site, ok := fc.Sites[id]; ok {
					for _, process := range fc.Processes {
						if process.Parent == site.Identity {
							p.TotalCount++
							if filterRecord(*process, queryParams) && process.Base.TimeRangeValid(queryParams) {
								processes = append(processes, *process)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(processes, &p, queryParams)
		case "routers":
			routers := []RouterRecord{}
			if id, ok := vars["id"]; ok {
				if site, ok := fc.Sites[id]; ok {
					for _, router := range fc.Routers {
						if router.Parent == site.Identity {
							p.TotalCount++
							if filterRecord(*router, queryParams) && router.Base.TimeRangeValid(queryParams) {
								routers = append(routers, *router)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(routers, &p, queryParams)
		case "links":
			links := []linkRecordResponse{}
			linkHandler := newLinkResponseHandler(fc.Sites, fc.Routers)
			if id, ok := vars["id"]; ok {
				if site, ok := fc.Sites[id]; ok {
					for _, link := range fc.Links {
						if fc.getRecordSiteId(*link) == site.Identity {
							lr, ok := linkHandler.handle(*link)
							if !ok {
								continue
							}
							p.TotalCount++
							if filterRecord(lr, queryParams) && link.Base.TimeRangeValid(queryParams) {
								links = append(links, lr)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(links, &p, queryParams)
		case "hosts":
			hosts := []HostRecord{}
			if id, ok := vars["id"]; ok {
				if site, ok := fc.Sites[id]; ok {
					for _, host := range fc.Hosts {
						if host.Parent == site.Identity {
							p.TotalCount++
							if filterRecord(*host, queryParams) && host.Base.TimeRangeValid(queryParams) {
								hosts = append(hosts, *host)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(hosts, &p, queryParams)
		}
	case Host:
		switch request.HandlerName {
		case "list":
			hosts := []HostRecord{}
			for _, host := range fc.Hosts {
				if filterRecord(*host, queryParams) && host.Base.TimeRangeValid(queryParams) {
					hosts = append(hosts, *host)
				}
			}
			p.TotalCount = len(fc.Hosts)
			retrieveError = sortAndSlice(hosts, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if host, ok := fc.Hosts[id]; ok {
					p.Count = 1
					p.Results = host
				}
			}
		}
	case Router:
		switch request.HandlerName {
		case "list":
			routers := []RouterRecord{}
			for _, router := range fc.Routers {
				if filterRecord(*router, queryParams) && router.Base.TimeRangeValid(queryParams) {
					routers = append(routers, *router)
				}
			}
			p.TotalCount = len(fc.Routers)
			retrieveError = sortAndSlice(routers, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if router, ok := fc.Routers[id]; ok {
					p.Count = 1
					p.Results = router
				}
			}
		case "flows":
		case "links":
			links := []linkRecordResponse{}
			linkHandler := newLinkResponseHandler(fc.Sites, fc.Routers)
			if id, ok := vars["id"]; ok {
				if router, ok := fc.Routers[id]; ok {
					for _, link := range fc.Links {
						if link.Parent == router.Identity {
							lr, ok := linkHandler.handle(*link)
							if !ok {
								continue
							}
							p.TotalCount++
							if filterRecord(lr, queryParams) && link.Base.TimeRangeValid(queryParams) {
								links = append(links, lr)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(links, &p, queryParams)
		case "listeners":
			listeners := []ListenerRecord{}
			if id, ok := vars["id"]; ok {
				if router, ok := fc.Routers[id]; ok {
					for _, listener := range fc.Listeners {
						if listener.Parent == router.Identity {
							p.TotalCount++
							if filterRecord(*listener, queryParams) && listener.Base.TimeRangeValid(queryParams) {
								listeners = append(listeners, *listener)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(listeners, &p, queryParams)
		case "connectors":
			connectors := []ConnectorRecord{}
			if id, ok := vars["id"]; ok {
				if router, ok := fc.Routers[id]; ok {
					for _, connector := range fc.Connectors {
						if connector.Parent == router.Identity {
							p.TotalCount++
							if filterRecord(*connector, queryParams) && connector.Base.TimeRangeValid(queryParams) {
								connectors = append(connectors, *connector)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(connectors, &p, queryParams)
		}
	case Link:
		linkHandler := newLinkResponseHandler(fc.Sites, fc.Routers)
		switch request.HandlerName {
		case "list":
			links := []linkRecordResponse{}
			for _, link := range fc.Links {
				lr, ok := linkHandler.handle(*link)
				if !ok {
					continue
				}
				if filterRecord(lr, queryParams) && link.Base.TimeRangeValid(queryParams) {
					links = append(links, lr)
				}
			}
			p.TotalCount = len(fc.Links)
			retrieveError = sortAndSlice(links, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if link, ok := fc.Links[id]; ok {
					lr, ok := linkHandler.handle(*link)
					if ok {
						p.Count = 1
						p.Results = lr
					}
				}
			}
		}
	case Listener:
		switch request.HandlerName {
		case "list":
			listeners := []ListenerRecord{}
			for _, listener := range fc.Listeners {
				if filterRecord(*listener, queryParams) && listener.Base.TimeRangeValid(queryParams) {
					listeners = append(listeners, *listener)
				}
			}
			p.TotalCount = len(fc.Listeners)
			retrieveError = sortAndSlice(listeners, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if listener, ok := fc.Listeners[id]; ok {
					p.Count = 1
					p.Results = listener
				}
			}
		case "flows":
		}
	case Connector:
		switch request.HandlerName {
		case "list":
			connectors := []ConnectorRecord{}
			for _, connector := range fc.Connectors {
				if filterRecord(*connector, queryParams) && connector.Base.TimeRangeValid(queryParams) {
					connectors = append(connectors, *connector)
				}
			}
			p.TotalCount = len(fc.Connectors)
			retrieveError = sortAndSlice(connectors, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if connector, ok := fc.Connectors[id]; ok {
					p.Count = 1
					p.Results = connector
				}
			}
		case "flows":
		case "process":
			if id, ok := vars["id"]; ok {
				if connector, ok := fc.Connectors[id]; ok {
					if connector.ProcessId != nil {
						if process, ok := fc.Processes[*connector.ProcessId]; ok {
							p.Count = 1
							p.Results = *process
						}
					}
				}
			}
		}
	case Address:
		switch request.HandlerName {
		case "list":
			addresses := []VanAddressRecord{}
			for _, address := range fc.VanAddresses {
				if filterRecord(*address, queryParams) {
					fc.getAddressAdaptorCounts(address)
					addresses = append(addresses, *address)
				}
			}
			p.TotalCount = len(fc.VanAddresses)
			retrieveError = sortAndSlice(addresses, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if address, ok := fc.VanAddresses[id]; ok {
					fc.getAddressAdaptorCounts(address)
					p.Count = 1
					p.Results = address
				}
			}
		case "processes":
			processes := []ProcessRecord{}
			unique := make(map[string]*ProcessRecord)
			if id, ok := vars["id"]; ok {
				if vanaddr, ok := fc.VanAddresses[id]; ok {
					for _, connector := range fc.Connectors {
						if *connector.Address == vanaddr.Name && connector.ProcessId != nil {
							if process, ok := fc.Processes[*connector.ProcessId]; ok {
								if filterRecord(*process, queryParams) && process.Base.TimeRangeValid(queryParams) {
									unique[process.Identity] = process
								}
							}
						}
					}
					for _, process := range unique {
						processes = append(processes, *process)
					}
				}
			}
			p.TotalCount = len(unique)
			retrieveError = sortAndSlice(processes, &p, queryParams)
		case "processpairs":
			processPairs := []FlowAggregateRecord{}
			if id, ok := vars["id"]; ok {
				if vanaddr, ok := fc.VanAddresses[id]; ok {
					for _, connector := range fc.Connectors {
						if *connector.Address == vanaddr.Name && connector.ProcessId != nil {
							for _, aggregate := range fc.FlowAggregates {
								if aggregate.PairType == recordNames[Process] {
									if *connector.ProcessId == *aggregate.DestinationId {
										p.TotalCount++
										if filterRecord(*aggregate, queryParams) {
											processPairs = append(processPairs, *aggregate)
										}
									}
								}
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(processPairs, &p, queryParams)
		case "listeners":
			listeners := []ListenerRecord{}
			if id, ok := vars["id"]; ok {
				if vanaddr, ok := fc.VanAddresses[id]; ok {
					for _, listener := range fc.Listeners {
						if *listener.Address == vanaddr.Name {
							p.TotalCount++
							if filterRecord(*listener, queryParams) && listener.Base.TimeRangeValid(queryParams) {
								listeners = append(listeners, *listener)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(listeners, &p, queryParams)
		case "connectors":
			connectors := []ConnectorRecord{}
			if id, ok := vars["id"]; ok {
				if vanaddr, ok := fc.VanAddresses[id]; ok {
					for _, connector := range fc.Connectors {
						if *connector.Address == vanaddr.Name {
							p.TotalCount++
							if filterRecord(*connector, queryParams) && connector.Base.TimeRangeValid(queryParams) {
								connectors = append(connectors, *connector)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(connectors, &p, queryParams)
		}
	case Process:
		switch request.HandlerName {
		case "list":
			processes := []ProcessRecord{}
			for _, process := range fc.Processes {
				if filterRecord(*process, queryParams) && process.Base.TimeRangeValid(queryParams) {
					if process.connector != nil {
						process.Addresses = nil
						if connector, ok := fc.Connectors[*process.connector]; ok {
							if connector.Address != nil && connector.AddressId != nil {
								addrDetails := *connector.Address + "@" + *connector.AddressId + "@" + *connector.Protocol
								process.Addresses = append(process.Addresses, &addrDetails)
							}
						}
					}
					processes = append(processes, *process)
				}
			}
			p.TotalCount = len(fc.Processes)
			retrieveError = sortAndSlice(processes, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if process, ok := fc.Processes[id]; ok {
					p.Count = 1
					p.Results = process
				}
			}
		case "addresses":
			addresses := []VanAddressRecord{}
			if id, ok := vars["id"]; ok {
				if _, ok := fc.Processes[id]; ok {
					for _, connector := range fc.Connectors {
						if connector.ProcessId != nil && *connector.ProcessId == id {
							for _, address := range fc.VanAddresses {
								if *connector.Address == address.Name {
									if filterRecord(*address, queryParams) {
										fc.getAddressAdaptorCounts(address)
										addresses = append(addresses, *address)
									}
								}
							}
						}
					}
				}
			}
			p.TotalCount = len(fc.VanAddresses)
			retrieveError = sortAndSlice(addresses, &p, queryParams)
		case "connector":
			if id, ok := vars["id"]; ok {
				if process, ok := fc.Processes[id]; ok {
					if process.connector != nil {
						if connector, ok := fc.Connectors[*process.connector]; ok {
							p.Count = 1
							p.Results = *connector
						}
					}
				}
			}
		}
	case ProcessGroup:
		switch request.HandlerName {
		case "list":
			processGroups := []ProcessGroupRecord{}
			for _, processGroup := range fc.ProcessGroups {
				count := 0
				for _, process := range fc.Processes {
					if *process.GroupIdentity == processGroup.Identity {
						count++
					}
				}
				processGroup.ProcessCount = count
				p.TotalCount++
				if filterRecord(*processGroup, queryParams) && processGroup.Base.TimeRangeValid(queryParams) {
					processGroups = append(processGroups, *processGroup)
				}
			}
			retrieveError = sortAndSlice(processGroups, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if processGroup, ok := fc.ProcessGroups[id]; ok {
					count := 0
					for _, process := range fc.Processes {
						if *process.GroupIdentity == processGroup.Identity {
							count++
						}
					}
					processGroup.ProcessCount = count
					p.Count = 1
					p.Results = processGroup
				}
			}
		case "processes":
			processes := []ProcessRecord{}
			if id, ok := vars["id"]; ok {
				if processGroup, ok := fc.ProcessGroups[id]; ok {
					for _, process := range fc.Processes {
						if *process.GroupIdentity == processGroup.Identity {
							p.TotalCount++
							if filterRecord(*process, queryParams) && process.Base.TimeRangeValid(queryParams) {
								processes = append(processes, *process)
							}
						}
					}
				}
			}
			retrieveError = sortAndSlice(processes, &p, queryParams)
		}
	case SitePair:
		sourceId := url.Query().Get("sourceId")
		destinationId := url.Query().Get("destinationId")
		switch request.HandlerName {
		case "list":
			aggregates := []FlowAggregateRecord{}
			for _, aggregate := range fc.FlowAggregates {
				if aggregate.PairType == recordNames[Site] {
					p.TotalCount++
					if sourceId == "" && destinationId == "" ||
						sourceId == *aggregate.SourceId && destinationId == "" ||
						sourceId == "" && destinationId == *aggregate.DestinationId ||
						sourceId == *aggregate.SourceId && destinationId == *aggregate.DestinationId {
						if filterRecord(*aggregate, queryParams) {
							aggregates = append(aggregates, *aggregate)
						}
					}
				}
			}
			retrieveError = sortAndSlice(aggregates, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if flowAggregate, ok := fc.FlowAggregates[id]; ok {
					if flowAggregate.PairType == recordNames[Site] {
						p.Count = 1
						p.Results = flowAggregate
					}
				}
			}
		}
	case ProcessGroupPair:
		sourceId := url.Query().Get("sourceId")
		destinationId := url.Query().Get("destinationId")
		switch request.HandlerName {
		case "list":
			aggregates := []FlowAggregateRecord{}
			for _, aggregate := range fc.FlowAggregates {
				if aggregate.PairType == recordNames[ProcessGroup] {
					p.TotalCount++
					if sourceId == "" && destinationId == "" ||
						sourceId == *aggregate.SourceId && destinationId == "" ||
						sourceId == "" && destinationId == *aggregate.DestinationId ||
						sourceId == *aggregate.SourceId && destinationId == *aggregate.DestinationId {
						if filterRecord(*aggregate, queryParams) {
							aggregates = append(aggregates, *aggregate)
						}
					}
				}
			}
			retrieveError = sortAndSlice(aggregates, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if flowAggregate, ok := fc.FlowAggregates[id]; ok {
					if flowAggregate.PairType == recordNames[ProcessGroup] {
						p.Count = 1
						p.Results = flowAggregate
					}
				}
			}
		}
	case ProcessPair:
		sourceId := url.Query().Get("sourceId")
		destinationId := url.Query().Get("destinationId")
		switch request.HandlerName {
		case "list":
			aggregates := []FlowAggregateRecord{}
			for _, aggregate := range fc.FlowAggregates {
				if aggregate.PairType == recordNames[Process] {
					p.TotalCount++
					if sourceId == "" && destinationId == "" ||
						sourceId == *aggregate.SourceId && destinationId == "" ||
						sourceId == "" && destinationId == *aggregate.DestinationId ||
						sourceId == *aggregate.SourceId && destinationId == *aggregate.DestinationId {
						// try to associate a protocol to the process pair
						if process, ok := fc.Processes[*aggregate.DestinationId]; ok {
							if process.connector != nil {
								if connector, ok := fc.Connectors[*process.connector]; ok {
									aggregate.Protocol = connector.Protocol
								}
							}
						}
						if filterRecord(*aggregate, queryParams) {
							aggregates = append(aggregates, *aggregate)
						}
					}
				}
			}
			retrieveError = sortAndSlice(aggregates, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if flowAggregate, ok := fc.FlowAggregates[id]; ok {
					if flowAggregate.PairType == recordNames[Process] {
						if process, ok := fc.Processes[*flowAggregate.DestinationId]; ok {
							if process.connector != nil {
								if connector, ok := fc.Connectors[*process.connector]; ok {
									flowAggregate.Protocol = connector.Protocol
								}
							}
						}
						p.Count = 1
						p.Results = flowAggregate
					}
				}
			}
		}
	case FlowAggregate:
		sourceId := url.Query().Get("sourceId")
		destinationId := url.Query().Get("destinationId")
		switch request.HandlerName {
		case "sitepair-list":
			aggregates := []FlowAggregateRecord{}
			for _, aggregate := range fc.FlowAggregates {
				if aggregate.PairType == recordNames[Site] {
					p.TotalCount++
					if sourceId == "" && destinationId == "" ||
						sourceId == *aggregate.SourceId && destinationId == "" ||
						sourceId == "" && destinationId == *aggregate.DestinationId ||
						sourceId == *aggregate.SourceId && destinationId == *aggregate.DestinationId {
						aggregates = append(aggregates, *aggregate)
					}
				}
			}
			retrieveError = sortAndSlice(aggregates, &p, queryParams)
		case "sitepair-item":
			if id, ok := vars["id"]; ok {
				if flowAggregate, ok := fc.FlowAggregates[id]; ok {
					if flowAggregate.PairType == recordNames[Site] {
						p.Count = 1
						p.Results = flowAggregate
					}
				}
			}
		case "processpair-list":
			aggregates := []FlowAggregateRecord{}
			for _, aggregate := range fc.FlowAggregates {
				if aggregate.PairType == recordNames[Process] {
					p.TotalCount++
					if sourceId == "" && destinationId == "" ||
						sourceId == *aggregate.SourceId && destinationId == "" ||
						sourceId == "" && destinationId == *aggregate.DestinationId ||
						sourceId == *aggregate.SourceId && destinationId == *aggregate.DestinationId {
						aggregates = append(aggregates, *aggregate)
					}
				}
			}
			retrieveError = sortAndSlice(aggregates, &p, queryParams)
		case "processpair-item":
			if id, ok := vars["id"]; ok {
				if flowAggregate, ok := fc.FlowAggregates[id]; ok {
					if flowAggregate.PairType == recordNames[Process] {
						p.Count = 1
						p.Results = flowAggregate
					}
				}
			}
		case "processgrouppair-list":
			aggregates := []FlowAggregateRecord{}
			for _, aggregate := range fc.FlowAggregates {
				if aggregate.PairType == recordNames[ProcessGroup] {
					p.TotalCount++
					if sourceId == "" && destinationId == "" ||
						sourceId == *aggregate.SourceId && destinationId == "" ||
						sourceId == "" && destinationId == *aggregate.DestinationId ||
						sourceId == *aggregate.SourceId && destinationId == *aggregate.DestinationId {
						aggregates = append(aggregates, *aggregate)
					}
				}
			}
			retrieveError = sortAndSlice(aggregates, &p, queryParams)
		case "processgrouppair-item":
			if id, ok := vars["id"]; ok {
				if flowAggregate, ok := fc.FlowAggregates[id]; ok {
					if flowAggregate.PairType == recordNames[ProcessGroup] {
						p.Count = 1
						p.Results = flowAggregate
					}
				}
			}
		}
	case EventSource:
		switch request.HandlerName {
		case "list":
			eventSources := []EventSourceRecord{}
			for _, eventSource := range fc.eventSources {
				if filterRecord(eventSource.EventSourceRecord, queryParams) && eventSource.EventSourceRecord.Base.TimeRangeValid(queryParams) {
					eventSources = append(eventSources, eventSource.EventSourceRecord)
				}
			}
			p.TotalCount = len(fc.eventSources)
			retrieveError = sortAndSlice(eventSources, &p, queryParams)
		case "item":
			if id, ok := vars["id"]; ok {
				if eventSource, ok := fc.eventSources[id]; ok {
					p.Count = 1
					p.Results = eventSource.EventSourceRecord
				}
			}
		}
	case Collector:
		// TODO: emit and collect Collector records
		switch request.HandlerName {
		case "list":
			collectors := []CollectorRecord{}
			collectors = append(collectors, fc.Collector)
			p.TotalCount = len(collectors)
			retrieveError = sortAndSlice(collectors, &p, queryParams)
		case "item":
			p.Count = 1
			p.Results = &fc.Collector
		case "connectors-to-process":
			connectors := []string{}
			for _, connId := range fc.connectorsToReconcile {
				if _, ok := fc.Connectors[connId]; ok {
					p.TotalCount++
					connectors = append(connectors, connId)
				}
			}
			retrieveError = sortAndSlice(connectors, &p, queryParams)
		}
	default:
		log.Println("COLLECTOR: Unrecognize record request", request.RecordType)
	}
	if retrieveError != nil {
		p.Status = retrieveError.Error()
	}
	p.elapsed = uint64(time.Now().UnixNano())/uint64(time.Microsecond) - p.timestamp
	apiQueryLatencyMetric, err := fc.metrics.apiQueryLatency.GetMetricWith(map[string]string{"recordType": recordNames[request.RecordType], "handler": request.HandlerName})
	if err == nil {
		apiQueryLatencyMetric.Observe(float64(p.elapsed))
	}
	data, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		log.Println("COLLECTOR: Error marshalling results", err.Error())
		return nil, err
	}
	sd := string(data)
	return &sd, nil
}

func (fc *FlowCollector) reconcileConnectorRecords() error {
	for _, connId := range fc.connectorsToReconcile {
		t := time.Now()
		if connector, ok := fc.Connectors[connId]; ok {
			if connector.EndTime > 0 {
				delete(fc.connectorsToReconcile, connId)
			} else if connector.DestHost != nil {
				siteId := fc.getRecordSiteId(*connector)
				var matchHost *string
				found := false
				if net.ParseIP(*connector.DestHost) == nil {
					addrs, err := net.LookupHost(*connector.DestHost)
					if err == nil && len(addrs) > 0 {
						matchHost = &addrs[0]
					}
				} else {
					matchHost = connector.DestHost
				}
				for _, process := range fc.Processes {
					if siteId == process.Parent {
						if process.SourceHost != nil && matchHost != nil {
							if *matchHost == *process.SourceHost {
								found = true
							}
						} else if process.HostName != nil {
							if *process.HostName == *connector.DestHost {
								found = true
							}
						}
						if found {
							connector.ProcessId = &process.Identity
							connector.Target = process.Name
							process.connector = &connector.Identity
							process.ProcessBinding = &Bound
							log.Printf("COLLECTOR: Connector %s/%s associated to process %s\n", connector.Identity, *connector.Address, *process.Name)
							delete(fc.connectorsToReconcile, connId)
							break
						}
					}
				}
				if !found {
					parts := strings.Split(siteId, "-")
					processName := "site-servers-" + parts[0]
					diffTime := connector.StartTime
					wait := 30 * oneSecond
					if fc.startTime > connector.StartTime {
						diffTime = fc.startTime
						wait = 120 * oneSecond
					}
					diff := uint64(t.UnixNano())/uint64(time.Microsecond) - diffTime
					if diff > wait {
						for _, process := range fc.Processes {
							if process.Name != nil && *process.Name == processName {
								log.Printf("COLLECTOR: Associating connector %s to external process %s\n", connector.Identity, processName)
								connector.ProcessId = &process.Identity
								delete(fc.connectorsToReconcile, connId)
								break
							}
						}
					}
				}
			}
		} else {
			delete(fc.connectorsToReconcile, connId)
		}
	}
	return nil
}

func (fc *FlowCollector) ageAndPurgeRecords() error {
	t := time.Now()
	for _, source := range fc.eventSources {
		diff := uint64(t.UnixNano())/uint64(time.Microsecond) - source.EventSourceRecord.LastHeard
		if diff > 60*oneSecond {
			log.Printf("COLLECTOR: Purging event source %s of type %s \n", source.Beacon.Identity, source.Beacon.SourceType)
			fc.purgeEventSource(source.EventSourceRecord)
		}
	}

	// recentConnectors for flows after the fact
	for _, connector := range fc.recentConnectors {
		diff := uint64(t.UnixNano())/uint64(time.Microsecond) - connector.EndTime
		if diff > 120*oneSecond {
			delete(fc.recentConnectors, connector.Identity)
		}
	}

	return nil
}

func (fc *FlowCollector) getAddressAdaptorCounts(addr *VanAddressRecord) error {
	listenerCount := 0
	connectorCount := 0
	for _, listener := range fc.Listeners {
		if listener.Address != nil && *listener.Address == addr.Name {
			listenerCount++
		}
	}
	for _, connector := range fc.Connectors {
		if connector.Address != nil && *connector.Address == addr.Name {
			connectorCount++
		}
	}
	addr.ListenerCount = listenerCount
	addr.ConnectorCount = connectorCount

	return nil
}

func (fc *FlowCollector) purgeEventSource(eventSource EventSourceRecord) error {
	// it would be good to indicate the reason
	t := time.Now()
	now := uint64(t.UnixNano()) / uint64(time.Microsecond)

	switch eventSource.Beacon.SourceType {
	case recordNames[Router]:
		for _, listener := range fc.Listeners {
			if listener.Parent == eventSource.Identity {
				listener.EndTime = now
				listener.Purged = true
				fc.updateRecord(*listener)
			}
		}
		for _, connector := range fc.Connectors {
			if connector.Parent == eventSource.Identity {
				connector.EndTime = now
				fc.updateRecord(*connector)
			}
		}
		for _, link := range fc.Links {
			if link.Parent == eventSource.Identity {
				link.EndTime = now
				link.Purged = true
				fc.updateRecord(*link)
			}
		}
		if router, ok := fc.Routers[eventSource.Identity]; ok {
			// workaround for gateway site
			if fc.isGatewaySite(router.Parent) {
				for _, process := range fc.Processes {
					if process.Parent == router.Parent {
						process.EndTime = now
						process.Purged = true
						fc.updateRecord(*process)
					}
				}
				if site, ok := fc.Sites[router.Parent]; ok {
					site.EndTime = now
					site.Purged = true
					fc.updateRecord(*site)
				}
			}
			router.EndTime = now
			router.Purged = true
			fc.updateRecord(*router)
		}
	case recordNames[Controller]:
		for _, process := range fc.Processes {
			if process.Parent == eventSource.Identity {
				process.EndTime = now
				process.Purged = true
				fc.updateRecord(*process)
			}
		}
		for _, host := range fc.Hosts {
			if host.Parent == eventSource.Identity {
				host.EndTime = now
				host.Purged = true
				fc.updateRecord(*host)
			}
		}
		for _, site := range fc.Sites {
			if site.Identity == eventSource.Identity {
				site.EndTime = now
				site.Purged = true
				fc.updateRecord(*site)
			}
		}
	}
	for id, es := range fc.eventSources {
		if id == eventSource.Identity {
			for _, receiver := range es.receivers {
				receiver.stop()
			}
			es.send.sender.stop()
		}
	}
	eventSource.Purged = true
	eventSource.EndTime = now
	log.Printf("COLLECTOR: %s \n", prettyPrint(eventSource))
	delete(fc.eventSources, eventSource.Identity)

	return nil
}
