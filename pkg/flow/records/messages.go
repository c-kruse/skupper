package records

import (
	"github.com/interconnectedcloud/go-amqp"
)

type BeaconMessage struct {
	Version    uint32
	SourceType string
	Address    string
	Direct     string
	Identity   string
}

func DecodeBeacon(msg *amqp.Message) BeaconMessage {
	var m BeaconMessage
	if version, ok := msg.ApplicationProperties["v"].(uint32); ok {
		m.Version = version
	}
	if sourceType, ok := msg.ApplicationProperties["sourceType"].(string); ok {
		m.SourceType = sourceType
	}
	if address, ok := msg.ApplicationProperties["address"].(string); ok {
		m.Address = address
	}
	if direct, ok := msg.ApplicationProperties["direct"].(string); ok {
		m.Direct = direct
	}
	if identity, ok := msg.ApplicationProperties["id"].(string); ok {
		m.Identity = identity
	}
	return m
}

func (m *BeaconMessage) Encode() *amqp.Message {
	return &amqp.Message{
		Properties: &amqp.MessageProperties{
			To:      "mc/sfe.all",
			Subject: "BEACON",
		},
		ApplicationProperties: map[string]interface{}{
			"v":          m.Version,
			"sourceType": m.SourceType,
			"address":    m.Address,
			"direct":     m.Direct,
			"id":         m.Identity,
		},
	}
}

type HeartbeatMessage struct {
	Source   string
	Identity string
	Version  uint32
	Now      uint64
}

func DecodeHeartbeat(msg *amqp.Message) HeartbeatMessage {
	var m HeartbeatMessage
	m.Source = msg.Properties.To

	if version, ok := msg.ApplicationProperties["v"].(uint32); ok {
		m.Version = version
	}
	if now, ok := msg.ApplicationProperties["now"].(uint64); ok {
		m.Now = now
	}
	if identity, ok := msg.ApplicationProperties["id"].(string); ok {
		m.Identity = identity
	}
	return m
}

func (m *HeartbeatMessage) Encode() *amqp.Message {
	return &amqp.Message{
		Properties: &amqp.MessageProperties{
			To:      "mc/sfe." + m.Identity,
			Subject: "HEARTBEAT",
		},
		ApplicationProperties: map[string]interface{}{
			"v":   m.Version,
			"now": m.Now,
			"id":  m.Identity,
		},
	}
}

type FlushMessage struct {
	Address string
	Source  string
}

func DecodeFlush(msg *amqp.Message) FlushMessage {
	return FlushMessage{
		Address: msg.Properties.To,
		Source:  msg.Properties.ReplyTo,
	}
}

func (m *FlushMessage) Encode() *amqp.Message {
	return &amqp.Message{
		Properties: &amqp.MessageProperties{
			To:      m.Address,
			Subject: "FLUSH",
		},
	}
}

type RecordMessage struct {
	Address string
	Records []Record
}

func DecodeRecord(msg *amqp.Message) (RecordMessage, error) {
	record := RecordMessage{
		Address: msg.Properties.To,
	}
	var err error
	record.Records, err = decodeRecords(msg)
	return record, err
}

func (m RecordMessage) Encode() *amqp.Message {
	var records []interface{}
	for _, record := range m.Records {
		records = append(records, record.AttributeSet())
	}
	return &amqp.Message{
		Properties: &amqp.MessageProperties{
			To:      m.Address,
			Subject: "RECORD",
		},
		Value: records,
	}
}
