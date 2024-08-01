package api

type ResponseSetters interface {
	SetStatus(s string)
	SetCount(c int64)
	SetTotalCount(c *int64)
	SetTimeRangeCount(c *int64)
}

type SetResponseOptional[T any] interface {
	Set(*T)
	ResponseSetters
}
type SetResponse[T any] interface {
	Set(T)
	ResponseSetters
}

func (b *BaseResponse) SetStatus(s string) {
	b.Status = s
}
func (b *BaseResponse) SetCount(c int64) {
	b.Count = c
}
func (b *BaseResponse) SetTotalCount(c *int64) {
	b.TotalCount = c
}
func (b *BaseResponse) SetTimeRangeCount(c *int64) {
	b.TimeRangeCount = c
}
