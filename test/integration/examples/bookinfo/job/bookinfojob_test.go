//go:build job
// +build job

package job

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skupperproject/skupper/pkg/utils"
	"gotest.tools/assert"
)

func TestBookinfoJob(t *testing.T) {
	var body string

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := utils.RetryWithContext(ctx, time.Millisecond*50, func() (bool, error) {
		resp, err := tryProductPage()
		if err != nil {
			t.Logf("error requesting product page: %s", err)
			return false, nil
		}
		body = string(resp)
		return true, nil
	})
	assert.Assert(t, err)

	fmt.Printf("body:\n%s\n", body)
	assert.Assert(t, strings.Contains(body, "Book Details"))
	assert.Assert(t, strings.Contains(body, "An extremely entertaining play by Shakespeare. The slapstick humour is refreshing!"))
	assert.Assert(t, !strings.Contains(body, "Ratings service is currently unavailable"))
}

func tryProductPage() ([]byte, error) {
	client := http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get("http://productpage:9080/productpage?u=test")
	if err != nil {
		return nil, err
	}

	if resp.Status != "200 OK" {
		return nil, fmt.Errorf("unexpedted http response status: %v", resp.Status)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}
