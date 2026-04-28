// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package framer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/datadog-agent/pkg/logs/message"
	status "github.com/DataDog/datadog-agent/pkg/logs/status/utils"
)

// processSyslog feeds chunks through a SyslogFraming framer and collects non-empty output.
// Empty frames (from stray delimiters/unexpected bytes) are filtered, matching the
// behavior of forwardMessages which skips zero-length content.
func processSyslog(t *testing.T, limit int, chunks [][]byte) (contents []string, rawLens []int) {
	t.Helper()
	outputFn := func(msg *message.Message, rawDataLen int) {
		if len(msg.GetContent()) > 0 {
			contents = append(contents, string(msg.GetContent()))
			rawLens = append(rawLens, rawDataLen)
		}
	}
	fr := NewFramer(outputFn, SyslogFraming, limit)
	for _, c := range chunks {
		logMessage := message.NewMessage(c, nil, "", 0)
		fr.Process(logMessage)
	}
	fr.Flush()
	return
}

func TestSyslogNonTransparentFraming(t *testing.T) {
	// Simple LF-delimited syslog messages.
	msg1 := "<34>1 2024-01-01T00:00:00Z host app - - - hello"
	msg2 := "<34>1 2024-01-01T00:00:01Z host app - - - world"
	input := []byte(msg1 + "\n" + msg2 + "\n")
	wantContent := []string{msg1, msg2}
	wantLens := []int{len(msg1) + 1, len(msg2) + 1}

	t.Run("one chunk", func(t *testing.T) {
		got, lens := processSyslog(t, 4096, [][]byte{input})
		require.Equal(t, wantContent, got)
		require.Equal(t, wantLens, lens)
	})

	t.Run("one-byte chunks", func(t *testing.T) {
		chunks := make([][]byte, len(input))
		for i, b := range input {
			chunks[i] = []byte{b}
		}
		got, lens := processSyslog(t, 4096, chunks)
		require.Equal(t, wantContent, got)
		require.Equal(t, wantLens, lens)
	})
}

func TestSyslogNonTransparentNULDelimiter(t *testing.T) {
	// NUL-delimited frames (RFC 6587 §3.4.2 alternative trailer).
	msg1 := "<34>1 2024-01-01T00:00:00Z host app - - - hello"
	msg2 := "<34>1 2024-01-01T00:00:01Z host app - - - world"
	input := []byte(msg1 + "\x00" + msg2 + "\x00")

	got, lens := processSyslog(t, 4096, [][]byte{input})
	require.Equal(t, []string{msg1, msg2}, got)
	require.Equal(t, []int{len(msg1) + 1, len(msg2) + 1}, lens)
}

func TestSyslogNonTransparentCRLF(t *testing.T) {
	// CR+LF trailing should be trimmed from content.
	msg := "<34>1 2024-01-01T00:00:00Z host app - - - hello\r"
	input := []byte(msg + "\n")

	got, _ := processSyslog(t, 4096, [][]byte{input})
	require.Equal(t, []string{"<34>1 2024-01-01T00:00:00Z host app - - - hello"}, got)
}

func TestSyslogOctetCounting(t *testing.T) {
	// Two octet-counted messages back to back.
	msg1 := "<34>1 2024-01-01T00:00:00Z host app - - - hello"
	msg2 := "<34>1 2024-01-01T00:00:01Z host app - - - world"
	input := []byte(fmt.Sprintf("%d %s%d %s", len(msg1), msg1, len(msg2), msg2))

	wantContent := []string{msg1, msg2}
	header1Len := len(fmt.Sprintf("%d ", len(msg1)))
	header2Len := len(fmt.Sprintf("%d ", len(msg2)))
	wantLens := []int{header1Len + len(msg1), header2Len + len(msg2)}

	t.Run("one chunk", func(t *testing.T) {
		got, lens := processSyslog(t, 4096, [][]byte{input})
		require.Equal(t, wantContent, got)
		require.Equal(t, wantLens, lens)
	})

	t.Run("one-byte chunks", func(t *testing.T) {
		chunks := make([][]byte, len(input))
		for i, b := range input {
			chunks[i] = []byte{b}
		}
		got, lens := processSyslog(t, 4096, chunks)
		require.Equal(t, wantContent, got)
		require.Equal(t, wantLens, lens)
	})
}

