package main

import "time"

type Config struct {
	APIListenAddress     string
	APIDisableAccessLogs bool
	TLSCert              string
	TLSKey               string

	MetricsListenAddress string

	EnableConsole   bool
	ConsoleLocation string
	PrometheusAPI   string

	AuthMode     string
	BasicAuthDir string

	FlowConnectionFile string
	FlowRecordTTL      time.Duration

	EnableProfile bool
	CORSAllowAll  bool
}

type TLSSpec struct {
	CA     string `json:"ca,omitempty"`
	Cert   string `json:"cert,omitempty"`
	Key    string `json:"key,omitempty"`
	Verify bool   `json:"verify,omitempty"`
}

type ConnectionSpec struct {
	Scheme string  `json:"scheme,omitempty"`
	Host   string  `json:"host,omitempty"`
	Port   string  `json:"port,omitempty"`
	TLS    TLSSpec `json:"tls,omitempty"`
}
