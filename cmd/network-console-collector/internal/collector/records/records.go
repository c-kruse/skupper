package records

import (
	"time"

	"github.com/skupperproject/skupper/pkg/vanflow"
)

var _ vanflow.Record = AddressRecord{}

type AddressRecord struct {
	ID       string
	Name     string
	Protocol string
	Start    time.Time
}

func (r AddressRecord) Identity() string {
	return r.ID
}

func (r AddressRecord) GetTypeMeta() vanflow.TypeMeta {
	return vanflow.TypeMeta{
		Type:       "AddressRecord",
		APIVersion: "v1alpha1",
	}
}

type ProcessGroupRecord struct {
	ID    string
	Name  string
	Start time.Time
}

func (r ProcessGroupRecord) Identity() string {
	return r.ID
}

func (r ProcessGroupRecord) GetTypeMeta() vanflow.TypeMeta {
	return vanflow.TypeMeta{
		Type:       "ProcessGroupRecord",
		APIVersion: "v1alpha1",
	}
}