func TestSyslogMixedFraming(t *testing.T) {
	// Mix of octet-counted and non-transparent frames in one stream.
	msg1 := "<34>1 host app - - - octet-counted"
	msg2 := "<34>1 host app - - - non-transparent"

	input := []byte(fmt.Sprintf("%d %s%s\n", len(msg1), msg1, msg2))

	headerLen := len(fmt.Sprintf("%d ", len(msg1)))
	got, lens := processSyslog(t, 4096, [][]byte{input})
	require.Equal(t, []string{msg1, msg2}, got)
	require.Equal(t, []int{headerLen + len(msg1), len(msg2) + 1}, lens)
}

func TestSyslogStrayDelimiters(t *testing.T) {
	// Stray newlines/NULs between frames should be consumed silently.
	msg := "<34>1 host app - - - message"
	input := []byte("\n\n\x00\r" + msg + "\n")

	got, _ := processSyslog(t, 4096, [][]byte{input})
	require.Equal(t, []string{msg}, got)
}

func TestSyslogOctetCountingPartialBuffer(t *testing.T) {
	// Octet-counted message split across two Process calls.
	msg := "<34>1 2024-01-01T00:00:00Z host app - - - hello world"
	full := []byte(fmt.Sprintf("%d %s", len(msg), msg))

	// Split in the middle of the message body.
	split := len(full) / 2
	chunk1 := full[:split]
	chunk2 := full[split:]

	got, lens := processSyslog(t, 4096, [][]byte{chunk1, chunk2})
	require.Equal(t, []string{msg}, got)
	headerLen := len(fmt.Sprintf("%d ", len(msg)))
	require.Equal(t, []int{headerLen + len(msg)}, lens)
}

func TestSyslogOctetCountingPartialHeader(t *testing.T) {
	// Octet-count header split across chunks (e.g., "4" then "9 <34>...").
	msg := "<34>1 2024-01-01T00:00:00Z host app - - - hello"
	full := []byte(fmt.Sprintf("%d %s", len(msg), msg))

	// Split within the length digits.
	chunk1 := full[:1] // just "4"
	chunk2 := full[1:] // "9 <34>..."

	got, _ := processSyslog(t, 4096, [][]byte{chunk1, chunk2})
	require.Equal(t, []string{msg}, got)
}

func TestSyslogContentLenLimitOctetCounted(t *testing.T) {
	// Octet-counted message exceeding content limit is split into
	// bounded continuation frames with zero data loss.
	limit := 20
	msg := "<34>1 " + strings.Repeat("x", 30) // 36 bytes total
	header := fmt.Sprintf("%d ", len(msg))
	full := []byte(header + msg)

	got, rawLens := processSyslog(t, limit, [][]byte{full})
	require.True(t, len(got) > 1, "oversized frame should be split into multiple frames")

	// Verify zero data loss: concatenating all emitted frames should
	// reproduce the complete original input (header + body).
	combined := strings.Join(got, "")
	assert.Equal(t, string(full), combined)

	// First frame should be exactly limit bytes of raw content.
	assert.Len(t, got[0], limit)
	assert.Equal(t, limit, rawLens[0])
}

func TestSyslogContentLenLimitNonTransparent(t *testing.T) {
	// Non-transparent message exceeding content limit is split into
	// bounded continuation frames with zero data loss.
	limit := 20
	msg := "<34>1 " + strings.Repeat("x", 30) // 36 bytes
	input := []byte(msg + "\n")

	got, rawLens := processSyslog(t, limit, [][]byte{input})
	require.True(t, len(got) > 1, "oversized frame should be split into multiple frames")

	// First frame is raw bytes from the start of the buffer.
	assert.Len(t, got[0], limit)
	assert.Equal(t, limit, rawLens[0])

	// Verify zero data loss: concatenated output reproduces the full message.
	combined := strings.Join(got, "")
	assert.Equal(t, msg, combined)
}

