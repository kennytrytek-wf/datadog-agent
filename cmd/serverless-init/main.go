// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !windows

package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/DataDog/datadog-agent/cmd/serverless-init/exitcode"
	serverlessInitLog "github.com/DataDog/datadog-agent/cmd/serverless-init/log"
	"github.com/DataDog/datadog-agent/cmd/serverless-init/mode"
	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer"
	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer/demultiplexerimpl"
	"github.com/DataDog/datadog-agent/comp/core/autodiscovery"
	"github.com/DataDog/datadog-agent/comp/core/autodiscovery/autodiscoveryimpl"
	coreconfig "github.com/DataDog/datadog-agent/comp/core/config"
	delegatedauth "github.com/DataDog/datadog-agent/comp/core/delegatedauth/def"
	delegatedauthfx "github.com/DataDog/datadog-agent/comp/core/delegatedauth/fx"
	delegatedauthnooptypes "github.com/DataDog/datadog-agent/comp/core/delegatedauth/noop-impl/types"
	healthprobeDef "github.com/DataDog/datadog-agent/comp/core/healthprobe/def"
	healthprobeFx "github.com/DataDog/datadog-agent/comp/core/healthprobe/fx"
	"github.com/DataDog/datadog-agent/comp/core/hostname/hostnameimpl"
	"github.com/DataDog/datadog-agent/comp/core/hostname/hostnameinterface"
	logdef "github.com/DataDog/datadog-agent/comp/core/log/def"
	logfx "github.com/DataDog/datadog-agent/comp/core/log/fx"
	secrets "github.com/DataDog/datadog-agent/comp/core/secrets/def"
	secretsfx "github.com/DataDog/datadog-agent/comp/core/secrets/fx"
	secretnooptypes "github.com/DataDog/datadog-agent/comp/core/secrets/noop-impl/types"
	tagger "github.com/DataDog/datadog-agent/comp/core/tagger/def"
	localTaggerFx "github.com/DataDog/datadog-agent/comp/core/tagger/fx"
	nooptelemetry "github.com/DataDog/datadog-agent/comp/core/telemetry/fx-noop"
	workloadfilterfx "github.com/DataDog/datadog-agent/comp/core/workloadfilter/fx"
	"github.com/DataDog/datadog-agent/comp/dogstatsd"
	dogstatsdServer "github.com/DataDog/datadog-agent/comp/dogstatsd/server"
	filterlistfx "github.com/DataDog/datadog-agent/comp/filterlist/fx"
	"github.com/DataDog/datadog-agent/comp/forwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/eventplatform/eventplatformimpl"
	eventplatformreceiverimpl "github.com/DataDog/datadog-agent/comp/forwarder/eventplatformreceiver/eventplatformreceiverimpl"
	orchestratorimpl "github.com/DataDog/datadog-agent/comp/forwarder/orchestrator/orchestratorimpl"
	haagentfx "github.com/DataDog/datadog-agent/comp/haagent/fx"
	healthplatform "github.com/DataDog/datadog-agent/comp/healthplatform"
	logscompression "github.com/DataDog/datadog-agent/comp/serializer/logscompression/def"
	logscompressionfx "github.com/DataDog/datadog-agent/comp/serializer/logscompression/fx"
	metricscompressionfx "github.com/DataDog/datadog-agent/comp/serializer/metricscompression/fx"

	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	workloadmetafx "github.com/DataDog/datadog-agent/comp/core/workloadmeta/fx"
	"github.com/DataDog/datadog-agent/pkg/aggregator"

	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/cmd/serverless-init/cloudservice"
	enhancedmetrics "github.com/DataDog/datadog-agent/cmd/serverless-init/enhanced-metrics"
	serverlessInitTag "github.com/DataDog/datadog-agent/cmd/serverless-init/tag"
	logsAgent "github.com/DataDog/datadog-agent/comp/logs/agent"
	"github.com/DataDog/datadog-agent/pkg/config/model"
	pkgconfigsetup "github.com/DataDog/datadog-agent/pkg/config/setup"
	configUtils "github.com/DataDog/datadog-agent/pkg/config/utils"
	"github.com/DataDog/datadog-agent/pkg/serverless/metrics"
	"github.com/DataDog/datadog-agent/pkg/serverless/otlp"
	serverlessTag "github.com/DataDog/datadog-agent/pkg/serverless/tags"
	"github.com/DataDog/datadog-agent/pkg/serverless/trace"
	tracelog "github.com/DataDog/datadog-agent/pkg/trace/log"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const datadogConfigPath = "datadog.yaml"

