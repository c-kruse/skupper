package v1

import (
	"time"
)

type BaseRecord struct {
	Identity  string     `vflow:"1,identity"`
	StartTime *time.Time `vflow:"3"`
	EndTime   *time.Time `vflow:"4"`
}