func TestSyslogOversizedMalformedSplit(t *testing.T) {
	// Malformed content exceeding contentLenLimit is split rather than truncated.
	// Use a limit large enough to hold the valid syslog message in one frame.
	limit := 10
	junk := strings.Repeat("Z", 25)
	validMsg := "<34>1 msg"
	input := []byte(junk + validMsg + "\n")

	tailerInfo := status.NewInfoRegistry()
	var contents []string
	var truncated []bool
	outputFn := func(msg *message.Message, _ int) {
		if len(msg.GetContent()) > 0 {
			contents = append(contents, string(msg.GetContent()))
			truncated = append(truncated, msg.ParsingExtra.IsTruncated)
		}
	}
	fr := NewSyslogFramer(outputFn, limit, tailerInfo)
	fr.Process(message.NewMessage(input, nil, "", 0))

	require.True(t, len(contents) >= 3, "expected at least 3 frames: split malformed + valid syslog, got %d: %v", len(contents), contents)

	// The last frame is the valid syslog message (fits within limit).
	assert.Equal(t, validMsg, contents[len(contents)-1])

	// Verify zero data loss for the malformed portion.
	malformedParts := contents[:len(contents)-1]
	malformedCombined := strings.Join(malformedParts, "")
	assert.Equal(t, junk, malformedCombined)

	// First chunk should be flagged as truncated.
	assert.True(t, truncated[0], "first chunk of oversized malformed frame should be truncated")

	rendered := tailerInfo.Rendered()
	oversized := rendered["Syslog Oversized Frames"]
	require.NotEmpty(t, oversized)
}

func TestSyslogOversizedFlushFrame(t *testing.T) {
	t.Run("matcher splits oversized buffer", func(t *testing.T) {
		// Test the FlushFrame method directly on the matcher.
		limit := 10
		matcher := &syslogFrameMatcher{contentLenLimit: limit}
		buf := []byte("<134>" + strings.Repeat("A", 25)) // 30 bytes

		// First call: emits limit bytes.
		content, rawDataLen := matcher.FlushFrame(buf)
		require.NotNil(t, content)
		assert.Len(t, content, limit)
		assert.Equal(t, limit, rawDataLen)

		// Second call with remainder.
		buf = buf[rawDataLen:]
		content, rawDataLen = matcher.FlushFrame(buf)
		require.NotNil(t, content)
		assert.Len(t, content, limit)
		assert.Equal(t, limit, rawDataLen)

		// Third call with final remainder.
		buf = buf[rawDataLen:]
		content, rawDataLen = matcher.FlushFrame(buf)
		require.NotNil(t, content)
		assert.Len(t, content, 10)
		assert.Equal(t, 10, rawDataLen)
	})

	t.Run("Flush loop emits all bytes at EOF", func(t *testing.T) {
		// Use a small enough message that Process() buffers it (under limit),
		// then verify Flush emits it.
		limit := 20
		msg := "<134>hello world" // 16 bytes, under limit

		var contents []string
		var truncated []bool
		outputFn := func(msg *message.Message, _ int) {
			if len(msg.GetContent()) > 0 {
				contents = append(contents, string(msg.GetContent()))
				truncated = append(truncated, msg.ParsingExtra.IsTruncated)
			}
		}
		tailerInfo := status.NewInfoRegistry()
		fr := NewSyslogFramer(outputFn, limit, tailerInfo)
		fr.Process(message.NewMessage([]byte(msg), nil, "", 0))
		require.Empty(t, contents, "no delimiter, nothing emitted yet")

		fr.Flush()
		require.Len(t, contents, 1)
		assert.Equal(t, msg, contents[0])
		assert.False(t, truncated[0], "single flush frame should not be truncated")
	})
}