// Shutdown time budget for serverless-init. These values sum to ~9s, which
// fits within the tightest supported platform grace window. Platform defaults:
//   Cloud Run (tightest): 10s — https://cloud.google.com/run/docs/container-contract#shutdown
//   Azure Container Apps:  30s — https://learn.microsoft.com/en-us/azure/container-apps/application-lifecycle-management#shutdown
//   Azure App Service:     30s via WEBSITES_SHUTDOWN_TIMEOUT (scale-in SIGTERM delivery is unreliable)
// Any change here is the only edit needed — all phases read these constants.
//
// The three metrics-pipeline bounds (aggregator → forwarder) are separate
// because the metrics path stages independently: the demux/aggregator drains
// its sample channel and serializes the final payload, then the forwarder
// purges any HTTP transactions queued during that drain.
const (
	// traceStopTimeout bounds Stop()'s wait for the trace agent's Run loop
	// to exit. Best-effort; logs a warning on overrun and continues shutdown.
	traceStopTimeout = 3 * time.Second

	// logsFlushTimeout bounds the flush of buffered customer log records via
	// flushLogsAgent. Strict ctx; cancels in-progress sends on overrun.
	logsFlushTimeout = 2 * time.Second

	// metricsAggregatorStopTimeoutSeconds bounds the demux Stop: forces a
	// final flush of the metrics pipeline, including incomplete dogstatsd
	// buckets, into the serializer and through the forwarder. Set via the
	// aggregator_stop_timeout config key (integer seconds).
	metricsAggregatorStopTimeoutSeconds = 2

	// metricsForwarderStopTimeoutSeconds bounds the purge phase of the
	// DefaultForwarder. In-flight HTTP requests continue in background
	// goroutines after Stop returns. Set via the forwarder_stop_timeout
	// config key (integer seconds).
	metricsForwarderStopTimeoutSeconds = 2

	// metricsFlushInterval is the demux periodic flush cadence during the
	// run (not shutdown). Kept here so the full picture is in one place: a
	// tick that fires close to SIGTERM consumes part of the shutdown budget.
	metricsFlushInterval = 3 * time.Second

	// shutdownBudgetWatchdog fires a debug log if total shutdown elapsed time
	// exceeds this. Sum of the four phase budgets is 9 s; this gives a tiny
	// buffer before Cloud Run's 10 s SIGTERM-to-SIGKILL grace window expires.
	shutdownBudgetWatchdog = 9*time.Second + 500*time.Millisecond
)

var modeConf mode.Conf

