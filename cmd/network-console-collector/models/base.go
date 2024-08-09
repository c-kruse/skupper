package models

type ListResponse[T any] struct {
	Results        []T `json:"results"`
	Count          int `json:"count"`
	TimeRangeCount int `json:"timeRangeCount"`
}

type ItemResponse[T any] struct {
	Results        T   `json:"results"`
	Count          int `json:"count"`
	TimeRangeCount int `json:"timeRangeCount"`
}

type BaseRecord struct {
	ID        string `json:"identity" validate:"required"`
	Parent    string `json:"parent,omitempty"`
	StartTime uint64 `json:"startTime"`
	EndTime   uint64 `json:"endTime"`
	Source    string `json:"source"`
}

type SiteRecord struct {
	BaseRecord
	// Matrix of literal/ptr, default/omitempty, optional/required default/nullable
	L    string  `json:"l"`
	LR   string  `json:"lr" validate:"required"`
	LN   string  `json:"ln" extensions:"x-nullable"`
	LRN  string  `json:"lrn" validate:"required" extensions:"x-nullable"`
	LO   string  `json:"lo,omitempty"`
	LOR  string  `json:"lor,omitempty" validate:"required"`
	LON  string  `json:"lon,omitempty" extensions:"x-nullable"`
	LONR string  `json:"lonr,omitempty" validate:"required" extensions:"x-nullable"`
	P    *string `json:"p"`
	PR   *string `json:"pr" validate:"required"`
	PN   *string `json:"pn" extensions:"x-nullable"`
	PRN  *string `json:"prn" validate:"required" extensions:"x-nullable"`
	PO   *string `json:"po,omitempty"`
	POR  *string `json:"por,omitempty" validate:"required"`
	PON  *string `json:"pon,omitempty" extensions:"x-nullable"`
	PONR *string `json:"ponr,omitempty" validate:"required" extensions:"x-nullable"`
}
