// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package syslog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/datadog-agent/comp/logs/agent/config"
	"github.com/DataDog/datadog-agent/pkg/logs/message"
	"github.com/DataDog/datadog-agent/pkg/logs/sources"
)

func newTestMessage(content []byte) *message.Message {
	source := sources.NewLogSource("test", &config.LogsConfig{})
	origin := message.NewOrigin(source)
	msg := message.NewMessage(content, origin, "", time.Now().UnixNano())
	msg.RawDataLen = len(content)
	return msg
}

func TestSyslogParser_NetworkFormat_RFC5424(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(`<165>1 2003-10-11T22:14:15.003Z mymachine evntslog - ID47 [exampleSDID@32473 iut="3"] An application event log entry`))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	// StateStructured
	assert.Equal(t, message.StateStructured, result.State)

	// Content is the MSG body
	assert.Equal(t, "An application event log entry", string(result.GetContent()))

	// Status from severity
	assert.Equal(t, message.StatusNotice, result.Status)

	// Appname stored as source/service override in ParsingExtra
	assert.Equal(t, "evntslog", result.ParsingExtra.SourceOverride)
	assert.Equal(t, "evntslog", result.ParsingExtra.ServiceOverride)

	// RawDataLen preserved
	assert.Equal(t, len(input.GetContent()), result.RawDataLen)

	// Timestamp extracted
	assert.Equal(t, "2003-10-11T22:14:15.003Z", result.ParsingExtra.Timestamp)
}

func TestSyslogParser_NetworkFormat_BSD(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(`<34>Oct 11 22:14:15 mymachine su: 'su root' failed for lonvick on /dev/pts/8`))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	assert.Equal(t, message.StateStructured, result.State)
	assert.Equal(t, message.StatusCritical, result.Status)

	// Appname stored as source/service override in ParsingExtra
	assert.Equal(t, "su", result.ParsingExtra.SourceOverride)
	assert.Equal(t, "su", result.ParsingExtra.ServiceOverride)
}

func TestSyslogParser_BSDLineFormat(t *testing.T) {
	parser := NewParser(true)

	// BSD line without PRI: "Oct 11 22:14:15 mymachine su: message"
	input := newTestMessage([]byte(`Oct 11 22:14:15 mymachine su: 'su root' failed for lonvick on /dev/pts/8`))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	assert.Equal(t, message.StateStructured, result.State)

	// No PRI -> StatusInfo
	assert.Equal(t, message.StatusInfo, result.Status)

	// Appname stored as source/service override in ParsingExtra
	assert.Equal(t, "su", result.ParsingExtra.SourceOverride)
	assert.Equal(t, "su", result.ParsingExtra.ServiceOverride)
}

func TestSyslogParser_AppNameNILVALUE(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(`<14>1 2003-10-11T22:14:15.003Z mymachine - - - - test message`))
	result, err := parser.Parse(input)
	require.NoError(t, err)
	assert.Equal(t, message.StateStructured, result.State)

	// NILVALUE appname should not set override
	assert.Equal(t, "", result.ParsingExtra.SourceOverride)
	assert.Equal(t, "", result.ParsingExtra.ServiceOverride)
}

func TestSyslogParser_Malformed(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(`<14>`))
	result, err := parser.Parse(input)

	// On parse failure, the original message is returned unmodified.
	assert.Error(t, err)
	assert.Same(t, input, result)
	assert.Equal(t, "<14>", string(result.GetContent()))
}

func TestSyslogParser_SupportsPartialLine(t *testing.T) {
	parser := NewParser(true)
	assert.False(t, parser.SupportsPartialLine())
}

func TestSyslogParser_AutoDetect_MixedFormats(t *testing.T) {
	// A single parser instance should auto-detect PRI vs non-PRI per line.
	parser := NewParser(true)

	// Line with PRI (network format)
	priInput := newTestMessage([]byte(`<14>1 2003-10-11T22:14:15.003Z myhost myapp - - - PRI message`))
	priResult, err := parser.Parse(priInput)
	require.NoError(t, err)
	assert.Equal(t, message.StateStructured, priResult.State)
	assert.Equal(t, "PRI message", string(priResult.GetContent()))
	assert.Equal(t, message.StatusInfo, priResult.Status) // 14%8=6 -> info

	// Line without PRI (BSD on-disk format)
	bsdInput := newTestMessage([]byte(`Oct 11 22:14:15 myhost su: BSD message`))
	bsdResult, err := parser.Parse(bsdInput)
	require.NoError(t, err)
	assert.Equal(t, message.StateStructured, bsdResult.State)
	assert.Equal(t, "BSD message", string(bsdResult.GetContent()))
	assert.Equal(t, message.StatusInfo, bsdResult.Status) // no PRI -> -1 -> info

	// Another PRI line to confirm no state lock-in
	pri2Input := newTestMessage([]byte(`<11>1 2003-10-11T22:14:16.003Z myhost otherapp - - - Second PRI`))
	pri2Result, err := parser.Parse(pri2Input)
	require.NoError(t, err)
	assert.Equal(t, "Second PRI", string(pri2Result.GetContent()))
	assert.Equal(t, message.StatusError, pri2Result.Status) // 11%8=3 -> error
}

