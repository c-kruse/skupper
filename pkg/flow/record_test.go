package flow

import (
	"net/http"
	"testing"

	"gotest.tools/assert"
)

func TestTimeRange(t *testing.T) {
	type test struct {
		name        string
		startTime   uint64
		endTime     uint64
		queryParams QueryParams
		result      bool
	}

	testTable := []test{
		// A[2,3]
		{
			name:      "A-intersects",
			startTime: 2,
			endTime:   3,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: intersects,
			},
			result: false,
		},
		{
			name:      "A-contains",
			startTime: 2,
			endTime:   3,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: contains,
			},
			result: false,
		},
		{
			name:      "A-within",
			startTime: 2,
			endTime:   3,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: within,
			},
			result: false,
		},
		// B[5,7]
		{
			name:      "B-intersects",
			startTime: 5,
			endTime:   7,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: intersects,
			},
			result: true,
		},
		{
			name:      "B-contains",
			startTime: 5,
			endTime:   7,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: contains,
			},
			result: false,
		},
		{
			name:      "B-within",
			startTime: 5,
			endTime:   7,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: within,
			},
			result: false,
		},
		// C[8,11]
		{
			name:      "C-intersects",
			startTime: 8,
			endTime:   11,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: intersects,
			},
			result: true,
		},
		{
			name:      "C-contains",
			startTime: 8,
			endTime:   11,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: contains,
			},
			result: false,
		},
		{
			name:      "C-within",
			startTime: 8,
			endTime:   11,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: within,
			},
			result: true,
		},
		// D[12,14]
		{
			name:      "D-intersects",
			startTime: 12,
			endTime:   14,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: intersects,
			},
			result: true,
		},
		{
			name:      "D-contains",
			startTime: 12,
			endTime:   14,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: contains,
			},
			result: false,
		},
		{
			name:      "D-within",
			startTime: 12,
			endTime:   14,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: within,
			},
			result: false,
		},
		// E[12,14]
		{
			name:      "E-intersects",
			startTime: 16,
			endTime:   18,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: intersects,
			},
			result: false,
		},
		{
			name:      "E-contains",
			startTime: 16,
			endTime:   18,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: contains,
			},
			result: false,
		},
		{
			name:      "E-within",
			startTime: 16,
			endTime:   18,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: within,
			},
			result: false,
		},
		// F[4,16]
		{
			name:      "F-intersects",
			startTime: 4,
			endTime:   16,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: intersects,
			},
			result: true,
		},
		{
			name:      "F-contains",
			startTime: 4,
			endTime:   16,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: contains,
			},
			result: true,
		},
		{
			name:      "F-within",
			startTime: 4,
			endTime:   16,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: within,
			},
			result: false,
		},
		// G[9,0]
		{
			name:      "G-intersects",
			startTime: 9,
			endTime:   0,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: intersects,
			},
			result: true,
		},
		{
			name:      "G-contains",
			startTime: 9,
			endTime:   0,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: contains,
			},
			result: false,
		},
		{
			name:      "G-within",
			startTime: 9,
			endTime:   0,
			queryParams: QueryParams{
				TimeRangeStart:     6,
				TimeRangeEnd:       13,
				TimeRangeOperation: within,
			},
			result: false,
		},
	}

	for _, test := range testTable {
		base := Base{
			StartTime: test.startTime,
			EndTime:   test.endTime,
		}
		result := base.TimeRangeValid(test.queryParams)
		assert.Equal(t, test.result, result, test.name)
	}
}

func TestRecordState(t *testing.T) {
	type test struct {
		name        string
		startTime   uint64
		endTime     uint64
		queryParams QueryParams
		result      bool
	}

	testTable := []test{
		// A[2,3]
		{
			name:      "A-all",
			startTime: 2,
			endTime:   15,
			queryParams: QueryParams{
				TimeRangeStart:     1,
				TimeRangeEnd:       20,
				TimeRangeOperation: intersects,
				State:              all,
			},
			result: true,
		},
		{
			name:      "A-active",
			startTime: 2,
			endTime:   0,
			queryParams: QueryParams{
				TimeRangeStart:     1,
				TimeRangeEnd:       20,
				TimeRangeOperation: intersects,
				State:              active,
			},
			result: true,
		},
		{
			name:      "A-not-active",
			startTime: 2,
			endTime:   15,
			queryParams: QueryParams{
				TimeRangeStart:     1,
				TimeRangeEnd:       20,
				TimeRangeOperation: intersects,
				State:              active,
			},
			result: false,
		},
		{
			name:      "A-terminated",
			startTime: 2,
			endTime:   3,
			queryParams: QueryParams{
				TimeRangeStart:     1,
				TimeRangeEnd:       20,
				TimeRangeOperation: intersects,
				State:              terminated,
			},
			result: true,
		},
		{
			name:      "A-not-terminated",
			startTime: 2,
			endTime:   0,
			queryParams: QueryParams{
				TimeRangeStart:     1,
				TimeRangeEnd:       20,
				TimeRangeOperation: intersects,
				State:              terminated,
			},
			result: false,
		},
	}

	for _, test := range testTable {
		base := Base{
			StartTime: test.startTime,
			EndTime:   test.endTime,
		}
		result := base.TimeRangeValid(test.queryParams)
		assert.Equal(t, test.result, result, test.name)
	}
}

