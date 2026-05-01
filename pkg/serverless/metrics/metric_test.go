// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package metrics

import (
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/comp/core"
	delegatedauthmock "github.com/DataDog/datadog-agent/comp/core/delegatedauth/mock"
	"github.com/DataDog/datadog-agent/comp/core/hostname/hostnameimpl"
	secrets "github.com/DataDog/datadog-agent/comp/core/secrets/def"
	secretsmock "github.com/DataDog/datadog-agent/comp/core/secrets/mock"
	nooptagger "github.com/DataDog/datadog-agent/comp/core/tagger/impl-noop"
	"github.com/DataDog/datadog-agent/comp/dogstatsd/listeners"
	filterlistmock "github.com/DataDog/datadog-agent/comp/filterlist/fx-mock"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder/resolver"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder/transaction"
	haagentmock "github.com/DataDog/datadog-agent/comp/haagent/mock"
	logscompression "github.com/DataDog/datadog-agent/comp/serializer/logscompression/fx-mock"
	metricscompression "github.com/DataDog/datadog-agent/comp/serializer/metricscompression/fx-mock"
	"github.com/DataDog/datadog-agent/pkg/aggregator"
	configmock "github.com/DataDog/datadog-agent/pkg/config/mock"
	pkgconfigsetup "github.com/DataDog/datadog-agent/pkg/config/setup"
	configutils "github.com/DataDog/datadog-agent/pkg/config/utils"
	pkgmetrics "github.com/DataDog/datadog-agent/pkg/metrics"
	"github.com/DataDog/datadog-agent/pkg/serverless/metrics/metricstest"
	"github.com/DataDog/datadog-agent/pkg/util/cache"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/hostname"
)

func TestMain(m *testing.M) {
	// setting the hostname cache saves about 1s when starting the metric agent
	cacheKey := cache.BuildAgentKey("hostname")
	cache.Cache.Set(cacheKey, hostname.Data{}, cache.NoExpiration)
	os.Exit(m.Run())
}

func TestConstructionDoesNotBlock(t *testing.T) {
	if os.Getenv("CI") == "true" && runtime.GOOS == "darwin" {
		t.Skip("known to fail on the macOS Gitlab runners because of the already running Agent")
	}
	mockConfig := configmock.New(t)
	pkgconfigsetup.LoadDatadog(mockConfig, secretsmock.New(t), delegatedauthmock.New(t), nil)
	deps := metricstest.New(t, nooptagger.NewComponent())
	metricAgent := &ServerlessMetricAgent{Demux: deps.Demux}
	assert.NotNil(t, metricAgent.Demux)
}

func TestRaceFlushVersusParsePacket(t *testing.T) {
	mockConfig := configmock.New(t)
	pkgconfigsetup.LoadDatadog(mockConfig, secretsmock.New(t), delegatedauthmock.New(t), nil)
	mockConfig.SetDefault("dogstatsd_port", listeners.RandomPortName)

	deps := metricstest.New(t, nooptagger.NewComponent())

	url := deps.DogstatsdServer.UDPLocalAddr()
	conn, err := net.Dial("udp", url)
	require.NoError(t, err, "cannot connect to DSD socket")
	defer conn.Close()

	finish := &sync.WaitGroup{}
	finish.Add(2)

	go func(wg *sync.WaitGroup) {
		for i := 0; i < 1000; i++ {
			conn.Write([]byte("daemon:666|g|#sometag1:somevalue1,sometag2:somevalue2"))
			time.Sleep(10 * time.Nanosecond)
		}
		wg.Done()
	}(finish)

	go func(wg *sync.WaitGroup) {
		for i := 0; i < 1000; i++ {
			deps.DogstatsdServer.ServerlessFlush(time.Second * 10)
		}
		wg.Done()
	}(finish)

	finish.Wait()
}

// countingForwarder wraps NoopForwarder, provides a real domain resolver so that
// the serializer's pipeline path is exercised, and counts sketch transactions.
type countingForwarder struct {
	defaultforwarder.NoopForwarder
	sketchCount atomic.Int64
	resolvers   []resolver.DomainResolver
}

func newCountingForwarder() *countingForwarder {
	r, _ := resolver.NewSingleDomainResolver("https://fake.datadoghq.com",
		[]configutils.APIKeys{configutils.NewAPIKeys("api_key", "fakeapikey")})
	return &countingForwarder{resolvers: []resolver.DomainResolver{r}}
}

// GetDomainResolvers returns the fake resolver so buildPipelines creates a pipeline.
func (f *countingForwarder) GetDomainResolvers() []resolver.DomainResolver {
	return f.resolvers
}

// SubmitTransaction increments the sketch counter when a sketch-series transaction arrives.
func (f *countingForwarder) SubmitTransaction(txn *transaction.HTTPTransaction) error {
	if strings.Contains(txn.Endpoint.Name, "sketch") {
		f.sketchCount.Add(1)
	}
	return nil
}

// SubmitSketchSeries is kept for interface compliance but is not called by the pipeline path.
func (f *countingForwarder) SubmitSketchSeries(_ transaction.BytesPayloads, _ http.Header) error {
	return nil
}

// TestFlushOnStopDeliversSample asserts that a sample submitted via AddEnhancedMetric
// immediately before ForceFlushToSerializer is reliably delivered to the serializer.
// It runs 100 iterations to catch the ~50% race the deleted TestWaitForPendingSamplesThenFlush
// was designed to detect: the timeSamplerWorker's select can pick flushChan over samplesChan,
// causing a flush before the sample is enqueued.
func TestFlushOnStopDeliversSample(t *testing.T) {
	mockConfig := configmock.New(t)
	pkgconfigsetup.LoadDatadog(mockConfig, secretsmock.New(t), delegatedauthmock.New(t), nil)

	cf := newCountingForwarder()

	deps := fxutil.Test[aggregator.TestDeps](t,
		fx.Provide(func() secrets.Component { return secretsmock.New(t) }),
		fx.Provide(func() defaultforwarder.Component { return cf }),
		core.MockBundle(),
		hostnameimpl.MockModule(),
		haagentmock.Module(),
		logscompression.MockModule(),
		metricscompression.MockModule(),
		filterlistmock.MockModule(),
	)

	const iterations = 100
	for i := 0; i < iterations; i++ {
		opts := aggregator.DefaultAgentDemultiplexerOptions()
		opts.FlushInterval = time.Hour // disable automatic flushes
		opts.DontStartForwarders = true
		demux := aggregator.InitAndStartAgentDemultiplexerForTest(deps, opts, "")

		agent := New(demux, Tags{})
		// Use a fixed past timestamp so the sample falls in a completed bucket
		// relative to the flush time (time.Now()), ensuring flushBefore removes it.
		agent.AddEnhancedMetric("test.metric", 1.0, pkgmetrics.MetricSourceServerless, 1000.0)
		// Yield so the timeSamplerWorker goroutine can drain samplesChan before
		// ForceFlushToSerializer sends to flushChan. Without this yield the Go
		// scheduler may pick flushChan first (the race this test exercises).
		runtime.Gosched()
		demux.ForceFlushToSerializer(time.Now(), true)
		demux.Stop(false)
	}

	require.Equal(t, int64(iterations), cf.sketchCount.Load(),
		"every AddEnhancedMetric call must produce exactly one sketch flush")
}