func TestSyslogParser_NonSyslogText(t *testing.T) {
	parser := NewParser(true)

	lines := []string{
		"this is not syslog at all",
		"ERROR 2025-01-15 connection refused",
		"12345 plain numeric line",
		`{"json":"log","level":"warn"}`,
	}

	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			input := newTestMessage([]byte(line))
			result, err := parser.Parse(input)

			// On parse failure, the original message is returned unmodified.
			assert.Error(t, err)
			assert.Same(t, input, result)
			assert.Equal(t, line, string(result.GetContent()))
		})
	}
}

func TestSyslogParser_RenderedContent_RFC5424(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(`<165>1 2003-10-11T22:14:15.003Z mymachine evntslog - ID47 [exampleSDID@32473 iut="3"] An application event log entry`))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	// Render should produce valid JSON with message and syslog fields
	rendered, err := result.Render()
	require.NoError(t, err)
	assert.Contains(t, string(rendered), `"message"`)
	assert.Contains(t, string(rendered), `"syslog"`)
	assert.Contains(t, string(rendered), `"An application event log entry"`)
}

func TestSyslogParser_IngestionTimestampPreserved(t *testing.T) {
	parser := NewParser(true)
	ts := int64(1234567890)

	source := sources.NewLogSource("test", &config.LogsConfig{})
	origin := message.NewOrigin(source)
	msg := message.NewMessage([]byte(`<14>1 2003-10-11T22:14:15.003Z myhost myapp - - - test`), origin, "", ts)

	result, err := parser.Parse(msg)
	require.NoError(t, err)
	assert.Equal(t, ts, result.IngestionTimestamp)
}

// ---------------------------------------------------------------------------
// CEF/LEEF integration tests (full syslog envelope + CEF/LEEF body)
// ---------------------------------------------------------------------------

