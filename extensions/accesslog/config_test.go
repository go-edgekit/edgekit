package accesslog_test

import (
	"errors"
	"testing"

	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

// mapSink discards records to test the map form of a Sink.
type mapSink map[string]accesslog.Record

// Report discards the record.
func (mapSink) Report(accesslog.Record) {}

// sliceSink discards records to test the slice form of a Sink.
type sliceSink []accesslog.Record

// Report discards the record.
func (sliceSink) Report(accesslog.Record) {}

// channelSink discards records to test the channel form of a Sink.
type channelSink chan accesslog.Record

// Report discards the record.
func (channelSink) Report(accesslog.Record) {}

// scalarSink discards records to test a Sink that cannot be nil.
type scalarSink int

// Report discards the record.
func (scalarSink) Report(accesslog.Record) {}

func TestNewRejectsNilSinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		sink accesslog.Sink
	}{
		{
			name: "interface",
			sink: nil,
		},
		{
			name: "function",
			sink: accesslog.SinkFunc(nil),
		},
		{
			name: "pointer",
			sink: (*collector)(nil),
		},
		{
			name: "map",
			sink: mapSink(nil),
		},
		{
			name: "slice",
			sink: sliceSink(nil),
		},
		{
			name: "channel",
			sink: channelSink(nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, err := accesslog.New(accesslog.Config{
				Sink: tc.sink,
			})
			if !errors.Is(err, accesslog.ErrNoSink) {
				t.Errorf("New() error = %v, want ErrNoSink", err)
			}

			if recorder != nil {
				t.Errorf("New() = %v, want nil", recorder)
			}
		})
	}
}

func TestNewAcceptsNonNilSinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		sink accesslog.Sink
	}{
		{
			name: "function",
			sink: accesslog.SinkFunc(func(accesslog.Record) {}),
		},
		{
			name: "pointer",
			sink: newCollector(t, 0),
		},
		{
			name: "map",
			sink: mapSink{},
		},
		{
			name: "slice",
			sink: sliceSink{},
		},
		{
			name: "channel",
			sink: make(channelSink),
		},
		{
			name: "zero_scalar",
			sink: scalarSink(0),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, err := accesslog.New(accesslog.Config{
				Sink: tc.sink,
			})
			if err != nil {
				t.Errorf("New() error = %v, want nil", err)
			}

			if recorder == nil {
				t.Error("New() = nil, want a recorder")
			}
		})
	}
}
