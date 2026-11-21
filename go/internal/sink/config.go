// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package sink is ferun-sink, the platform's log store (ADR 0026,
// docs/architecture/sink.md): the nodes' OTLP log ingest over mTLS, the
// hot stage in its PostgreSQL, the cold stage in Quickwit's indexes (§10),
// the read API for tenants, and retention; and its spans
// (docs/architecture/traces.md), read in the shape of Jaeger's API.
//
// Every flag has an environment variable FERUN_SINK_<NAME> (the flag's
// name in upper snake case); a flag given wins over its variable. The
// secrets come from a variable, SINK_SERVICE_TOKEN, the sink's service
// token at ferund, and from a file, the password of Quickwit's Kafka
// sources (--quickwit-kafka-password-file); neither is in a flag.
package sink

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// EnvPrefix starts every environment variable of a flag.
const EnvPrefix = "FERUN_SINK_"

// The variables of the secrets.
const (
	EnvServiceToken = "SINK_SERVICE_TOKEN"
)

// How the sink signs in to Kafka (--kafka-sasl).
const (
	// KafkaSASLNone is plaintext without SASL: a listener inside the
	// cluster that a NetworkPolicy guards (Strimzi's plain).
	KafkaSASLNone = "none"
	// KafkaSASLAWSMSKIAM is TLS and SASL AWS_MSK_IAM, Amazon MSK's IAM
	// access control (port 9098), with the AWS credentials the platform
	// hands the process: an EKS Pod Identity's, or static keys
	// (awsauth.Source).
	KafkaSASLAWSMSKIAM = "aws-msk-iam"
)

// The log formats (--log-format).
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Config is the configuration of `ferun-sink serve`: its flags, its
// secrets, and the tunables tests change, which are not flags.
type Config struct {
	// Name is the instance's name, a DNS label ($(POD_NAME)); its
	// certificate's URI is spiffe://<TrustDomain>/sink/<Name>.
	Name string
	// Listen is the HTTP API's address, IngestListen the ingest's (gRPC
	// over TLS).
	Listen, IngestListen string
	// DatabaseURL is the sink's own database.
	DatabaseURL string
	TrustDomain string
	// FerunURL is ferund's API inside the cluster: GET /api/v1/tenant and
	// POST /api/v1/service-certificates.
	FerunURL string
	// ServiceToken is the sink's service token at ferund.
	ServiceToken string
	// The index (ADR 0026 §10): Quickwit's indexers for ingest, its
	// searchers for everything else, and the prefix of the log indexes'
	// names (<prefix>-v<version>-<days>d).
	IndexSearchURL, IndexPrefix string
	// The log of record (ADR 0026 §11): Kafka's brokers, and a new
	// topic's partitions, replicas and retention.
	KafkaBrokers                      []string
	KafkaPartitions, KafkaReplication int
	KafkaRetention                    time.Duration
	// KafkaSASL is how the sink signs in to the brokers, KafkaSASLNone or
	// KafkaSASLAWSMSKIAM. It is the sink's own: Quickwit's Kafka sources
	// get the brokers alone, and Quickwit 0.9's source (librdkafka) has no
	// way to sign in with IAM, so with aws-msk-iam they cannot read.
	KafkaSASL string
	// Quickwit's Kafka sources reach the brokers on their own, as the sink
	// makes them: at QuickwitKafkaBrokers (none: KafkaBrokers), and, with
	// a QuickwitKafkaUsername, over TLS with SASL SCRAM-SHA-512 as that
	// user, since Quickwit 0.9 cannot sign in with IAM (an Amazon MSK
	// listener of its own, 9096). The password is read from
	// QuickwitKafkaPasswordFile at the start; it goes to Quickwit with
	// each source the sink makes, so Quickwit's metastore holds it.
	QuickwitKafkaBrokers      []string
	QuickwitKafkaUsername     string
	QuickwitKafkaPasswordFile string
	QuickwitKafkaPassword     string
	// IndexPartitions is how many partitions a new index hashes its lines
	// into by tenant, environment and job (its partition_key); an index
	// keeps the number it was made with.
	IndexPartitions int
	// Retention is how long lines are kept, unless a stream asks for
	// longer (ferun.retention_days); RetentionMax bounds what it asks.
	Retention, RetentionMax time.Duration
	// Quota is the bytes of text a tenant may store a day; 0: no quota.
	Quota     int64
	LogLevel  slog.Level
	LogFormat string

	// Traces (docs/architecture/traces.md): the bytes of span documents
	// a tenant may store a day (0: no quota), how long spans are kept,
	// and the prefix of the trace index's name (<prefix>-v<N>-<days>d).
	TraceQuota       int64
	TraceRetention   time.Duration
	TraceIndexPrefix string

	// CommitTimeout is the indexes' commit interval: a line is searchable
	// at most this long after it is stored (Quickwit's
	// commit_timeout_secs, set when an index is made).
	CommitTimeout time.Duration
	// Tunables, not flags:
	// the sweep runs every SweepInterval; a follow sends {} after Heartbeat
	// without a line; a read scans at most ScanLimit bytes of lines.
	SweepInterval time.Duration
	Heartbeat     time.Duration
	ScanLimit     int64
}

