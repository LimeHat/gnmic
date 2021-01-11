package stan_output

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/karimra/gnmic/formatters"
	"github.com/karimra/gnmic/outputs"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/stan.go"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	stanDefaultTimeout      = 10
	stanDefaultPingInterval = 5
	stanDefaultPingRetry    = 2

	defaultSubjectName = "gnmic-telemetry"

	defaultFormat           = "json"
	defaultRecoveryWaitTime = 10 * time.Second
	defaultNumWorkers       = 1
	defaultWriteTimeout     = 10 * time.Second
)

func init() {
	outputs.Register("stan", func() outputs.Output {
		return &StanOutput{
			Cfg: &Config{},
			wg:  new(sync.WaitGroup),
		}
	})
}

type protoMsg struct {
	m    proto.Message
	meta outputs.Meta
}

// StanOutput //
type StanOutput struct {
	Cfg      *Config
	cancelFn context.CancelFunc
	logger   *log.Logger
	msgChan  chan *protoMsg
	wg       *sync.WaitGroup
	mo       *formatters.MarshalOptions
	evps     []formatters.EventProcessor
}

// Config //
type Config struct {
	Name             string        `mapstructure:"name,omitempty"`
	Address          string        `mapstructure:"address,omitempty"`
	SubjectPrefix    string        `mapstructure:"subject-prefix,omitempty"`
	Subject          string        `mapstructure:"subject,omitempty"`
	Username         string        `mapstructure:"username,omitempty"`
	Password         string        `mapstructure:"password,omitempty"`
	ClusterName      string        `mapstructure:"cluster-name,omitempty"`
	PingInterval     int           `mapstructure:"ping-interval,omitempty"`
	PingRetry        int           `mapstructure:"ping-retry,omitempty"`
	Format           string        `mapstructure:"format,omitempty"`
	RecoveryWaitTime time.Duration `mapstructure:"recovery-wait-time,omitempty"`
	NumWorkers       int           `mapstructure:"num-workers,omitempty"`
	Debug            bool          `mapstructure:"debug,omitempty"`
	WriteTimeout     time.Duration `mapstructure:"write-timeout,omitempty"`
	EventProcessors  []string      `mapstructure:"event-processors,omitempty"`
}

func (s *StanOutput) String() string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func (s *StanOutput) SetLogger(logger *log.Logger) {
	if logger != nil {
		s.logger = log.New(logger.Writer(), "stan_output ", logger.Flags())
		return
	}
	s.logger = log.New(os.Stderr, "stan_output ", log.LstdFlags|log.Lmicroseconds)
}

func (s *StanOutput) SetEventProcessors(ps map[string]map[string]interface{}, log *log.Logger) {
	for _, epName := range s.Cfg.EventProcessors {
		if epCfg, ok := ps[epName]; ok {
			epType := ""
			for k := range epCfg {
				epType = k
				break
			}
			if in, ok := formatters.EventProcessors[epType]; ok {
				ep := in()
				err := ep.Init(epCfg[epType], log)
				if err != nil {
					s.logger.Printf("failed initializing event processor '%s' of type='%s': %v", epName, epType, err)
					continue
				}
				s.evps = append(s.evps, ep)
				s.logger.Printf("added event processor '%s' of type=%s to stan output", epName, epType)
			}
		}
	}
}

// Init //
func (s *StanOutput) Init(ctx context.Context, cfg map[string]interface{}, opts ...outputs.Option) error {
	err := outputs.DecodeConfig(cfg, s.Cfg)
	if err != nil {
		return err
	}
	err = s.setDefaults()
	if err != nil {
		return err
	}
	for _, opt := range opts {
		opt(s)
	}
	s.msgChan = make(chan *protoMsg)
	initMetrics()
	s.mo = &formatters.MarshalOptions{Format: s.Cfg.Format}
	ctx, s.cancelFn = context.WithCancel(ctx)
	s.wg.Add(s.Cfg.NumWorkers)
	for i := 0; i < s.Cfg.NumWorkers; i++ {
		cfg := *s.Cfg
		cfg.Name = fmt.Sprintf("%s-%d", cfg.Name, i)
		go s.worker(ctx, i, &cfg)
	}

	s.logger.Printf("initialized stan producer: %s", s.String())
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return nil
}

func (s *StanOutput) setDefaults() error {
	if s.Cfg.Name == "" {
		s.Cfg.Name = "gnmic-" + uuid.New().String()
	}
	if s.Cfg.ClusterName == "" {
		return fmt.Errorf("clusterName is mandatory")
	}
	if s.Cfg.Subject == "" && s.Cfg.SubjectPrefix == "" {
		s.Cfg.Subject = defaultSubjectName
	}
	if s.Cfg.RecoveryWaitTime == 0 {
		s.Cfg.RecoveryWaitTime = defaultRecoveryWaitTime
	}
	if s.Cfg.WriteTimeout <= 0 {
		s.Cfg.WriteTimeout = defaultWriteTimeout
	}
	if s.Cfg.NumWorkers <= 0 {
		s.Cfg.NumWorkers = defaultNumWorkers
	}
	if s.Cfg.Format == "" {
		s.Cfg.Format = defaultFormat
	}
	if !(s.Cfg.Format == "event" || s.Cfg.Format == "protojson" || s.Cfg.Format == "proto" || s.Cfg.Format == "json") {
		return fmt.Errorf("unsupported output format: '%s' for output type STAN", s.Cfg.Format)
	}
	if s.Cfg.PingInterval == 0 {
		s.Cfg.PingInterval = stanDefaultPingInterval
	}
	if s.Cfg.PingRetry == 0 {
		s.Cfg.PingRetry = stanDefaultPingRetry
	}
	return nil
}