// preloadEarly mutates the global Datadog() config with the overrides that the
// forwarder, DogStatsD server and demultiplexer read at Fx-construction time.
// These must be applied before fxutil.OneShot so that they win over any values
// loaded from env vars or datadog.yaml (SourceAgentRuntime has higher priority
// than SourceEnvVar/SourceFile).
func preloadEarly(metricAgentTags []string) {
	// Serverless containers don't persist across restarts, so disk-spill of
	// undelivered transactions has no value. Disable it explicitly even though
	// the default is already 0 — keeps behavior deterministic under user
	// overrides or future default changes.
	pkgconfigsetup.Datadog().Set("forwarder_storage_max_size_in_bytes", 0, model.SourceAgentRuntime)

	// Bound DefaultForwarder.Stop()'s purge window so shutdown stays inside
	// the platform grace budget (Cloud Run default 10s), leaving room for
	// the aggregator flush, logs flush, and trace agent stop.
	pkgconfigsetup.Datadog().Set("forwarder_stop_timeout", metricsForwarderStopTimeoutSeconds, model.SourceAgentRuntime)

	// Bound AgentDemultiplexer.Stop()'s flush window. Explicit here so all
	// metrics-pipeline bounds are in one place and readable alongside the
	// forwarder timeout above.
	pkgconfigsetup.Datadog().Set("aggregator_stop_timeout", metricsAggregatorStopTimeoutSeconds, model.SourceAgentRuntime)

	// Prevent any UDP packets from being stuck in the buffer and not parsed:
	// with this option set to 1ms, all packets received are immediately sent
	// to the parser.
	pkgconfigsetup.Datadog().Set("dogstatsd_packet_buffer_flush_timeout", 1*time.Millisecond, model.SourceAgentRuntime)

	// DogStatsD in serverless-init listens on UDP only — suppress the default
	// UDS listener, which requires a filesystem socket that has no use here.
	pkgconfigsetup.Datadog().Set("dogstatsd_socket", "", model.SourceAgentRuntime)

	// Force series v2 API for the serializer.
	pkgconfigsetup.Datadog().Set("use_v2_api.series", true, model.SourceAgentRuntime)

	// Disable UDS listener for the APM receiver — traces are sent via HTTP to
	// localhost in serverless. Avoids noisy error logs.
	pkgconfigsetup.Datadog().Set("apm_config.receiver_socket", "", model.SourceAgentRuntime)

	// Pass the serverless metric-agent tags to the DogStatsD server via the
	// standard config key. newServerCompat reads this at construction time.
	pkgconfigsetup.Datadog().Set("dogstatsd_tags", metricAgentTags, model.SourceAgentRuntime)

	// Every serverless-init environment (Cloud Run services and jobs, Container
	// Apps, App Service, local) can terminate before the current bucket closes,
	// so the final ForceFlushToSerializer call on Stop needs to include
	// incomplete bucket contents — otherwise short-lived workloads drop their
	// last bucket. This only affects the shutdown flush: AgentDemultiplexer's
	// periodic flushLoop hardcodes forceFlushAll=false on ticks, so
	// bucket-aligned flushes during the run are preserved.
	pkgconfigsetup.Datadog().Set("dogstatsd_flush_incomplete_buckets", true, model.SourceAgentRuntime)
}

