// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGoAwayObserverRequiresExactOptIn(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		debug   string
		want    bool
	}{
		{name: "disabled without debug", want: true},
		{name: "enabled exact", enabled: true, debug: "http2debug=2", want: true},
		{name: "enabled combined", enabled: true, debug: "http2debug=2,netdns=go"},
		{name: "enabled wrong level", enabled: true, debug: "http2debug=1"},
		{name: "disabled ambient", debug: "http2debug=2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := goAwayObserverModeAllowed(test.enabled, test.debug); got != test.want {
				t.Fatalf("mode allowed=%t, want %t", got, test.want)
			}
		})
	}
}

func filteredEvents(t *testing.T, writes ...string) ([]map[string]any, string) {
	t.Helper()
	var outputBytes bytes.Buffer
	out := &output{writer: json.NewEncoder(&outputBytes), failed: make(chan struct{})}
	filter := newFilteredH2DebugWriter(out)
	for _, value := range writes {
		if _, err := filter.Write([]byte(value)); err != nil {
			t.Fatal("filtered logger write failed")
		}
	}
	var events []map[string]any
	decoder := json.NewDecoder(&outputBytes)
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal("decode sanitized observer event")
		}
		events = append(events, event)
	}
	return events, outputBytes.String()
}

func TestFilteredH2DebugWriterEmitsOnlySanitizedParsedGoAwayFields(t *testing.T) {
	events, encoded := filteredEvents(t,
		`http2: Framer 0xcafe: read DATA stream=1 len=24 data="synthetic-private-value"`+"\n",
		`http2: Framer 0xbeef: read HEADERS flags=END_HEADERS stream=3 len=18`+
			` fields=["private-token: synthetic-secret"]`+"\n",
		`http2: Framer 0x1234: read GOAWAY len=12 LastStreamID=3 ErrCode=NO_ERROR Debug="synthetic-debug-secret"`+"\n",
	)
	if len(events) != 1 {
		t.Fatalf("expected one filtered GOAWAY parser event, got %d", len(events))
	}
	event := events[0]
	if event["event"] != "goaway_frame_parsed" ||
		event["source"] != "go_http2debug_filtered_framer_read" ||
		event["frame_type"] != "GOAWAY" || event["stream_id"] != float64(0) ||
		event["last_stream_id"] != float64(3) || event["error_code"] != float64(0) ||
		event["framer_ordinal"] != float64(1) {
		t.Fatalf("unexpected sanitized GOAWAY fields: %#v", event)
	}
	if event["monotonic_elapsed_ns"].(float64) < 0 {
		t.Fatalf("invalid monotonic observation time: %#v", event)
	}
	for _, forbidden := range []string{"synthetic-private-value", "synthetic-secret", "synthetic-debug-secret", "0x1234", "Debug", "data=", "fields="} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("raw Go diagnostic content escaped filtering: %s", encoded)
		}
	}
}

func TestFilteredH2DebugWriterRejectsUnsupportedAndOversizedRecords(t *testing.T) {
	long := "http2: Framer 0x1234: " + strings.Repeat("private-header-value", maxH2DebugLine)
	events, encoded := filteredEvents(t,
		`http2: Framer 0x1234: read GOAWAY unusual-format synthetic-secret`+"\n",
		`http2: Framer 0x5678: read GOAWAY len=8 LastStreamID=1 ErrCode=PROTOCOL_ERROR Debug="secret"`+"\n",
		long+"\n",
	)
	if len(events) != 3 {
		t.Fatalf("expected only bounded observer failure summaries, got %d", len(events))
	}
	if events[0]["event"] != "goaway_observer_invalid" || events[0]["reason"] != "format" ||
		events[1]["event"] != "goaway_observer_invalid" || events[1]["reason"] != "fields" ||
		events[2]["event"] != "observer_log_line_overflow" {
		t.Fatalf("unexpected sanitized failure summaries: %#v", events)
	}
	for _, forbidden := range []string{"synthetic-secret", "PROTOCOL_ERROR", "secret", "private-header-value", "0x1234"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("diagnostic filter leaked rejected raw line: %s", encoded)
		}
	}
}
