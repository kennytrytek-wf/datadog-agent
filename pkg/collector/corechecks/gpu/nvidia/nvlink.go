// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux && nvml

package nvidia

import (
	"fmt"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/hashicorp/go-multierror"

	"github.com/DataDog/datadog-agent/pkg/collector/corechecks/gpu/model"
	"github.com/DataDog/datadog-agent/pkg/gpu/prm"
	ddnvml "github.com/DataDog/datadog-agent/pkg/gpu/safenvml"
	"github.com/DataDog/datadog-agent/pkg/metrics"
)

type prmMetricsSource interface {
	RegisterRequests([]model.PRMRequest)
	GetCounters(deviceUUID string, port int) (map[string]uint64, error)
}

type nvlinkCollector struct {
	device   ddnvml.Device
	ports    []int
	prmCache prmMetricsSource
}

func getNVLinkCount(device ddnvml.Device) (int, error) {
	fields := []nvml.FieldValue{{
		FieldId: nvml.FI_DEV_NVLINK_LINK_COUNT,
		ScopeId: 0,
	}}

	if err := device.GetFieldValues(fields); err != nil {
		return 0, fmt.Errorf("get NVLink link count: %w", err)
	}

	totalPorts, err := fieldValueToNumber[int](nvml.ValueType(fields[0].ValueType), fields[0].Value)
	if err != nil {
		return 0, fmt.Errorf("convert NVLink link count: %w", err)
	}
	return totalPorts, nil
}

func newNVLinkCollector(device ddnvml.Device, deps *CollectorDependencies) (Collector, error) {
	if deps == nil || deps.PRMCache == nil {
		return nil, errUnsupportedDevice
	}

	totalPorts, err := getNVLinkCount(device)
	if err != nil {
		if ddnvml.IsAPIUnsupportedOnDevice(err, device) {
			return nil, errUnsupportedDevice
		}
		return nil, err
	}
	if totalPorts <= 0 {
		return nil, errUnsupportedDevice
	}

	ports, requests := buildNVLinkRequests(device, totalPorts)

	c := &nvlinkCollector{
		device:   device,
		ports:    ports,
		prmCache: deps.PRMCache,
	}
	c.prmCache.RegisterRequests(requests)

	return c, nil
}

func (c *nvlinkCollector) DeviceUUID() string {
	return c.device.GetDeviceInfo().UUID
}

func (c *nvlinkCollector) Name() CollectorName {
	return nvlink
}

func (c *nvlinkCollector) Collect() ([]Metric, error) {
	var (
		allMetrics []Metric
		multiErr   error
	)

	for _, port := range c.ports {
		counters, err := c.prmCache.GetCounters(c.DeviceUUID(), port)
		if err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("read PLR counters for port %d: %w", port, err))
			continue
		}

		for _, field := range prm.PLRCounterFields {
			value, found := counters[field]
			if !found {
				multiErr = multierror.Append(multiErr, fmt.Errorf("missing PLR counter %q for port %d", field, port))
				continue
			}

			allMetrics = append(allMetrics, Metric{
				Name:  field,
				Value: float64(value),
				Type:  metrics.GaugeType,
				Tags: []string{
					fmt.Sprintf("nvlink_port:%d", port),
				},
				Priority: Medium,
			})
		}
	}

	if len(allMetrics) == 0 && multiErr != nil {
		return nil, multiErr
	}

	return allMetrics, multiErr
}

func buildNVLinkRequests(device ddnvml.Device, totalPorts int) ([]int, []model.PRMRequest) {
	ports := make([]int, 0, totalPorts)
	requests := make([]model.PRMRequest, 0, totalPorts)
	deviceUUID := device.GetDeviceInfo().UUID
	for port := 1; port <= totalPorts; port++ {
		ports = append(ports, port)
		requests = append(requests, model.PRMRequest{
			DeviceUUID: deviceUUID,
			Port:       port,
			Group:      prm.PPCNTGroupPLR,
		})
	}
	return ports, requests
}