func main() {

	modeConf = mode.DetectMode()
	setEnvWithoutOverride(modeConf.EnvDefaults)

	// Detect the cloud service and compute tags before Fx starts so that
	// dogstatsd_tags is in place when the DogStatsD server is constructed.
	cloudService := cloudservice.GetCloudServiceType()
	log.Debugf("Detected cloud service: %s", cloudService.GetOrigin())
	tagConfig := configureTags(cloudService)
	metricAgentTags := serverlessTag.MapToArray(serverlessInitTag.MakeMetricAgentTags(tagConfig.Tags))

	preloadEarly(metricAgentTags)

	// Load the config file early so that yaml-configured values (e.g.
	// api_key) are visible to the api_key check below. setup() calls
	// LoadDatadog again with the real Fx-injected components; that is
	// intentional — the second call resolves secrets and applies any
	// delegated-auth overrides. The noop implementations used here are
	// consistent with the pattern in cmd/agent/common/import.go.
	if err := pkgconfigsetup.LoadDatadog(pkgconfigsetup.Datadog(), &secretnooptypes.SecretNoop{}, &delegatedauthnooptypes.DelegatedAuthNoop{}, nil); err != nil {
		log.Debugf("early config load error (non-fatal): %v", err)
	}

	// Fast-fail: if no API key is configured (via env var or yaml), the
	// dogstatsd server has no useful work — every sample it ingests would be
	// dropped by the forwarder with an HTTP error (the one-shot forwarder
	// Errorf still fires once even with use_dogstatsd=false, which is
	// expected). Skip its lifecycle hooks entirely. The forwarder and demux
	// are still wired up by Fx (matching Core Agent behavior) but stay quiet
	// because nothing produces samples.
	//
	// We only check the top-level api_key here. apm_config.api_key is
	// checked separately inside setup() and is unaffected.
	if configUtils.SanitizeAPIKey(pkgconfigsetup.Datadog().GetString("api_key")) == "" {
		pkgconfigsetup.Datadog().Set("use_dogstatsd", false, model.SourceAgentRuntime)
	}

	metricTags := metrics.Tags{
		Metric:              metricAgentTags,
		EnhancedMetric:      serverlessTag.MapToArray(tagConfig.EnhancedMetricTags),
		EnhancedUsageMetric: serverlessTag.MapToArray(tagConfig.EnhancedUsageMetricTags),
	}

	err := fxutil.OneShot(
		run,
		fx.Provide(func() cloudservice.CloudService { return cloudService }),
		fx.Supply(tagConfig),
		fx.Supply(metricTags),
		delegatedauthfx.Module(),
		workloadfilterfx.Module(),
		autodiscoveryimpl.Module(),
		healthplatform.Bundle(),
		fx.Provide(func(config coreconfig.Component) healthprobeDef.Options {
			return healthprobeDef.Options{
				Port:           config.GetInt("health_port"),
				LogsGoroutines: config.GetBool("log_all_goroutines_when_unhealthy"),
			}
		}),
		localTaggerFx.Module(),
		healthprobeFx.Module(),
		workloadmetafx.Module(workloadmeta.NewParams()),
		fx.Supply(coreconfig.NewParams("")),
		coreconfig.Module(),
		logscompressionfx.Module(),
		metricscompressionfx.Module(),
		filterlistfx.Module(),
		orchestratorimpl.Module(orchestratorimpl.NewNoopParams()),
		eventplatformimpl.Module(eventplatformimpl.NewDisabledParams()),
		eventplatformreceiverimpl.Module(),
		haagentfx.Module(),
		forwarder.Bundle(defaultforwarder.NewParams()),
		demultiplexerimpl.Module(demultiplexerimpl.NewDefaultParams(
			demultiplexerimpl.WithFlushInterval(metricsFlushInterval),
			demultiplexerimpl.WithContinueOnMissingHostname(),
		)),
		dogstatsd.Bundle(dogstatsdServer.Params{Serverless: true}),
		secretsfx.Module(),
		fx.Supply(logdef.ForOneShot(modeConf.LoggerName, "error", true)),
		logfx.Module(),
		nooptelemetry.Module(),
		hostnameimpl.Module(),
	)

	if err != nil {
		log.Error(err)
		exitCode := exitcode.From(err)
		log.Debugf("propagating exit code %v", exitCode)
		log.Flush()
		os.Exit(exitCode)
	}
}

