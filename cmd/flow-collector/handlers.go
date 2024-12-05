package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/skupperproject/skupper/pkg/flow"
)

func (c *Controller) eventsourceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.EventSource, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) siteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Site, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) hostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Host, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) routerHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Router, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) linkHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Link, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) listenerHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Listener, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) connectorHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Connector, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) addressHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Address, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) processHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Process, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) processGroupHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.ProcessGroup, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) flowHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Flow, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) flowPairHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.FlowPair, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) sitePairHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.SitePair, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) processGroupPairHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.ProcessGroupPair, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) processPairHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.ProcessPair, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) collectorHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	c.FlowCollector.Request <- flow.ApiRequest{RecordType: flow.Collector, Request: r}
	response := <-c.FlowCollector.Response
	w.WriteHeader(response.Status)
	if response.Body != nil {
		fmt.Fprintf(w, "%s", *response.Body)
	}
}

func (c *Controller) promqueryHandler(w http.ResponseWriter, r *http.Request) {
	client := http.Client{}

	urlOut := c.FlowCollector.Collector.PrometheusUrl + "query?" + r.URL.RawQuery
	proxyReq, err := http.NewRequest(r.Method, urlOut, nil)
	if err != nil {
		log.Printf("COLLECTOR: prom proxy request error: %s\n", err.Error())

		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Internal Server Error: %s\n", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		log.Printf("COLLECTOR: Prometheus query error: %s\n", err.Error())

		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Internal Server Error: %s\n", err.Error())
		return
	}
	w.WriteHeader(proxyResp.StatusCode)
	defer proxyResp.Body.Close()
	if _, err := io.Copy(w, proxyResp.Body); err != nil {
		log.Printf("COLLECTOR: query proxy response write error: %s", err.Error())
	}
}

func (c *Controller) promqueryrangeHandler(w http.ResponseWriter, r *http.Request) {
	client := http.Client{}

	urlOut := c.FlowCollector.Collector.PrometheusUrl + "query_range?" + r.URL.RawQuery
	proxyReq, err := http.NewRequest(r.Method, urlOut, nil)
	if err != nil {
		log.Printf("COLLECTOR: prom proxy request error: %s \n", err.Error())

		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Internal Server Error: %s\n", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		log.Printf("COLLECTOR: Prometheus query_range error: %s\n", err.Error())

		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Internal Server Error: %s\n", err.Error())
		return
	}
	defer proxyResp.Body.Close()
	w.WriteHeader(proxyResp.StatusCode)
	if _, err := io.Copy(w, proxyResp.Body); err != nil {
		log.Printf("COLLECTOR: rangequery proxy response write error: %s", err.Error())
	}
}

func noAuth(h http.HandlerFunc) http.HandlerFunc {
	return h
}

// basicAuthHandler handles basic auth for multiple users.
// map[username]sha256(username + ":" + password)
// Attempts to validate requests in constant time.
type basicAuthHandler map[string][32]byte

// newBasicAuthHandler initializes a basicAuthHandler from the supplied
// directory.
func newBasicAuthHandler(root string) (basicAuthHandler, error) {
	users := make(basicAuthHandler)

	entries, err := os.ReadDir(root)
	if err != nil {
		return users, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		if strings.HasPrefix(filename, ".") { // skip hidden files
			continue
		}
		path := filepath.Join(root, filename)
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("could not open %s: %s", path, err)
		}
		defer f.Close()

		checksum := sha256.New()
		checksum.Write([]byte(filename + ":"))
		written, err := io.Copy(checksum, io.LimitReader(f, 1024))
		if err != nil {
			return nil, fmt.Errorf("could not read contents %s: %s", path, err)
		}
		if written < 8 {
			log.Printf("COLLECTOR: Ignoring auth user %q. Contents too short", path)
			continue
		}

		var sum [32]byte
		copy(sum[:], checksum.Sum(nil))
		users[filename] = sum
	}
	return users, nil
}

func (h basicAuthHandler) HandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()

		if ok && h.check(user, password) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Basic realm=skupper")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

func (h basicAuthHandler) check(user, password string) bool {
	given := sha256.Sum256([]byte(user + ":" + password))
	for _, required := range h {
		match := subtle.ConstantTimeCompare(required[:], given[:])
		if match == 1 {
			return true
		}
	}
	return false
}