func TestParameters(t *testing.T) {
	type test struct {
		url         string
		queryParams QueryParams
	}

	testTable := []test{
		{
			url: "http://host?timeRangeStart=1234&timeRangeEnd=0",
			queryParams: QueryParams{
				Offset:             -1,
				Limit:              -1,
				SortBy:             "identity.asc",
				Filter:             "",
				TimeRangeStart:     uint64(1234),
				TimeRangeEnd:       uint64(0),
				TimeRangeOperation: intersects,
			},
		},
		{
			url: "http://host?timeRangeStart=0&timeRangeEnd=0&timeRangeOperation=contains",
			queryParams: QueryParams{
				Offset:             -1,
				Limit:              -1,
				SortBy:             "identity.asc",
				Filter:             "",
				TimeRangeStart:     uint64(0),
				TimeRangeEnd:       uint64(0),
				TimeRangeOperation: contains,
			},
		},
		{
			url: "http://host?timeRangeStart=1234&timeRangeEnd=0&timeRangeOperation=within",
			queryParams: QueryParams{
				Offset:             -1,
				Limit:              -1,
				SortBy:             "identity.asc",
				Filter:             "",
				TimeRangeStart:     uint64(1234),
				TimeRangeEnd:       uint64(0),
				TimeRangeOperation: within,
			},
		},
		{
			url: "http://host?timeRangeStart=0&timeRangeEnd=4567&timeRangeOperation=intersects&offset=10&limit=10&sortBy=sourcePort.desc&filter=forwardFlow.protocol.tcp",
			queryParams: QueryParams{
				Offset:             10,
				Limit:              10,
				SortBy:             "sourcePort.desc",
				Filter:             "forwardFlow.protocol.tcp",
				TimeRangeStart:     uint64(0),
				TimeRangeEnd:       uint64(4567),
				TimeRangeOperation: intersects,
			},
		},
		{
			url: "http://host?processRole=external&processRole=internal&timeRangeStart=0&timeRangeEnd=0&limit=0&offset=0",
			queryParams: QueryParams{
				SortBy: "identity.asc",
				FilterFields: map[string][]string{
					"processRole": {"external", "internal"},
				},
			},
		},
	}

	for _, test := range testTable {
		t.Run(test.url, func(t *testing.T) {
			req, _ := http.NewRequest("GET", test.url, nil)
			q := req.URL.Query()
			req.URL.RawQuery = q.Encode()
			qp := getQueryParams(req.URL)
			assert.Equal(t, qp.Offset, test.queryParams.Offset)
			assert.Equal(t, qp.Limit, test.queryParams.Limit)
			assert.Equal(t, qp.SortBy, test.queryParams.SortBy)
			assert.Equal(t, qp.TimeRangeStart, test.queryParams.TimeRangeStart)
			assert.Equal(t, qp.TimeRangeEnd, test.queryParams.TimeRangeEnd)
			assert.Equal(t, qp.TimeRangeOperation, test.queryParams.TimeRangeOperation)
		})
	}
}

func TestPagination(t *testing.T) {
	type test struct {
		offset int
		limit  int
		length int
		start  int
		end    int
	}

	testTable := []test{
		{
			offset: -1,
			limit:  -1,
			length: 100,
			start:  0,
			end:    100,
		},
		{
			offset: 0,
			limit:  10,
			length: 100,
			start:  0,
			end:    10,
		},
		{
			offset: 90,
			limit:  20,
			length: 100,
			start:  90,
			end:    100,
		},
		{
			offset: 110,
			limit:  20,
			length: 100,
			start:  100,
			end:    100,
		},
	}

	for _, test := range testTable {
		start, end := paginate(test.offset, test.limit, test.length)
		assert.Equal(t, test.start, start)
		assert.Equal(t, test.end, end)
	}
}

func TestMatchField(t *testing.T) {
	field1 := "foo"
	field1Value := []string{"foo"}
	field2 := uint64(12345678)
	field2Value := []string{"12345678"}
	field3 := int32(87654321)
	field3Value := []string{"87654321"}
	field4 := int64(12345678)
	field4Value := []string{"12345678"}
	field5 := int(12345678)
	field5Value := []string{"12345678"}

	match := matchFieldValues(field1, field1Value)
	assert.Equal(t, match, true)
	match = matchFieldValues(field2, field2Value)
	assert.Equal(t, match, true)
	match = matchFieldValues(field3, field3Value)
	assert.Equal(t, match, true)
	match = matchFieldValues(field4, field4Value)
	assert.Equal(t, match, true)
	match = matchFieldValues(field5, field5Value)
	assert.Equal(t, match, true)
	match = matchFieldValues(field5, field1Value)
	assert.Equal(t, match, false)
}

func TestCompareFields(t *testing.T) {
	type test struct {
		field1 interface{}
		field2 interface{}
		order  string
		result bool
	}

	testTable := []test{
		{
			field1: "foo",
			field2: "bar",
			order:  "asc",
			result: false,
		},
		{
			field1: "foo",
			field2: "bar",
			order:  "desc",
			result: true,
		},
		{
			field1: uint64(12345678),
			field2: uint64(87654321),
			order:  "asc",
			result: true,
		},
		{
			field1: uint64(12345678),
			field2: uint64(87654321),
			order:  "desc",
			result: false,
		},
		{
			field1: int32(12345678),
			field2: int32(87654321),
			order:  "asc",
			result: true,
		},
		{
			field1: int32(12345678),
			field2: int32(87654321),
			order:  "desc",
			result: false,
		},
		{
			field1: int64(12345678),
			field2: int64(87654321),
			order:  "asc",
			result: true,
		},
		{
			field1: int64(12345678),
			field2: int64(87654321),
			order:  "desc",
			result: false,
		},
		{
			field1: int(12345678),
			field2: int(87654321),
			order:  "asc",
			result: true,
		},
		{
			field1: int(12345678),
			field2: int(87654321),
			order:  "desc",
			result: false,
		},
	}

	for _, test := range testTable {
		result := compareFields(test.field1, test.field2, test.order)
		assert.Equal(t, test.result, result)
	}
}