// removing these unused dependencies will cause silent crash due to fx framework
func run(
	secretComp secrets.Component,
	delegatedAuthComp delegatedauth.Component,
	_ autodiscovery.Component,
	_ healthprobeDef.Component,
	tagger tagger.Component,
	logsCompression logscompression.Component,
	hostname hostnameinterface.Component,
	_ defaultforwarder.Component,
	_ demultiplexer.Component,
	demux aggregator.Demultiplexer,
	_ dogstatsdServer.Component,
	cloudService cloudservice.CloudService,
	tagConfig tagConfiguration,
	metricTags metrics.Tags,
) error {
	cloudService, logConfig, tracingCtx, metricAgent, logsAgent, enhancedMetricsCollector, enhancedMetricsEnabled := setup(
		secretComp, delegatedAuthComp, modeConf, tagger, logsCompression, hostname,
		cloudService, tagConfig, metricTags, demux,
	)

	err := modeConf.Runner(logConfig)

	// Defers are LIFO. Order of execution:
	//   1. Watchdog timer starts (debug log if shutdown exceeds 9.5 s).
	//   2. cloudService.Shutdown submits the task.ended metric; the enhanced
	//      metrics collector stops emitting.
	//   3. trace agent stops (drains traces, flushes stats, sends) — bounded
	//      by traceStopTimeout (3 s).
	//   4. logs agent flushes any buffered records — bounded by
	//      logsFlushTimeout (2 s).
	//   5. run() returns; Fx OnStop fires demux.Stop(true) which performs the
	//      final metric flush (incomplete buckets included via
	//      dogstatsd_flush_incomplete_buckets) — bounded by
	//      metricsAggregatorStopTimeoutSeconds (2 s) — then drains the
	//      forwarder — bounded by metricsForwarderStopTimeoutSeconds (2 s).
	//
	// IMPORTANT: by convention, no new metric samples should be enqueued past
	// step 2. Steps 3-5 may run concurrently with a periodic flush tick;
	// although Stop(true) drains anything still buffered, samples added
	// after step 2 race with the budget rather than producing useful data.
	defer flushLogsAgent(logConfig.FlushTimeout, logsAgent)
	defer tracingCtx.TraceAgent.Stop()
	defer func() {
		cloudService.Shutdown(metricAgent, enhancedMetricsEnabled, err)

		if enhancedMetricsCollector != nil {
			enhancedMetricsCollector.Stop()
		}
	}()
	// Watchdog: log a single debug line if shutdown overruns the budget.
	// Declared last → executes first → captures shutdown start. The timer
	// runs in the background; if shutdown finishes early the log just never
	// fires (the goroutine is killed when the process exits). No cleanup
	// needed.
	defer func() {
		start := time.Now()
		time.AfterFunc(shutdownBudgetWatchdog, func() {
			log.Debugf("serverless-init shutdown exceeded %v budget (elapsed: %v)",
				shutdownBudgetWatchdog, time.Since(start))
		})
	}()

	return err
}