func TestSyslogFlushEmitsAllBytes(t *testing.T) {
	// Flush emits all remaining bytes at EOF, including partial
	// octet-counted frames and non-'<' prefixed content.
	limit := 4096

	t.Run("partial octet-counted frame is emitted at EOF", func(t *testing.T) {
		var contents []string
		outputFn := func(msg *message.Message, _ int) {
			if len(msg.GetContent()) > 0 {
				contents = append(contents, string(msg.GetContent()))
			}
		}
		tailerInfo := status.NewInfoRegistry()
		fr := NewSyslogFramer(outputFn, limit, tailerInfo)

		fr.Process(message.NewMessage([]byte("200 <134>partial"), nil, "", 0))
		require.Empty(t, contents)

		fr.Flush()
		require.Len(t, contents, 1, "partial octet-counted frame should now be emitted at EOF")
		assert.Equal(t, "200 <134>partial", contents[0])
	})

	t.Run("non-syslog content is emitted at EOF", func(t *testing.T) {
		var contents []string
		outputFn := func(msg *message.Message, _ int) {
			if len(msg.GetContent()) > 0 {
				contents = append(contents, string(msg.GetContent()))
			}
		}
		tailerInfo := status.NewInfoRegistry()
		fr := NewSyslogFramer(outputFn, limit, tailerInfo)

		fr.Process(message.NewMessage([]byte("just plain text"), nil, "", 0))
		require.Empty(t, contents)

		fr.Flush()
		require.Len(t, contents, 1)
		assert.Equal(t, "just plain text", contents[0])
	})
}

func TestSyslogOversizedZeroDataLoss(t *testing.T) {
	// End-to-end verification that every byte of an oversized syslog stream
	// appears in the output, regardless of framing method.
	limit := 15

	t.Run("octet-counted", func(t *testing.T) {
		body := "<34>1 " + strings.Repeat("B", 40)
		frame := fmt.Sprintf("%d %s", len(body), body)
		// Follow with a delimited message so the framer can sync.
		nextBody := "<34>1 next"
		nextMsg := nextBody + "\n"
		input := []byte(frame + nextMsg)

		got, _ := processSyslog(t, limit, [][]byte{input})
		require.True(t, len(got) >= 2, "expected split frames plus the next message")

		// Concatenating ALL output should reproduce the full input
		// (minus the trailing newline delimiter).
		combined := strings.Join(got, "")
		assert.Equal(t, frame+nextBody, combined)
	})

	t.Run("non-transparent", func(t *testing.T) {
		body := "<34>1 " + strings.Repeat("C", 40) // 46 bytes
		input := []byte(body + "\n")

		got, _ := processSyslog(t, limit, [][]byte{input})
		combined := strings.Join(got, "")
		assert.Equal(t, body, combined)
	})
}

func TestSyslogFramingIntegrationWithFramer(t *testing.T) {
	// End-to-end test through the full Framer.Process path with various chunk sizes.
	msg1 := "<34>1 host app - - - msg1"
	msg2 := "<34>1 host app - - - msg2"
	msg3 := "<34>1 host app - - - msg3"

	// msg1: octet-counted, msg2: non-transparent LF, msg3: octet-counted
	input := []byte(fmt.Sprintf("%d %s%s\n%d %s", len(msg1), msg1, msg2, len(msg3), msg3))
	wantContent := []string{msg1, msg2, msg3}

	for _, chunkSize := range []int{1, 2, 5, 10, len(input)} {
		t.Run(fmt.Sprintf("chunk_%d", chunkSize), func(t *testing.T) {
			var chunks [][]byte
			for i := 0; i < len(input); i += chunkSize {
				end := i + chunkSize
				if end > len(input) {
					end = len(input)
				}
				chunks = append(chunks, input[i:end])
			}
			got, _ := processSyslog(t, 4096, chunks)
			require.Equal(t, wantContent, got)
		})
	}
}

func TestSyslogFrameMatcherUnexpectedByte(t *testing.T) {
	// Unexpected leading bytes before a valid frame are emitted as a single
	// malformed frame, then the real syslog message is parsed normally.
	input := []byte("X<34>1 host app - - - msg\n")

	got, _ := processSyslog(t, 4096, [][]byte{input})
	require.Equal(t, []string{"X", "<34>1 host app - - - msg"}, got)
}