// Default returns the defaults of serve, without the required settings.
func Default() Config {
	return Config{
		Listen:        ":8080",
		IngestListen:  ":4317",
		Retention:     720 * time.Hour,
		RetentionMax:  9600 * time.Hour,
		LogLevel:      slog.LevelInfo,
		LogFormat:     LogFormatJSON,
		IndexPrefix:   "ferun-logs",
		CommitTimeout: 10 * time.Second,
		SweepInterval: 10 * time.Minute,
		Heartbeat:     15 * time.Second,
		ScanLimit:     256 << 20,

		IndexPartitions:  16,
		KafkaPartitions:  64,
		KafkaReplication: 3,
		KafkaRetention:   24 * time.Hour,
		KafkaSASL:        KafkaSASLNone,
		TraceQuota:       1 << 30,
		TraceRetention:   720 * time.Hour,
		TraceIndexPrefix: "ferun-traces",
	}
}

// EnvName is the environment variable of a flag.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse reads the flags of serve from args, the variables of the flags not
// given and of the secrets through getenv, over Default, and validates the
// result. --help yields flag.ErrHelp.
func Parse(args []string, getenv func(string) string) (Config, error) {
	c := Default()
	fs := newFlagSet("serve")
	level := register(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return Config{}, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		return Config{}, fmt.Errorf("--log-level: %w", err)
	}
	if getenv != nil {
		c.ServiceToken = getenv(EnvServiceToken)
	}
	if c.QuickwitKafkaPasswordFile != "" {
		b, err := os.ReadFile(c.QuickwitKafkaPasswordFile)
		if err != nil {
			return Config{}, fmt.Errorf("--quickwit-kafka-password-file: %w", err)
		}
		c.QuickwitKafkaPassword = strings.TrimRight(string(b), "\r\n")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func register(fs *flag.FlagSet, c *Config) *string {
	fs.StringVar(&c.Name, "name", c.Name, "the instance's name, a DNS label: $(POD_NAME) on Kubernetes (required)")
	fs.StringVar(&c.Listen, "listen", c.Listen, "the HTTP API's address")
	fs.StringVar(&c.IngestListen, "ingest-listen", c.IngestListen, "the ingest's address (OTLP and SinkService over gRPC and TLS, nodes only)")
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "the sink's PostgreSQL database (required); PGPASSWORD may hold the password")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "the SPIFFE trust domain (required)")
	fs.StringVar(&c.FerunURL, "ferun-url", c.FerunURL, "ferund's API inside the cluster, e.g. http://ferund.ferun.svc:8100 (required)")
	fs.StringVar(&c.IndexSearchURL, "index-search-url", c.IndexSearchURL, "Quickwit's REST API at its searchers, e.g. http://quickwit-searcher:7280 (required)")
	fs.Var((*stringList)(&c.KafkaBrokers), "kafka-brokers", "the Kafka cluster's brokers, comma-separated host:port, also Quickwit's sources' (required)")
	fs.IntVar(&c.KafkaPartitions, "kafka-partitions", c.KafkaPartitions, "the partitions of a new topic")
	fs.IntVar(&c.KafkaReplication, "kafka-replication", c.KafkaReplication, "the replicas of a new topic's partitions")
	fs.DurationVar(&c.KafkaRetention, "kafka-retention", c.KafkaRetention, "how long a new topic keeps its records: the replay buffer, not the store")
	fs.StringVar(&c.KafkaSASL, "kafka-sasl", c.KafkaSASL, "how the sink signs in to Kafka: none (plaintext) or aws-msk-iam (TLS and SASL AWS_MSK_IAM with the process's AWS credentials, Amazon MSK's port 9098)")
	fs.Var((*stringList)(&c.QuickwitKafkaBrokers), "quickwit-kafka-brokers", "the brokers Quickwit's Kafka sources read, comma-separated host:port (default: --kafka-brokers)")
	fs.StringVar(&c.QuickwitKafkaUsername, "quickwit-kafka-username", c.QuickwitKafkaUsername, "the SASL SCRAM-SHA-512 user of Quickwit's Kafka sources, over TLS (none: plaintext, no SASL)")
	fs.StringVar(&c.QuickwitKafkaPasswordFile, "quickwit-kafka-password-file", c.QuickwitKafkaPasswordFile, "the file of that user's password (required with --quickwit-kafka-username)")
	fs.StringVar(&c.IndexPrefix, "index-prefix", c.IndexPrefix, "the prefix of the log indexes' names, <prefix>-v<version>-<days>d")
	fs.IntVar(&c.IndexPartitions, "index-partitions", c.IndexPartitions, "how many partitions a new index hashes lines into by tenant, environment and job, 1 to 200 (an index keeps its own)")
	fs.DurationVar(&c.CommitTimeout, "index-commit-timeout", c.CommitTimeout, "how soon a line is searchable in the index, whole seconds (a new index's commit_timeout_secs)")
	fs.DurationVar(&c.Retention, "log-retention", c.Retention, "how long lines are kept, unless a stream asks for longer")
	fs.DurationVar(&c.RetentionMax, "log-retention-max", c.RetentionMax, "the longest a stream may ask for (ferun.retention_days)")
	fs.Var((*byteSize)(&c.Quota), "log-quota", "the bytes a tenant may store a day (bytes, or with a KiB, MiB or GiB suffix); 0: no quota")
	fs.Var((*byteSize)(&c.TraceQuota), "trace-quota", "the bytes of spans a tenant may store a day (bytes, or with a KiB, MiB or GiB suffix); 0: no quota")
	fs.DurationVar(&c.TraceRetention, "trace-retention", c.TraceRetention, "how long spans are kept")
	fs.StringVar(&c.TraceIndexPrefix, "trace-index-prefix", c.TraceIndexPrefix, "the prefix of the trace index's name, <prefix>-v<N>-<days>d")
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "json or text")
	return &level
}

