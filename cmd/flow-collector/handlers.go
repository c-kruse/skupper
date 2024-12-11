package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/skupperproject/skupper/pkg/flow"
	"github.com/skupperproject/skupper/pkg/fs"
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

var authUserFileExpr = regexp.MustCompile(`^[a-zA-Z0-9].*$`)

// basicAuthHandler handles basic auth for multiple users.
type basicAuthHandler struct {
	root  string
	mu    sync.RWMutex
	users map[string]string
}

// newBasicAuthHandler initializes a basicAuthHandler from the supplied
// directory.
func newBasicAuthHandler(root string, stop <-chan struct{}) (*basicAuthHandler, error) {
	basicUsers := &basicAuthHandler{root: root}
	if err := basicUsers.reload(); err != nil {
		return basicUsers, err
	}

	watcher, err := fs.NewWatcher()
	if err != nil {
		return basicUsers, err
	}
	watcher.Start(stop)

	// Restrict usernames to files begining with an alphanumeric character
	// Omits hidden files
	watcher.Add(root, basicUsers, authUserFileExpr)
	return basicUsers, nil
}

func (h *basicAuthHandler) HandlerFunc(next http.HandlerFunc) http.HandlerFunc {
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

func (h *basicAuthHandler) OnCreate(f string) {
	if err := h.reload(); err != nil {
		log.Printf("COLLECTOR: auth handler reload error: %s\n", err.Error())
	}
}
func (h *basicAuthHandler) OnUpdate(f string) {
	h.OnCreate(f)
}
func (h *basicAuthHandler) OnRemove(f string) {
	h.OnCreate(f)
}

func (h *basicAuthHandler) reload() error {
	users := make(map[string]string)

	entries, err := os.ReadDir(h.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		username := entry.Name()

		if !authUserFileExpr.MatchString(username) {
			continue
		}
		path := filepath.Join(h.root, username)
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("could not open %s: %s", path, err)
		}
		defer f.Close()

		passwdBytes, err := io.ReadAll(io.LimitReader(f, 1024))
		if err != nil {
			return fmt.Errorf("could not read contents %s: %s", path, err)
		}

		users[username] = string(passwdBytes)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.users = users
	return nil
}

func (h *basicAuthHandler) check(user, given string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for username, required := range h.users {
		if username == user && given == required {
			return true
		}
	}
	return false
}
