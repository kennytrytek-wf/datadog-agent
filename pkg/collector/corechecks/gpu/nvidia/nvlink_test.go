// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux && nvml

package nvidia

import (
	"encoding/binary"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/datadog-agent/pkg/gpu/testutil"
)

func TestGetNvlinkBuildersAndCollectorCollect(t *testing.T) {
	mockDevice := setupMockDeviceWithLibOpts(t, func(device *mock.Device) *mock.Device {
		testutil.WithMockAllDeviceFunctions()(device)
		device.GetFieldValuesFunc = func(values []nvml.FieldValue) nvml.Return {
			require.Len(t, values, 1)
			values[0].ValueType = uint32(nvml.VALUE_TYPE_UNSIGNED_INT)
			values[0].Value = [8]byte{2, 0, 0, 0, 0, 0, 0, 0}
			return nvml.SUCCESS
		}
		device.ReadWritePRM_v1Func = func(buffer *nvml.PRMTLV_v1) nvml.Return {
			port := int(binary.BigEndian.Uint32(buffer.InData[20:24]) >> 16)
			response := makePLRResponseBytes(uint64(port * 100))
			copy(buffer.InData[:], response)
			return nvml.SUCCESS
		}
		return device
	})

	builders, err := getNvlinkBuilders(mockDevice)
	require.NoError(t, err)
	require.Len(t, builders, 2)

	var metrics []Metric
	for _, builder := range builders {
		collector, err := builder.builder(mockDevice, nil)
		require.NoError(t, err)

		collected, err := collector.Collect()
		require.NoError(t, err)
		metrics = append(metrics, collected...)
	}

	require.Len(t, metrics, len(plrCounterFields)*2)

	port1Count := 0
	port2Count := 0
	for _, metric := range metrics {
		switch {
		case hasTag(metric.Tags, "nvlink_port:1"):
			port1Count++
		case hasTag(metric.Tags, "nvlink_port:2"):
			port2Count++
		default:
			t.Fatalf("missing nvlink_port tag on metric %+v", metric)
		}
	}
	require.Equal(t, len(plrCounterFields), port1Count)
	require.Equal(t, len(plrCounterFields), port2Count)
}

func TestGetNvlinkBuildersPartialFailure(t *testing.T) {
	mockDevice := setupMockDeviceWithLibOpts(t, func(device *mock.Device) *mock.Device {
		testutil.WithMockAllDeviceFunctions()(device)
		device.GetFieldValuesFunc = func(values []nvml.FieldValue) nvml.Return {
			require.Len(t, values, 1)
			values[0].ValueType = uint32(nvml.VALUE_TYPE_UNSIGNED_INT)
			values[0].Value = [8]byte{2, 0, 0, 0, 0, 0, 0, 0}
			return nvml.SUCCESS
		}
		device.ReadWritePRM_v1Func = func(buffer *nvml.PRMTLV_v1) nvml.Return {
			port := int(binary.BigEndian.Uint32(buffer.InData[20:24]) >> 16)
			if port == 2 {
				return nvml.ERROR_NOT_SUPPORTED
			}
			response := makePLRResponseBytes(100)
			copy(buffer.InData[:], response)
			return nvml.SUCCESS
		}
		return device
	})

	builders, err := getNvlinkBuilders(mockDevice)
	require.NoError(t, err)
	require.Len(t, builders, 2)

	var metrics []Metric
	for _, builder := range builders {
		collector, err := builder.builder(mockDevice, nil)
		require.NoError(t, err)

		collected, err := collector.Collect()
		if err != nil {
			continue
		}

		metrics = append(metrics, collected...)
	}

	require.Len(t, metrics, len(plrCounterFields))
	for _, metric := range metrics {
		require.Contains(t, metric.Tags, "nvlink_port:1")
	}
}

func TestGetNvlinkBuildersUnsupportedDevice(t *testing.T) {
	tests := []struct {
		name      string
		customize func(*mock.Device) *mock.Device
		assertErr func(*testing.T, error)
	}{
		{
			name: "field API unsupported",
			customize: func(device *mock.Device) *mock.Device {
				testutil.WithMockAllDeviceFunctions()(device)
				device.GetFieldValuesFunc = func(_ []nvml.FieldValue) nvml.Return {
					return nvml.ERROR_NOT_SUPPORTED
				}
				return device
			},
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				require.NotErrorIs(t, err, errUnsupportedDevice)
			},
		},
		{
			name: "no nvlink ports",
			customize: func(device *mock.Device) *mock.Device {
				testutil.WithMockAllDeviceFunctions()(device)
				device.GetFieldValuesFunc = func(values []nvml.FieldValue) nvml.Return {
					require.Len(t, values, 1)
					values[0].ValueType = uint32(nvml.VALUE_TYPE_UNSIGNED_INT)
					values[0].Value = [8]byte{}
					return nvml.SUCCESS
				}
				return device
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorIs(t, err, errUnsupportedDevice)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDevice := setupMockDeviceWithLibOpts(t, tt.customize)
			_, err := getNvlinkBuilders(mockDevice)
			tt.assertErr(t, err)
		})
	}
}

func hasTag(tags []string, expected string) bool {
	for _, tag := range tags {
		if tag == expected {
			return true
		}
	}
	return false
}