func TestSyslogParser_CEF_RFC5424(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(
		`<14>1 2026-03-30T12:00:00Z firewall01 CEF - - - CEF:0|Security|threatmanager|1.0|100|worm successfully stopped|10|src=10.0.0.1 dst=2.1.2.2 spt=1232`,
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	assert.Equal(t, message.StateStructured, result.State)

	// CEF/LEEF messages have an empty message body; all data is in "siem"
	assert.Equal(t, "", string(result.GetContent()))

	// Render and verify JSON structure
	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	// Syslog envelope fields should be present
	syslog, ok := data["syslog"].(map[string]interface{})
	require.True(t, ok, "syslog key missing")
	assert.Equal(t, "firewall01", syslog["hostname"])
	assert.Equal(t, "CEF", syslog["appname"])

	// SIEM fields should be present
	siem, ok := data["siem"].(map[string]interface{})
	require.True(t, ok, "siem key missing from rendered output")
	assert.Equal(t, "CEF", siem["format"])
	assert.Equal(t, "0", siem["version"])
	assert.Equal(t, "Security", siem["device_vendor"])
	assert.Equal(t, "threatmanager", siem["device_product"])
	assert.Equal(t, "1.0", siem["device_version"])
	assert.Equal(t, "100", siem["event_id"])
	assert.Equal(t, "worm successfully stopped", siem["name"])
	assert.Equal(t, "10", siem["severity"])

	ext, ok := siem["extension"].(map[string]interface{})
	require.True(t, ok, "extension missing")
	assert.Equal(t, "10.0.0.1", ext["src"])
	assert.Equal(t, "2.1.2.2", ext["dst"])
	assert.Equal(t, "1232", ext["spt"])
}

func TestSyslogParser_CEF_BSD(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(
		`<34>Oct 11 22:14:15 myhost CEF: CEF:0|Vendor|Product|1.0|200|Attack|8|src=1.2.3.4`,
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	assert.Equal(t, "", string(result.GetContent()))

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	siem, ok := data["siem"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "CEF", siem["format"])
	assert.Equal(t, "Vendor", siem["device_vendor"])
}

func TestSyslogParser_LEEF10_RFC5424(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(
		"<13>1 2026-03-30T12:00:00Z server01 LEEF - - - LEEF:1.0|Microsoft|MSExchange|2013 SP1|15345|src=10.0.1.7\tdst=10.0.0.5\tsev=5",
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	assert.Equal(t, message.StateStructured, result.State)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	siem, ok := data["siem"].(map[string]interface{})
	require.True(t, ok, "siem key missing from rendered output")
	assert.Equal(t, "LEEF", siem["format"])
	assert.Equal(t, "1.0", siem["version"])
	assert.Equal(t, "Microsoft", siem["device_vendor"])
	assert.Equal(t, "MSExchange", siem["device_product"])
	assert.Equal(t, "2013 SP1", siem["device_version"])
	assert.Equal(t, "15345", siem["event_id"])

	_, hasName := siem["name"]
	assert.False(t, hasName)
	_, hasSev := siem["severity"]
	assert.False(t, hasSev)

	ext, ok := siem["extension"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "10.0.1.7", ext["src"])
	assert.Equal(t, "10.0.0.5", ext["dst"])
	assert.Equal(t, "5", ext["sev"])
}

func TestSyslogParser_LEEF20_CustomDelimiter(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(
		`<113>1 2026-03-30T12:00:00Z hostname4 LEEF - - - LEEF:2.0|Lancope|StealthWatch|1.0|41|^|src=10.0.1.8^dst=10.0.0.5^sev=5`,
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	siem, ok := data["siem"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "LEEF", siem["format"])
	assert.Equal(t, "2.0", siem["version"])
	assert.Equal(t, "41", siem["event_id"])

	ext, ok := siem["extension"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "10.0.1.8", ext["src"])
	assert.Equal(t, "10.0.0.5", ext["dst"])
}

func TestSyslogParser_NonCEFLEEF_Unchanged(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(
		`<165>1 2003-10-11T22:14:15.003Z mymachine evntslog - ID47 - Just a regular message`,
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	_, hasSiem := data["siem"]
	assert.False(t, hasSiem, "siem key should not be present for non-CEF/LEEF messages")
	assert.Equal(t, "Just a regular message", data["message"])
}

func TestSyslogParser_CEF_EscapedPipesInHeader(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(
		`<14>1 2026-03-30T12:00:00Z host app - - - CEF:0|Vendor\|Inc|Product|1.0|100|Name|5|src=1.2.3.4`,
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	siem := data["siem"].(map[string]interface{})
	assert.Equal(t, "Vendor|Inc", siem["device_vendor"])
}

func TestSyslogParser_CEF_ExtensionWithSpacesInValue(t *testing.T) {
	parser := NewParser(true)

	input := newTestMessage([]byte(
		`<14>1 2026-03-30T12:00:00Z host app - - - CEF:0|V|P|1.0|100|N|5|filePath=/user/dir/my file.txt dst=10.0.0.1`,
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	ext := data["siem"].(map[string]interface{})["extension"].(map[string]interface{})
	assert.Equal(t, "/user/dir/my file.txt", ext["filePath"])
	assert.Equal(t, "10.0.0.1", ext["dst"])
}

func TestSyslogParser_SIEMParsingDisabled(t *testing.T) {
	parser := NewParser(false)

	input := newTestMessage([]byte(
		`<14>1 2026-03-30T12:00:00Z host app - - - CEF:0|Security|FW|1.0|100|Blocked|5|src=10.0.0.1 dst=10.0.0.2`,
	))
	result, err := parser.Parse(input)
	require.NoError(t, err)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	// With SIEM parsing disabled, the message body should be preserved as-is
	// and no "siem" key should be present.
	assert.Contains(t, data["message"], "CEF:0|Security|FW|")
	assert.NotContains(t, data, "siem")
}

// ---------------------------------------------------------------------------
// RFC 3164 §4.3.2: Valid PRI, no valid TIMESTAMP (e.g. Python SysLogHandler)
// ---------------------------------------------------------------------------

func TestSyslogParser_NoTimestamp(t *testing.T) {
	parser := NewParser(true)

	// Python's SysLogHandler produces <PRI>message with no TIMESTAMP/HOSTNAME.
	input := newTestMessage([]byte("<132>Python SysLogHandler TCP test"))
	result, err := parser.Parse(input)

	// Must parse successfully (not a raw pass-through)
	require.NoError(t, err)
	assert.NotSame(t, input, result, "should return a new structured message, not the original")
	assert.Equal(t, message.StateStructured, result.State)

	// Status derived from PRI: 132 % 8 = 4 = warning
	assert.Equal(t, message.StatusWarning, result.Status)

	// Message body is the content after PRI
	assert.Equal(t, "Python SysLogHandler TCP test", string(result.GetContent()))

	// No source/service override (AppName is nilvalue per §4.3.2)
	assert.Equal(t, "", result.ParsingExtra.SourceOverride)
	assert.Equal(t, "", result.ParsingExtra.ServiceOverride)

	// Timestamp is nilvalue (receiver supplies its own)
	assert.Equal(t, "-", result.ParsingExtra.Timestamp)

	// Render produces valid JSON with syslog metadata
	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	assert.Equal(t, "Python SysLogHandler TCP test", data["message"])

	syslog, ok := data["syslog"].(map[string]interface{})
	require.True(t, ok, "syslog key missing")
	assert.Equal(t, float64(16), syslog["facility"]) // 132 / 8
	assert.Equal(t, float64(4), syslog["severity"])  // 132 % 8
	assert.Equal(t, "-", syslog["hostname"])
	assert.Equal(t, "-", syslog["appname"])
}

func TestSyslogParser_NoTimestamp_LeadingSpace(t *testing.T) {
	parser := NewParser(true)

	// Content starting with non-letter, non-digit after PRI.
	input := newTestMessage([]byte("<134> message with leading space"))
	result, err := parser.Parse(input)

	require.NoError(t, err)
	assert.Equal(t, message.StateStructured, result.State)
	assert.Equal(t, " message with leading space", string(result.GetContent()))
	assert.Equal(t, message.StatusInfo, result.Status) // 134 % 8 = 6 = info
}

// ---------------------------------------------------------------------------
// Hostname enrichment from source_host tag (RFC 3164 §4.3.2 + receiver context)
// ---------------------------------------------------------------------------

func TestSyslogParser_NoTimestamp_HostnameFromSourceHost(t *testing.T) {
	parser := NewParser(true)

	// Simulates Python SysLogHandler over TCP: <PRI>message, no timestamp/hostname.
	// The StreamTailer adds "source_host:<ip>" to ParsingExtra.Tags.
	input := newTestMessage([]byte("<132>Python SysLogHandler TCP test"))
	input.ParsingExtra.Tags = []string{"source_host:10.0.0.42"}

	result, err := parser.Parse(input)
	require.NoError(t, err)
	assert.Equal(t, message.StateStructured, result.State)
	assert.Equal(t, "Python SysLogHandler TCP test", string(result.GetContent()))

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	syslog, ok := data["syslog"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "10.0.0.42", syslog["hostname"],
		"hostname should be enriched from source_host tag")
}

func TestSyslogParser_NoTimestamp_NoSourceHost_StaysNilvalue(t *testing.T) {
	parser := NewParser(true)

	// No source_host tag present — hostname stays nilvalue.
	input := newTestMessage([]byte("<132>Python SysLogHandler TCP test"))

	result, err := parser.Parse(input)
	require.NoError(t, err)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	syslog, ok := data["syslog"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "-", syslog["hostname"],
		"without source_host tag, hostname should remain nilvalue")
}

func TestSyslogParser_WithTimestamp_IgnoresSourceHost(t *testing.T) {
	parser := NewParser(true)

	// Full BSD message with hostname already present — source_host should NOT override.
	input := newTestMessage([]byte("<34>Oct 11 22:14:15 mymachine su: test"))
	input.ParsingExtra.Tags = []string{"source_host:10.0.0.42"}

	result, err := parser.Parse(input)
	require.NoError(t, err)

	rendered, err := result.Render()
	require.NoError(t, err)

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &data))

	syslog, ok := data["syslog"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "mymachine", syslog["hostname"],
		"when hostname is present in the message, source_host should not override it")
}

func TestExtractSourceHost(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want string
	}{
		{"present", []string{"env:prod", "source_host:10.0.0.1"}, "10.0.0.1"},
		{"missing", []string{"env:prod", "team:logs"}, ""},
		{"empty tags", nil, ""},
		{"ipv6", []string{"source_host:::1"}, "::1"},
		{"first match", []string{"source_host:10.0.0.1", "source_host:10.0.0.2"}, "10.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, extractSourceHost(tc.tags))
		})
	}
}

func TestSyslogParser_MalformedSyslogDoesNotExtractSIEM(t *testing.T) {
	parser := NewParser(true)
	// Malformed PRI (non-digit) followed by valid CEF — syslog parse fails,
	// so the original message is returned unmodified. No SIEM extraction,
	// no structured wrapping.
	input := newTestMessage([]byte(`<abc>CEF:0|V|P|1.0|100|N|5|src=1.2.3.4`))
	result, err := parser.Parse(input)
	require.Error(t, err)
	assert.Same(t, input, result)
	assert.Equal(t, `<abc>CEF:0|V|P|1.0|100|N|5|src=1.2.3.4`, string(result.GetContent()))
}