// Validate checks a configuration.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Name == "" {
		bad("--name is required")
	} else if !pki.IsNodeName(c.Name) {
		bad("--name %q is not a DNS label", c.Name)
	}
	if c.Listen == "" || c.IngestListen == "" {
		bad("--listen and --ingest-listen must not be empty")
	}
	if c.DatabaseURL == "" {
		bad("--database-url is required")
	}
	if c.TrustDomain == "" {
		bad("--trust-domain is required")
	} else if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		bad("--trust-domain: %w", err)
	}
	if u, err := url.Parse(c.FerunURL); c.FerunURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		bad("--ferun-url must be an http(s) URL")
	}
	if c.ServiceToken == "" {
		bad("%s is required", EnvServiceToken)
	}
	if len(c.KafkaBrokers) == 0 {
		bad("--kafka-brokers is required")
	}
	if c.KafkaPartitions < 1 || c.KafkaReplication < 1 || c.KafkaRetention < time.Minute {
		bad("--kafka-partitions and --kafka-replication must be positive, --kafka-retention at least a minute")
	}
	if c.KafkaSASL != KafkaSASLNone && c.KafkaSASL != KafkaSASLAWSMSKIAM {
		bad("--kafka-sasl must be %s or %s, not %q", KafkaSASLNone, KafkaSASLAWSMSKIAM, c.KafkaSASL)
	}
	if (c.QuickwitKafkaUsername == "") != (c.QuickwitKafkaPasswordFile == "") {
		bad("--quickwit-kafka-username and --quickwit-kafka-password-file go together")
	} else if c.QuickwitKafkaUsername != "" && c.QuickwitKafkaPassword == "" {
		bad("--quickwit-kafka-password-file is empty")
	}
	for _, u := range []struct{ flag, v string }{{"--index-search-url", c.IndexSearchURL}} {
		if p, err := url.Parse(u.v); u.v == "" || err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			bad("%s must be an http(s) URL", u.flag)
		}
	}
	if c.IndexPrefix == "" || strings.ContainsAny(c.IndexPrefix, "*/ ") {
		bad("--index-prefix must be a name")
	}
	if c.IndexPartitions < 1 || c.IndexPartitions > quickwit.MaxPartitions {
		bad("--index-partitions must be 1 to %d", quickwit.MaxPartitions)
	}
	if c.TraceIndexPrefix == "" || strings.ContainsAny(c.TraceIndexPrefix, "*/ ") {
		bad("--trace-index-prefix must be a name")
	} else if strings.HasPrefix(c.TraceIndexPrefix+"-", c.IndexPrefix+"-") || strings.HasPrefix(c.IndexPrefix+"-", c.TraceIndexPrefix+"-") {
		bad("--trace-index-prefix and --index-prefix must not name the same indexes")
	}
	if c.CommitTimeout < time.Second || c.CommitTimeout%time.Second != 0 {
		bad("--index-commit-timeout must be whole seconds, at least one")
	}
	if c.Retention <= 0 || c.RetentionMax < c.Retention {
		bad("--log-retention must be positive and at most --log-retention-max")
	}
	if c.Quota < 0 {
		bad("--log-quota must not be negative")
	}
	if c.TraceRetention <= 0 {
		bad("--trace-retention must be positive")
	}
	if c.LogFormat != LogFormatJSON && c.LogFormat != LogFormatText {
		bad("--log-format must be %s or %s, not %q", LogFormatJSON, LogFormatText, c.LogFormat)
	}
	return errors.Join(errs...)
}