func TestSyslogMalformedFrameEmission(t *testing.T) {
	t.Run("pure junk emitted as single malformed frame", func(t *testing.T) {
		tailerInfo := status.NewInfoRegistry()
		var contents []string
		outputFn := func(msg *message.Message, _ int) {
			if len(msg.GetContent()) > 0 {
				contents = append(contents, string(msg.GetContent()))
			}
		}
		fr := NewSyslogFramer(outputFn, 4096, tailerInfo)

		// "hello world" has no valid frame start and no delimiter, so the
		// matcher waits for more data. Flush emits nothing (no '<' prefix).
		// Adding a trailing newline lets the matcher emit it as one frame.
		fr.Process(message.NewMessage([]byte("hello world\n"), nil, "", 0))

		require.Equal(t, []string{"hello world"}, contents)

		rendered := tailerInfo.Rendered()
		discarded := rendered["Syslog Discarded Bytes"]
		require.NotEmpty(t, discarded)
		assert.Equal(t, "11", discarded[0])
	})

	t.Run("junk followed by valid frame resyncs at PRI", func(t *testing.T) {
		tailerInfo := status.NewInfoRegistry()
		var contents []string
		outputFn := func(msg *message.Message, _ int) {
			if len(msg.GetContent()) > 0 {
				contents = append(contents, string(msg.GetContent()))
			}
		}
		fr := NewSyslogFramer(outputFn, 4096, tailerInfo)

		validMsg := "<34>1 host app - - - real message"
		input := []byte("JUNK" + validMsg + "\n")
		fr.Process(message.NewMessage(input, nil, "", 0))

		require.Equal(t, []string{"JUNK", validMsg}, contents)

		rendered := tailerInfo.Rendered()
		discarded := rendered["Syslog Discarded Bytes"]
		require.NotEmpty(t, discarded)
		assert.Equal(t, "4", discarded[0])
	})

	t.Run("stray delimiters are not counted as malformed", func(t *testing.T) {
		tailerInfo := status.NewInfoRegistry()
		var contents []string
		outputFn := func(msg *message.Message, _ int) {
			if len(msg.GetContent()) > 0 {
				contents = append(contents, string(msg.GetContent()))
			}
		}
		fr := NewSyslogFramer(outputFn, 4096, tailerInfo)

		msg := "<34>1 host app - - - msg"
		input := []byte("\n\r\x00" + msg + "\n")
		fr.Process(message.NewMessage(input, nil, "", 0))

		require.Equal(t, []string{msg}, contents)

		rendered := tailerInfo.Rendered()
		discarded := rendered["Syslog Discarded Bytes"]
		require.NotEmpty(t, discarded)
		assert.Equal(t, "0", discarded[0])
	})

	t.Run("junk before octet-counted frame resyncs at digit", func(t *testing.T) {
		tailerInfo := status.NewInfoRegistry()
		var contents []string
		outputFn := func(msg *message.Message, _ int) {
			if len(msg.GetContent()) > 0 {
				contents = append(contents, string(msg.GetContent()))
			}
		}
		fr := NewSyslogFramer(outputFn, 4096, tailerInfo)

		syslogMsg := "<34>1 host app - - - octet msg"
		octetFrame := fmt.Sprintf("%d %s", len(syslogMsg), syslogMsg)
		input := []byte("XX" + octetFrame)
		fr.Process(message.NewMessage(input, nil, "", 0))

		require.Equal(t, []string{"XX", syslogMsg}, contents)

		rendered := tailerInfo.Rendered()
		discarded := rendered["Syslog Discarded Bytes"]
		require.NotEmpty(t, discarded)
		assert.Equal(t, "2", discarded[0])
	})
}

func TestSyslogTrimTrailer(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{"empty", []byte{}, []byte{}},
		{"no trailer", []byte("hello"), []byte("hello")},
		{"LF", []byte("hello\n"), []byte("hello")},
		{"NUL", []byte("hello\x00"), []byte("hello")},
		{"CRLF", []byte("hello\r\n"), []byte("hello")},
		{"CR only", []byte("hello\r"), []byte("hello")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := syslogTrimTrailer(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}
