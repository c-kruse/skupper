package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/cenkalti/backoff/v4"
	"github.com/interconnectedcloud/go-amqp"
	"github.com/skupperproject/skupper/pkg/certs"
	"github.com/skupperproject/skupper/pkg/flow/records"
	"github.com/skupperproject/skupper/pkg/qdr"
)

var (
	flags           *flag.FlagSet = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	MessagingConfig string
	AmqpServer      string
	TLSVerify       bool
	TLSCACert       string
	TLSCert         string
	TLSKey          string

	EnableBeacon      bool
	EnableHeartbeat   bool
	EnableRecords     bool
	EnableRecordsFlow bool
	EnableRecordsLogs bool
)

func init() {
	flags.Usage = func() {
		fmt.Printf(`Usage of %s:

Commands:
		log - log messages to stdout
`, os.Args[0])
		flags.PrintDefaults()
	}
	flags.StringVar(&MessagingConfig, "messaging-config", "", "optional path to a skupper connect.json")
	flags.StringVar(&AmqpServer, "server", "amqp://localhost:5671", "AMQP server to connect to")
	flags.BoolVar(&TLSVerify, "tls-verify", true, "validate server CA")
	flags.StringVar(&TLSCACert, "ca", "", "path to AMQP CA certificate")
	flags.StringVar(&TLSCert, "cert", "", "path to certificate when connecting with amqps")
	flags.StringVar(&TLSKey, "key", "", "path to certificate key when connecting with amqps")

	flags.BoolVar(&EnableBeacon, "enable-beacons", false, "")
	flags.BoolVar(&EnableHeartbeat, "enable-heartbeats", false, "")
	flags.BoolVar(&EnableRecords, "enable-records", true, "")
	flags.BoolVar(&EnableRecordsFlow, "enable-flow-records", true, "")
	flags.BoolVar(&EnableRecordsLogs, "enable-log-records", true, "")
	flags.Parse(os.Args[1:])
}
func main() {
	if len(flags.Args()) != 1 {
		fmt.Printf("error: expected command. got %v\n", flags.Args())
		flags.Usage()
		os.Exit(1)
	}

	connURL := AmqpServer
	var tlsCfg qdr.TlsConfigRetriever

	if MessagingConfig != "" {
		b, err := os.ReadFile(MessagingConfig)
		if err != nil {
			fmt.Printf("error: could not read messaging-config %s\n", err)
			flags.Usage()
			os.Exit(1)
		}
		var cfg connectJson
		if err := json.Unmarshal(b, &cfg); err != nil {
			fmt.Printf("error: could not parse messaging-config %s\n", err)
			flags.Usage()
			os.Exit(1)
		}
		tlsCfg = certs.GetTlsConfigRetriever(cfg.Tls.Verify, cfg.Tls.Cert, cfg.Tls.Key, cfg.Tls.Ca)
		connURL = fmt.Sprintf("%s://%s:%s", cfg.Scheme, cfg.Host, cfg.Port)
	}

	if TLSCert != "" {
		tlsCfg = certs.GetTlsConfigRetriever(TLSVerify, TLSCert, TLSKey, TLSCACert)
	}

	factory := qdr.NewConnectionFactory(connURL, tlsCfg)

	var cmdHandler func(context.Context, *qdr.ConnectionFactory)
	switch name := flags.Arg(0); name {
	case "log":
		cmdHandler = logOnly
	default:
		fmt.Printf("error: unexpected command %s\n", name)
		flags.Usage()
		os.Exit(1)

	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	go cmdHandler(ctx, factory)
	<-ctx.Done()
}

func logOnly(ctx context.Context, factory *qdr.ConnectionFactory) {
	beacons := watchBeacons(ctx, factory)

	msgs := make(chan *amqp.Message, 32)
	addresses := make(map[string]struct{})
	enc := json.NewEncoder(os.Stdout)
	for {
		select {
		case b := <-beacons:
			if _, ok := addresses[b.Address]; !ok {
				slog.Debug("received beacon from new event source", "address", b.Address)
				addresses[b.Address] = struct{}{} // mark as watched
				go watchAddr(ctx, factory, b.Address, msgs)
				if EnableRecordsFlow {
					go watchAddr(ctx, factory, b.Address+".flows", msgs)
				}
				if EnableRecordsLogs {
					go watchAddr(ctx, factory, b.Address+".logs", msgs)
				}
			}
			if !EnableBeacon {
				continue
			}
			if err := enc.Encode(Message{
				Subject:    "BEACON",
				V:          fmt.Sprint(b.Version),
				SourceType: b.SourceType,
				Address:    b.Address,
				Direct:     b.Direct,
				ID:         b.Identity,
			}); err != nil {
				slog.Error("error writing beacon message",
					"beacon", b,
					"error", err)
			}
		case msg := <-msgs:
			switch msg.Properties.Subject {
			case "HEARTBEAT":
				if !EnableHeartbeat {
					continue
				}
				heartbeat := records.DecodeHeartbeat(msg)
				if err := enc.Encode(Message{
					Subject: "HEARTBEAT",
					To:      heartbeat.Source,
					V:       fmt.Sprint(msg.ApplicationProperties["v"]),
					Now:     fmt.Sprint(heartbeat.Now),
					ID:      heartbeat.Identity,
				}); err != nil {
					slog.Error("error writing hearbeat message",
						"hearbeat", msg,
						"error", err)
				}
			case "RECORD":
				record, err := records.DecodeRecord(msg)
				if err != nil {
					slog.Error("error decoding record message",
						"msg", msg,
						"error", err)
				}
				if !EnableRecords {
					continue
				}
				records := make([]RecordData, len(record.Records))
				for i := range record.Records {
					records[i] = RecordData{
						Type: fmt.Sprintf("%T", record.Records[i]),
						Data: record.Records[i],
					}
				}
				if err := enc.Encode(Message{Subject: "RECORD", To: record.Address, Data: records}); err != nil {
					slog.Error("error writing record message",
						"record", record,
						"error", err)
				}
			}
		}
	}
}

type Message struct {
	To         string `json:"to"`
	Subject    string `json:"subject"`
	SourceType string `json:"sourceType,omitempty"`
	Address    string `json:"address,omitempty"`
	Direct     string `json:"direct,omitempty"`
	V          string `json:"v,omitempty"`
	Now        string `json:"now,omitempty"`
	ID         string `json:"id,omitempty"`
	Data       any    `json:"data,omitempty"`
}
type RecordData struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

func watchBeacons(ctx context.Context, factory *qdr.ConnectionFactory) <-chan records.BeaconMessage {
	beacons := make(chan records.BeaconMessage, 32)
	go func() {
		defer close(beacons)
		err := backoff.Retry(func() error {
			conn, err := factory.Connect()
			if err != nil {
				slog.Info("could not establish connection", "error", err)
				return fmt.Errorf("fault establishing connection: %w", err)
			}
			recv, err := conn.Receiver("mc/sfe.all", 256)
			if err != nil {
				slog.Info("could not open beacon receiver", err)
				return fmt.Errorf("fault establishing receiver session: %w", err)

			}
			defer recv.Close()
			slog.Debug("starting to listen on mc/sfe.all", "connection", conn)
			defer slog.Debug("stopped listening on mc/sfe.all", "connection", conn)
			for {
				msg, err := recv.Receive()
				if err != nil {
					slog.Info("fault receiving beacon message", "error", err)
					return fmt.Errorf("fault receiving message: %w", err)
				}
				err = recv.Accept(msg)
				if err != nil {
					slog.Info("fault accepting beacon message", "error", err)
					return fmt.Errorf("fault accepting message: %w", err)
				}
				beacons <- records.DecodeBeacon(msg)
			}
		}, backoff.NewExponentialBackOff())
		if err != nil {
			slog.Error("error watching beacons", "error", err)
		}
	}()
	return beacons
}

func watchAddr(ctx context.Context, factory *qdr.ConnectionFactory, address string, msgs chan<- *amqp.Message) {
	err := backoff.Retry(func() error {
		conn, err := factory.Connect()
		if err != nil {
			slog.Info("fault establushing connection", "address", address, "error", err)
			return fmt.Errorf("fault establishing connection: %w", err)
		}
		recv, err := conn.Receiver(address, 256)
		if err != nil {
			slog.Info("fault establushing receiver session", "address", address, "error", err)
			return fmt.Errorf("fault establishing receiver session: %w", err)

		}
		defer recv.Close()
		slog.Debug("starting to listen", "address", address, "connection", conn)
		defer slog.Debug("stopped listening", "address", address, "connection", conn)
		for {
			msg, err := recv.Receive()
			if err != nil {
				slog.Info("fault receiving message", "address", address, "error", err)
				return fmt.Errorf("fault receiving message: %w", err)

			}
			err = recv.Accept(msg)
			if err != nil {
				slog.Info("fault accepting message", "address", address, "error", err)
				return fmt.Errorf("fault accepting message: %w", err)
			}
			msgs <- msg
		}
	}, backoff.NewExponentialBackOff())
	if err != nil {
		slog.Error("failure listening", "address", address, err)
	}
}

type Record struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}
type tlsConfig struct {
	Ca     string `json:"ca,omitempty"`
	Cert   string `json:"cert,omitempty"`
	Key    string `json:"key,omitempty"`
	Verify bool   `json:"verify,omitempty"`
}

type connectJson struct {
	Scheme string    `json:"scheme,omitempty"`
	Host   string    `json:"host,omitempty"`
	Port   string    `json:"port,omitempty"`
	Tls    tlsConfig `json:"tls,omitempty"`
}