// SourceBrokers are the brokers of Quickwit's Kafka sources.
func (c *Config) SourceBrokers() []string {
	if len(c.QuickwitKafkaBrokers) > 0 {
		return c.QuickwitKafkaBrokers
	}
	return c.KafkaBrokers
}

// NewLogger returns the logger of a log format and level, writing to w.
func NewLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// Usage prints the help of serve.
func Usage(w io.Writer) {
	fs := newFlagSet("serve")
	c := Default()
	register(fs, &c)
	fmt.Fprintln(w, "Usage: ferun-sink [serve] [flags]\n\nRuns the sink until SIGTERM or SIGINT. Every flag has an environment variable "+
		EnvPrefix+"<NAME>;\nthe secret comes from "+EnvServiceToken+".\n\nFlags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses args into fs, then sets every flag not given from its
// environment variable; an empty variable counts as unset.
func parseFlags(fs *flag.FlagSet, args []string, getenv func(string) string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if given[f.Name] || getenv == nil {
			return
		}
		if v := getenv(EnvName(f.Name)); v != "" {
			if err := fs.Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", EnvName(f.Name), err))
			}
		}
	})
	return errors.Join(errs...)
}

// byteSize is a flag of bytes: an integer with an optional KiB, MiB or
// GiB suffix, as ferun-ingress writes them.
type byteSize int64

func (b *byteSize) String() string { return strconv.FormatInt(int64(*b), 10) }

func (b *byteSize) Set(v string) error {
	s := strings.TrimSpace(v)
	num, shift := s, 0
	for _, u := range []struct {
		suffix string
		shift  int
	}{{"KiB", 10}, {"MiB", 20}, {"GiB", 30}} {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			num, shift = strings.TrimSpace(n), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || n > (1<<62)>>shift {
		return fmt.Errorf("%q is not a byte size (an integer with an optional KiB, MiB or GiB suffix)", v)
	}
	*b = byteSize(n << shift)
	return nil
}

// stringList is a flag of a comma-separated list.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = nil
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			*l = append(*l, x)
		}
	}
	return nil
}
