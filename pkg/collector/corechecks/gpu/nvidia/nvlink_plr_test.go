// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build linux && nvml

package nvidia

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	gpuspec "github.com/DataDog/datadog-agent/pkg/collector/corechecks/gpu/spec"
)

func TestCreatePPCNTTLVByteArray(t *testing.T) {
	packet := createPPCNTTLVByteArray(ppcntGroupPLR, 3)
	require.Len(t, packet, (opTLVLenDwords+regTLVHeaderLenDwords+endTLVLenDwords)*dwordSizeBytes+ppcntSizeBytes)

	require.Equal(t, makeTLVHeader(tlvTypeOp, opTLVLenDwords), binary.BigEndian.Uint32(packet[0:4]))
	require.Equal(t, makeOpMethodAndReg(ppcntRegID), binary.BigEndian.Uint32(packet[4:8]))
	require.Equal(t, uint32(0), binary.BigEndian.Uint32(packet[8:12]))
	require.Equal(t, uint32(0), binary.BigEndian.Uint32(packet[12:16]))

	require.Equal(t, makeTLVHeader(tlvTypeReg, uint32(ppcntSizeBytes/dwordSizeBytes+regTLVHeaderLenDwords)), binary.BigEndian.Uint32(packet[16:20]))
	require.Equal(t, uint32((ppcntGroupPLR&0x3F)|(3<<16)), binary.BigEndian.Uint32(packet[20:24]))
	require.Equal(t, makeTLVHeader(tlvTypeEnd, endTLVLenDwords), binary.BigEndian.Uint32(packet[len(packet)-4:]))
}

func TestUnpackTLV(t *testing.T) {
	expected := make(map[string]uint64, len(plrCounterFields))
	payload := make([]byte, ppcntSizeBytes)
	binary.BigEndian.PutUint32(payload[0:4], ppcntGroupPLR)

	offset := 8
	for i, field := range plrCounterFields {
		value := uint64(i+1)<<32 | uint64(100+i)
		expected[field] = value
		binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(value>>32))
		offset += 4
		binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(value))
		offset += 4
	}

	packet := packTLV(ppcntRegID, ppcntSizeBytes, payload)
	metrics, err := unpackTLV(packet)
	require.NoError(t, err)
	require.Equal(t, expected, metrics)
}

func makePLRResponseBytes(seed uint64) []byte {
	payload := make([]byte, ppcntSizeBytes)
	binary.BigEndian.PutUint32(payload[0:4], ppcntGroupPLR)
	offset := 8
	for i := range plrCounterFields {
		value := seed + uint64(i)
		binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(value>>32))
		offset += 4
		binary.BigEndian.PutUint32(payload[offset:offset+4], uint32(value))
		offset += 4
	}
	return packTLV(ppcntRegID, ppcntSizeBytes, payload)
}

func TestPLRMetricSpecEntries(t *testing.T) {
	spec, err := gpuspec.LoadMetricsSpec()
	require.NoError(t, err)

	for _, metricName := range plrCounterFields {
		t.Run(metricName, func(t *testing.T) {
			metricSpec, ok := spec.Metrics[metricName]
			require.True(t, ok, "metric %s missing from spec", metricName)
			require.Contains(t, metricSpec.CustomTags, "nvlink_port")
			require.True(t, metricSpec.SupportsDeviceMode(gpuspec.DeviceModePhysical))
			require.False(t, metricSpec.SupportsDeviceMode(gpuspec.DeviceModeMIG))
			require.False(t, metricSpec.SupportsDeviceMode(gpuspec.DeviceModeVGPU))
		})
	}
}
