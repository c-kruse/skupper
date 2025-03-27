package site

import (
	"testing"

	"gotest.tools/assert"
)

func TestJoinSplitEscaping(t *testing.T) {
	testcases := [][]string{
		{"a,b,c"},
		{"multivalue.annotation.testing=a,b,c"},
		{"complexvalue.annotation.testing=a=11,b=22"},
		{"edge cases", "\\", ",", "\\,\\"},
		{
			"kanji.utf8.testing=計",
			"dogeomjis.utf8.testing=🐕🐶+\\🦮,(🌭)",
		},
	}
	for _, tc := range testcases {
		t.Run("", func(t *testing.T) {
			tmp := joinWithEscaping(tc, ',', '\\')
			actual := splitWithEscaping(tmp, ',', '\\')
			assert.DeepEqual(t, tc, actual)
		})
	}
}

func TestAsMap(t *testing.T) {
	out := asMap([]string{"x.y.z==b=c,d=e"}) // only splits on first =
	assert.Equal(t, out["x.y.z"], "=b=c,d=e")
}