// Write //
func (s *StanOutput) Write(ctx context.Context, rsp protoreflect.ProtoMessage, meta outputs.Meta) {
	if rsp == nil || s.mo == nil {
		return
	}

	select {
	case <-ctx.Done():
		return
	case s.msgChan <- &protoMsg{m: rsp, meta: meta}:
	case <-time.After(s.Cfg.WriteTimeout):
		if s.Cfg.Debug {
			s.logger.Printf("writing expired after %s, NATS output might not be initialized", s.Cfg.WriteTimeout)
		}
		StanNumberOfFailSendMsgs.WithLabelValues(s.Cfg.Name, "timeout").Inc()
		return
	}
}

// Metrics //
func (s *StanOutput) Metrics() []prometheus.Collector {
	return []prometheus.Collector{
		StanNumberOfSentMsgs,
		StanNumberOfSentBytes,
		StanNumberOfFailSendMsgs,
		StanSendDuration,
	}
}

// Close //
func (s *StanOutput) Close() error {
	s.cancelFn()
	s.wg.Wait()
	return nil
}

func (s *StanOutput) createSTANConn(c *Config) stan.Conn {
	opts := []nats.Option{
		nats.Name(c.Name),
	}
	if c.Username != "" && c.Password != "" {
		opts = append(opts, nats.UserInfo(c.Username, c.Password))
	}

	var nc *nats.Conn
	var sc stan.Conn
	var err error
CRCONN:
	s.logger.Printf("attempting to connect to %s", c.Address)
	nc, err = nats.Connect(c.Address, opts...)
	if err != nil {
		s.logger.Printf("failed to create connection: %v", err)
		time.Sleep(s.Cfg.RecoveryWaitTime)
		goto CRCONN
	}
	sc, err = stan.Connect(c.ClusterName, c.Name,
		stan.NatsConn(nc),
		stan.Pings(c.PingInterval, c.PingRetry),
		stan.SetConnectionLostHandler(func(_ stan.Conn, err error) {
			s.logger.Printf("STAN connection lost, reason: %v", err)
			s.logger.Printf("retryring...")
			//sc = s.createSTANConn(c)
		}),
	)
	if err != nil {
		s.logger.Printf("failed to create connection: %v", err)
		nc.Close()
		time.Sleep(s.Cfg.RecoveryWaitTime)
		goto CRCONN
	}
	s.logger.Printf("successfully connected to STAN server %s", c.Address)
	return sc
}

func (s *StanOutput) worker(ctx context.Context, i int, c *Config) {
	defer s.wg.Done()
	var stanConn stan.Conn
	workerLogPrefix := fmt.Sprintf("worker-%d", i)
	s.logger.Printf("%s starting", workerLogPrefix)
CRCONN:
	stanConn = s.createSTANConn(c)
	s.logger.Printf("%s initialized stan producer: %s", workerLogPrefix, s.String())
	defer stanConn.Close()
	defer stanConn.NatsConn().Close()
	for {
		select {
		case <-ctx.Done():
			s.logger.Printf("%s shutting down", workerLogPrefix)
			return
		case m := <-s.msgChan:
			b, err := s.mo.Marshal(m.m, m.meta, s.evps...)
			if err != nil {
				if s.Cfg.Debug {
					s.logger.Printf("%s failed marshaling proto msg: %v", workerLogPrefix, err)
				}
				StanNumberOfFailSendMsgs.WithLabelValues(c.Name, "marshal_error").Inc()
				continue
			}
			subject := s.subjectName(c, m.meta)
			start := time.Now()
			err = stanConn.Publish(subject, b)
			if err != nil {
				if s.Cfg.Debug {
					s.logger.Printf("%s failed to write to STAN subject %q: %v", workerLogPrefix, subject, err)
				}
				StanNumberOfFailSendMsgs.WithLabelValues(c.Name, "publish_error").Inc()
				stanConn.Close()
				stanConn.NatsConn().Close()
				time.Sleep(c.RecoveryWaitTime)
				goto CRCONN
			}
			StanSendDuration.WithLabelValues(c.Name).Set(float64(time.Since(start).Nanoseconds()))
			StanNumberOfSentMsgs.WithLabelValues(c.Name, subject).Inc()
			StanNumberOfSentBytes.WithLabelValues(c.Name, subject).Add(float64(len(b)))
		}
	}
}

func (s *StanOutput) subjectName(c *Config, meta outputs.Meta) string {
	if c.SubjectPrefix != "" {
		ssb := strings.Builder{}
		ssb.WriteString(s.Cfg.SubjectPrefix)
		if s, ok := meta["source"]; ok {
			source := strings.ReplaceAll(s, ".", "-")
			source = strings.ReplaceAll(source, " ", "_")
			ssb.WriteString(".")
			ssb.WriteString(source)
		}
		if subname, ok := meta["subscription-name"]; ok {
			ssb.WriteString(".")
			ssb.WriteString(subname)
		}
		return strings.ReplaceAll(ssb.String(), " ", "_")
	}
	return strings.ReplaceAll(s.Cfg.Subject, " ", "_")
}