func setup(
	secretComp secrets.Component,
	delegatedAuthComp delegatedauth.Component,
	_ mode.Conf,
	tagger tagger.Component,
	compression logscompression.Component,
	hostname hostnameinterface.Component,
	cloudService cloudservice.CloudService,
	tagConfig tagConfiguration,
	metricTags metrics.Tags,
	demux aggregator.Demultiplexer,
) (cloudservice.CloudService, *serverlessInitLog.Config, *cloudservice.TracingContext, *metrics.ServerlessMetricAgent, logsAgent.ServerlessLogsAgent, *enhancedmetrics.Collector, bool) {
	tracelog.SetLogger(log.NewWrapper(3))

	// load proxy settings
	pkgconfigsetup.LoadProxyFromEnv(pkgconfigsetup.Datadog())

	defaultSource := cloudService.GetDefaultLogsSource()
	agentLogConfig := serverlessInitLog.CreateConfig(defaultSource, logsFlushTimeout)

	// The datadog-agent requires Load to be called or it could
	// panic down the line.
	err := pkgconfigsetup.LoadDatadog(pkgconfigsetup.Datadog(), secretComp, delegatedAuthComp, nil)
	if err != nil {
		log.Debugf("Error loading config: %v\n", err)
	}

	origin := cloudService.GetOrigin()
	// Note: we do not modify tags for the LogsAgent.
	logsAgent := serverlessInitLog.SetupLogAgent(agentLogConfig, tagConfig.Tags, tagger, compression, hostname, origin)

	// When no API key is configured, skip trace agent initialization
	// to avoid noisy error logs. The process wrapper and logs agent still function normally.
	// Also check the deprecated apm_config.api_key, which the trace agent still honors.
	// NOTE: the Fx-managed forwarder, demultiplexer and DogStatsD server are
	// constructed and started unconditionally during fxutil.OneShot startup.
	// Without an API key the forwarder will log HTTP errors, but the process
	// continues to run (matching Core Agent behavior).
	apiKey := configUtils.SanitizeAPIKey(pkgconfigsetup.Datadog().GetString("api_key"))
	apmAPIKey := configUtils.SanitizeAPIKey(pkgconfigsetup.Datadog().GetString("apm_config.api_key"))
	if apiKey == "" && apmAPIKey == "" {
		log.Warnf("DD_API_KEY is not set; trace and metric collection are disabled. Set DD_API_KEY to enable monitoring.")
		traceAgent := trace.NewNoopTraceAgent()
		tracingCtx := &cloudservice.TracingContext{TraceAgent: traceAgent}
		return cloudService, agentLogConfig, tracingCtx, nil, logsAgent, nil, false
	}

	traceTags := serverlessInitTag.MakeTraceAgentTags(tagConfig.Tags)
	traceAgent := setupTraceAgent(traceTags, tagConfig.ConfiguredTags, tagger, origin)

	tracingCtx := &cloudservice.TracingContext{
		TraceAgent: traceAgent,
		SpanTags:   traceTags,
	}

	// TODO check for errors and exit
	_ = cloudService.Init(tracingCtx)

	metricAgent := metrics.New(demux, metricTags)

	enhancedMetricsEnabled := pkgconfigsetup.Datadog().GetBool("enhanced_metrics")
	if enhancedMetricsEnabled {
		cloudService.AddStartMetric(metricAgent)
	}

	setupOtlpAgent(metricAgent, tagger)

	var enhancedMetricsCollector *enhancedmetrics.Collector
	if enhancedMetricsEnabled {
		enhancedMetricsCollector, err = enhancedmetrics.NewCollector(metricAgent, cloudService.GetSource(), cloudService.GetMetricPrefix(), cloudService.GetUsageMetricSuffix(), 3*time.Second)
		if err != nil {
			log.Warnf("Failed to initialize enhanced metrics collector: %v", err)
		} else {
			go enhancedMetricsCollector.Start()
		}
	}

	return cloudService, agentLogConfig, tracingCtx, metricAgent, logsAgent, enhancedMetricsCollector, enhancedMetricsEnabled
}

// tagConfiguration holds the various tag sets for telemetry.
type tagConfiguration struct {
	ConfiguredTags []string // tags derived from DD_TAGS and DD_EXTRA_TAGS

	// tags derived from DD_TAGS and DD_EXTRA_TAGS, service, env, version, and tags derived from cloud service.
	// for use on dogstatsd metrics, legacy enhanced metrics, logs, and traces.
	Tags                    map[string]string
	EnhancedMetricTags      map[string]string // subset of tags derived from cloud service for enhanced metrics.
	EnhancedUsageMetricTags map[string]string // subset of tags derived from cloud service for enhanced usage metrics, including a high cardinality instance/replica tag.
}

func configureTags(cloudService cloudservice.CloudService) tagConfiguration {
	configuredTags := configUtils.GetConfiguredTags(pkgconfigsetup.Datadog(), false)
	configuredTagsMap := serverlessTag.ArrayToMap(configuredTags)

	baseTags := serverlessInitTag.GetBaseTagsMap()
	cloudTags := cloudService.GetTags()

	tags := serverlessTag.MergeWithOverwrite(baseTags, configuredTagsMap, cloudTags)

	serverlessInitTag.SetVersionMode(tags, modeConf.TagVersionMode)

	enhancedMetricTagSets := cloudService.GetEnhancedMetricTags(cloudTags)
	enhancedMetricTags := serverlessTag.MergeWithOverwrite(baseTags, configuredTagsMap, enhancedMetricTagSets.Base)

	serverlessInitTag.SetVersionMode(enhancedMetricTags, modeConf.TagVersionModeEnhancedMetrics)
	serverlessInitTag.SetSidecarModeTag(enhancedMetricTags, modeConf.SidecarMode)

	serverlessInitTag.SetVersionMode(enhancedMetricTagSets.Usage, modeConf.TagVersionModeEnhancedMetrics)
	serverlessInitTag.SetSidecarModeTag(enhancedMetricTagSets.Usage, modeConf.SidecarMode)

	return tagConfiguration{
		ConfiguredTags:          configuredTags,
		Tags:                    tags,
		EnhancedMetricTags:      enhancedMetricTags,
		EnhancedUsageMetricTags: enhancedMetricTagSets.Usage,
	}
}

