package v1

import (
	"fmt"

	"github.com/interconnectedcloud/go-amqp"
)

const (
	typeOfRecord  uint32 = 0
	identityAttr  uint32 = 1
	parentAttr    uint32 = 2
	startTimeAttr uint32 = 3
	endTimeAttr   uint32 = 4
)

type Record interface {
	// Encode returns a map containing the records attribute set in a
	// format ready for an amqp.Message Value.
	Encode() map[uint32]any
}

type recordDecoder func(map[interface{}]interface{}) (Record, error)

// decodeRecords decodes an AMQP Message into a set of Records. Uses the
// recordDecoders map to find the correct decoder for each record type.
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
		typeOfRecord, err := getFromAttributeSet[uint32](valueMap, typeOfRecord)
		if err != nil {
			return records, fmt.Errorf("error getting record type attribute: %w", err)
		}

		decode, ok := recordDecoders[recordType(typeOfRecord)]
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

// setOpt sets attributeSet[codePoint] = opt when opt is non-nil
func setOpt[T any](attributeSet map[uint32]any, codePoint uint32, opt *T) {
	if opt == nil {
		return
	}
	attributeSet[uint32(codePoint)] = *opt
}

// getFromAttributeSet gets a value from the attributeSet map with they key
// codePoint. When there is no value or it is not of the expected type it will
// return an error.
func getFromAttributeSet[T any](attributeSet map[any]any, codePoint uint32) (T, error) {
	var attribute T
	raw, ok := attributeSet[uint32(codePoint)]
	if !ok {
		return attribute, fmt.Errorf("attribute set did not contain codepoint %d", codePoint)
	}
	attribute, ok = raw.(T)
	if !ok {
		return attribute, fmt.Errorf("attribute set contained a value of type %T but expected %T", raw, attribute)
	}

	return attribute, nil
}

// getFromAttributeSetOpt optionally gets a value from the attributeSet map
// with the key codePoint. When there is no value it will return nil, and when
// it is of the incorrect type it will return an error.
func getFromAttributeSetOpt[T any](attributeSet map[any]any, codePoint uint32) (*T, error) {
	raw, ok := attributeSet[codePoint]
	if !ok {
		return nil, nil
	}
	out, ok := raw.(T)
	if !ok {
		return nil, fmt.Errorf("attribute set contained a value of type %T but expected %T", raw, out)
	}
	return &out, nil
}

type BaseRecord struct {
	Identity  string
	Parent    *string
	StartTime *uint64
	EndTime   *uint64
}

func (r BaseRecord) Encode() map[uint32]any {
	attributeSet := make(map[uint32]any)
	attributeSet[uint32(identityAttr)] = r.Identity
	setOpt(attributeSet, parentAttr, r.Parent)
	setOpt(attributeSet, startTimeAttr, r.StartTime)
	setOpt(attributeSet, endTimeAttr, r.EndTime)
	return attributeSet
}

func decodeBase(attributeSet map[any]any) (BaseRecord, error) {
	var base BaseRecord
	id, err := getFromAttributeSet[string](attributeSet, identityAttr)
	if err != nil {
		return base, fmt.Errorf("error getting record Identity attribute: %w", err)
	}

	base.Identity = id

	if base.Parent, err = getFromAttributeSetOpt[string](attributeSet, parentAttr); err != nil {
		return base, fmt.Errorf("error getting record Parent attribute: %w", err)
	}
	if base.StartTime, err = getFromAttributeSetOpt[uint64](attributeSet, startTimeAttr); err != nil {
		return base, fmt.Errorf("error getting record StartTime attribute: %w", err)
	}
	if base.EndTime, err = getFromAttributeSetOpt[uint64](attributeSet, endTimeAttr); err != nil {
		return base, fmt.Errorf("error getting record EndTime attribute: %w", err)
	}
	return base, nil
}
