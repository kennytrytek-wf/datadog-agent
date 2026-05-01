// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build test

// Package metricstest provides test helpers for constructing the Fx-managed
// forwarder, demultiplexer and DogStatsD server bundle used by serverless-init.
// Tests build a ServerlessMetricAgent on top of the returned components.
package metricstest

import (
	"testing"

	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer"
	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer/demultiplexerimpl"
	configcomp "github.com/DataDog/datadog-agent/comp/core/config"
	delegatedauth "github.com/DataDog/datadog-agent/comp/core/delegatedauth/def"
	delegatedauthmock "github.com/DataDog/datadog-agent/comp/core/delegatedauth/mock"
	"github.com/DataDog/datadog-agent/comp/core/hostname/hostnameimpl"
	logdef "github.com/DataDog/datadog-agent/comp/core/log/def"
	logmock "github.com/DataDog/datadog-agent/comp/core/log/mock"
	secrets "github.com/DataDog/datadog-agent/comp/core/secrets/def"
	secretsmock "github.com/DataDog/datadog-agent/comp/core/secrets/mock"
	tagger "github.com/DataDog/datadog-agent/comp/core/tagger/def"
	nooptelemetry "github.com/DataDog/datadog-agent/comp/core/telemetry/fx-noop"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	workloadmetafx "github.com/DataDog/datadog-agent/comp/core/workloadmeta/fx"
	"github.com/DataDog/datadog-agent/comp/dogstatsd"
	dogstatsdServer "github.com/DataDog/datadog-agent/comp/dogstatsd/server"
	filterlistfx "github.com/DataDog/datadog-agent/comp/filterlist/fx"
	"github.com/DataDog/datadog-agent/comp/forwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/defaultforwarder"
	"github.com/DataDog/datadog-agent/comp/forwarder/eventplatform/eventplatformimpl"
	eventplatformreceiverimpl "github.com/DataDog/datadog-agent/comp/forwarder/eventplatformreceiver/eventplatformreceiverimpl"
	orchestratorimpl "github.com/DataDog/datadog-agent/comp/forwarder/orchestrator/orchestratorimpl"
	haagentfx "github.com/DataDog/datadog-agent/comp/haagent/fx"
	logscompressionfx "github.com/DataDog/datadog-agent/comp/serializer/logscompression/fx"
	metricscompressionfx "github.com/DataDog/datadog-agent/comp/serializer/metricscompression/fx"
	"github.com/DataDog/datadog-agent/pkg/aggregator"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
)

// Deps is the set of components the serverless-init metric agent depends on.
// It mirrors the bundle wired up in cmd/serverless-init/main.go.
type Deps struct {
	fx.In

	DogstatsdServer dogstatsdServer.Component
	Demultiplexer   demultiplexer.Component
	Demux           aggregator.Demultiplexer
	Forwarder       defaultforwarder.Component
}

// New constructs the Fx-managed forwarder, demultiplexer and DogStatsD server
// using the same module graph as cmd/serverless-init/main.go. The provided
// tagger component is injected into the graph.
func New(t *testing.T, taggerComp tagger.Component) Deps {
	t.Helper()
	return fxutil.Test[Deps](t,
		fx.Supply(configcomp.NewParams("")),
		configcomp.Module(),
		fx.Provide(func() logdef.Component { return logmock.New(t) }),
		fx.Provide(func() secrets.Component { return secretsmock.New(t) }),
		fx.Provide(func() delegatedauth.Component { return delegatedauthmock.New(t) }),
		fx.Provide(func() tagger.Component { return taggerComp }),
		nooptelemetry.Module(),
		workloadmetafx.Module(workloadmeta.NewParams()),
		orchestratorimpl.Module(orchestratorimpl.NewNoopParams()),
		eventplatformimpl.Module(eventplatformimpl.NewDisabledParams()),
		eventplatformreceiverimpl.Module(),
		haagentfx.Module(),
		metricscompressionfx.Module(),
		logscompressionfx.Module(),
		filterlistfx.Module(),
		hostnameimpl.MockModule(),
		forwarder.Bundle(defaultforwarder.NewParams()),
		demultiplexerimpl.Module(demultiplexerimpl.NewDefaultParams(
			demultiplexerimpl.WithContinueOnMissingHostname(),
		)),
		dogstatsd.Bundle(dogstatsdServer.Params{Serverless: true}),
	)
}