var serverlessProfileTags = []string{
	// Azure tags
	"subscription_id",
	"resource_group",
	"resource_id",
	"replicate_name",
	"aca.subscription.id",
	"aca.resource.group",
	"aca.resource.id",
	"aca.replica.name",
	"aas.subscription.id",
	"aas.resource.group",
	"aas.resource.id",
	// Cloud-agnostic origin tag
	"_dd.origin",
}

func setupTraceAgent(tags map[string]string, configuredTags []string, tagger tagger.Component, origin string) trace.ServerlessTraceAgent {
	profileTags := make(map[string]string)
	for _, serverlessProfileTag := range serverlessProfileTags {
		if value, ok := tags[serverlessProfileTag]; ok {
			profileTags[serverlessProfileTag] = value
		}
	}

	// For Google Cloud Run Functions, add functionname tag to profiles so the profiling team can filter by functions
	if origin == cloudservice.CloudRunOrigin {
		_, functionTargetExists := os.LookupEnv("FUNCTION_TARGET")

		if functionTargetExists {
			profileTags["functionname"] = os.Getenv(cloudservice.ServiceNameEnvVar)
		}
	}

	// Note: serverless trace tag logic also in comp/trace/payload-modifier/impl/payloadmodifier_test.go
	functionTags := strings.Join(configuredTags, ",")
	traceAgent := trace.StartServerlessTraceAgent(trace.StartServerlessTraceAgentArgs{
		Enabled:               pkgconfigsetup.Datadog().GetBool("apm_config.enabled"),
		LoadConfig:            &trace.LoadConfig{Path: datadogConfigPath, Tagger: tagger},
		AdditionalProfileTags: profileTags,
		FunctionTags:          functionTags,
		StopTimeout:           traceStopTimeout,
	})
	traceAgent.SetTags(tags)
	go func() {
		for range time.Tick(3 * time.Second) {
			traceAgent.Flush()
		}
	}()
	return traceAgent
}

func setupOtlpAgent(metricAgent *metrics.ServerlessMetricAgent, tagger tagger.Component) {
	if !otlp.IsEnabled() {
		log.Debugf("otlp endpoint disabled")
		return
	}

	if metricAgent == nil || metricAgent.Demux == nil {
		log.Warn("metric agent or demux not ready, skipping OTLP agent setup")
		return
	}

	otlpAgent := otlp.NewServerlessOTLPAgent(metricAgent.Demux.Serializer(), tagger)
	otlpAgent.Start()
}

// flushLogsAgent flushes the logs agent with a bounded timeout. Metrics are
// flushed by the demultiplexer's Fx OnStop hook (demux.Stop(true)), so this
// helper is logs-only.
func flushLogsAgent(flushTimeout time.Duration, agent logsAgent.ServerlessLogsAgent) {
	if agent == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	agent.Flush(ctx)
}

func setEnvWithoutOverride(envToSet map[string]string) {
	for envName, envVal := range envToSet {
		if val, set := os.LookupEnv(envName); !set {
			os.Setenv(envName, envVal)
		} else {
			log.Debugf("%s already set with %s, skipping setting it", envName, val)
		}
	}
}
