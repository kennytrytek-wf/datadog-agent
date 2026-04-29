// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux && nvml

package nvidia

import (
	"fmt"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	ddnvml "github.com/DataDog/datadog-agent/pkg/gpu/safenvml"
)

type nvlinkCollector interface {
	Collector
	Port() int
}

type nvlinkCollectorBuilder func(device ddnvml.Device, port int, deps *CollectorDependencies) (Collector, error)

var nvlinkBuilders = map[CollectorName]nvlinkCollectorBuilder{
	nvlinkPLR: newNVLinkPLRCollector,
}

func getNvlinkBuilders(device ddnvml.Device) ([]subsystemBuilder, error) {
	totalPorts, err := getNVLinkCount(device)
	if err != nil {
		return nil, fmt.Errorf("get NVLink count: %w", err)
	}

	if totalPorts <= 0 {
		return nil, fmt.Errorf("%w: no ports found", errUnsupportedDevice)
	}

	var builders []subsystemBuilder
	for port := 1; port <= totalPorts; port++ {
		for name, nvlinkBuilder := range nvlinkBuilders {
			portBuilder := func(device ddnvml.Device, deps *CollectorDependencies) (Collector, error) {
				return nvlinkBuilder(device, port, deps)
			}

			builders = append(builders, subsystemBuilder{builder: portBuilder, name: name})
		}
	}

	return builders, nil
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

func portTag(port int) string {
	return fmt.Sprintf("nvlink_port:%d", port)
}
