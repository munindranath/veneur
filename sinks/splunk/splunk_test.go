package splunk_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/veneur/sinks/splunk"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/trace"
)

func jsonEndpoint(t testing.TB, ch chan<- splunk.Event) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") == "" {
			t.Errorf("Missing channel argument: %q", r.URL.String())
		}
		failed := false
		j := json.NewDecoder(r.Body)
		defer r.Body.Close()

		j.DisallowUnknownFields()
		for {
			input := splunk.Event{}
			err := j.Decode(&input)
			if err != nil {
				if err == io.EOF {
					return
				}
				if err == io.ErrUnexpectedEOF {
					t.Log("Encountered unexpected EOF, stopping")
					return
				}
				if strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				t.Errorf("Decoding JSON: %v", err)
				failed = true
				break
			}
			if ch != nil {
				ch <- input
			}
		}
		if failed {
			w.WriteHeader(400)
			w.Write([]byte(`{"text": "Error processing event", "code": 90}`))
		} else {
			w.Write([]byte(`{"text":"Success","code":0}`))
		}
	})
}

func TestSpanIngestBatch(t *testing.T) {
	const nToFlush = 10
	logger := logrus.StandardLogger()

	ch := make(chan splunk.Event, nToFlush)
	ts := httptest.NewServer(jsonEndpoint(t, ch))
	defer ts.Close()
	gsink, err := splunk.NewSplunkSpanSink(ts.URL, "00000000-0000-0000-0000-000000000000",
		"test-host", "", logger, time.Duration(0), time.Duration(0), nToFlush, 0, 1, 1*time.Second, 0)
	require.NoError(t, err)
	sink := gsink.(splunk.TestableSplunkSpanSink)
	err = sink.Start(nil)
	require.NoError(t, err)

	start := time.Unix(100000, 1000000)
	end := start.Add(5 * time.Second)
	span := &ssf.SSFSpan{
		ParentId:       4,
		TraceId:        6,
		StartTimestamp: start.UnixNano(),
		EndTimestamp:   end.UnixNano(),
		Service:        "test-srv",
		Name:           "test-span",
		Indicator:      true,
		Error:          true,
		Tags: map[string]string{
			"farts": "mandatory",
		},
		Metrics: []*ssf.SSFSample{
			ssf.Count("some.counter", 1, map[string]string{"purpose": "testing"}),
			ssf.Gauge("some.gauge", 20, map[string]string{"purpose": "testing"}),
		},
	}
	for i := 0; i < nToFlush; i++ {
		span.Id = int64(i + 1)
		err = sink.Ingest(span)
		require.NoError(t, err, "error ingesting the %dth span", i)
	}

	sink.Sync()

	for i := 0; i < nToFlush; i++ {
		event := <-ch
		fakeEvent := splunk.Event{}
		fakeEvent.SetTime(start)

		assert.Equal(t, *fakeEvent.Time, *event.Time)
		assert.Equal(t, "test-srv", *event.SourceType)
		assert.Equal(t, "test-host", *event.Host)

		spanB, err := json.Marshal(event.Event)
		require.NoError(t, err)
		output := splunk.SerializedSSF{}
		err = json.Unmarshal(spanB, &output)

		assert.Equal(t, float64(span.StartTimestamp)/float64(time.Second), output.StartTimestamp)
		assert.Equal(t, float64(span.EndTimestamp)/float64(time.Second), output.EndTimestamp)

		// span IDs can arrive out of order:
		spanID, err := strconv.Atoi(output.Id)
		require.NoError(t, err)
		assert.True(t, spanID < nToFlush+1, "Expected %d to be < %d", spanID, nToFlush)
		assert.True(t, spanID > 0, "Expected %d to be > 0", spanID)

		assert.Equal(t, strconv.FormatInt(span.ParentId, 10), output.ParentId)
		assert.Equal(t, strconv.FormatInt(span.TraceId, 10), output.TraceId)
		assert.Equal(t, "test-span", output.Name)
		assert.Equal(t, map[string]string{"farts": "mandatory"}, output.Tags)
		assert.Equal(t, true, output.Indicator)
		assert.Equal(t, true, output.Error)
	}
	sink.Stop()
}

type testBackend struct {
	spans chan *ssf.SSFSpan
}

func (be *testBackend) Close() error {
	return nil
}

func (be *testBackend) SendSync(ctx context.Context, span *ssf.SSFSpan) error {
	be.spans <- span
	return nil
}

func (be *testBackend) FlushSync(ctx context.Context) error {
	return nil
}

func TestTimeout(t *testing.T) {
	const nToFlush = 10
	logger := logrus.StandardLogger()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(100 * time.Millisecond))
	}))
	defer ts.Close()
	gsink, err := splunk.NewSplunkSpanSink(ts.URL, "00000000-0000-0000-0000-000000000000",
		"test-host", "", logger, time.Duration(0), time.Duration(10*time.Millisecond), nToFlush, 0, 1, 1*time.Second, 0)
	require.NoError(t, err)
	sink := gsink.(splunk.TestableSplunkSpanSink)

	spans := make(chan *ssf.SSFSpan)
	traceClient, err := trace.NewBackendClient(&testBackend{spans})
	require.NoError(t, err)
	err = sink.Start(traceClient)
	require.NoError(t, err)

	start := time.Unix(100000, 1000000)
	end := start.Add(5 * time.Second)
	span := &ssf.SSFSpan{
		ParentId:       4,
		TraceId:        6,
		StartTimestamp: start.UnixNano(),
		EndTimestamp:   end.UnixNano(),
		Service:        "test-srv",
		Name:           "test-span",
		Indicator:      true,
		Error:          true,
		Tags: map[string]string{
			"farts": "mandatory",
		},
		Metrics: []*ssf.SSFSample{
			ssf.Count("some.counter", 1, map[string]string{"purpose": "testing"}),
			ssf.Gauge("some.gauge", 20, map[string]string{"purpose": "testing"}),
		},
	}
	for i := 0; i < nToFlush; i++ {
		span.Id = int64(i + 1)
		err = sink.Ingest(span)
		require.NoError(t, err, "error ingesting the %dth span", i)
	}

	sink.Sync()
	ms := <-spans
	require.NotNil(t, ms)
	var found *ssf.SSFSample
	for _, sample := range ms.Metrics {
		if strings.HasSuffix(sample.Name, "splunk.hec_submission_failed_total") {
			found = sample
			break
		}
	}
	require.NotNil(t, found, "Expected a timeout metric to be reported")
	assert.Equal(t, found.Tags["cause"], "submission_timeout")
	sink.Stop()
}

const benchmarkCapacity = 100
const benchmarkWorkers = 3

func BenchmarkBatchIngest(b *testing.B) {
	logger := logrus.StandardLogger()

	// set up a null responder that we can flush to:
	ts := httptest.NewServer(jsonEndpoint(b, nil))
	defer ts.Close()
	gsink, err := splunk.NewSplunkSpanSink(ts.URL, "00000000-0000-0000-0000-000000000000",
		"test-host", "", logger, time.Duration(0), time.Duration(0), benchmarkCapacity, benchmarkWorkers, 1, 1*time.Second, 0)
	require.NoError(b, err)
	sink := gsink.(splunk.TestableSplunkSpanSink)

	err = sink.Start(nil)
	require.NoError(b, err)

	start := time.Unix(100000, 1000000)
	end := start.Add(5 * time.Second)
	span := &ssf.SSFSpan{
		ParentId:       4,
		TraceId:        6,
		Id:             0,
		StartTimestamp: start.UnixNano(),
		EndTimestamp:   end.UnixNano(),
		Service:        "test-srv",
		Name:           "test-span",
		Indicator:      true,
		Error:          true,
		Tags: map[string]string{
			"farts": "mandatory",
		},
		Metrics: []*ssf.SSFSample{
			ssf.Count("some.counter", 1, map[string]string{"purpose": "testing"}),
			ssf.Gauge("some.gauge", 20, map[string]string{"purpose": "testing"}),
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		span.Id = int64(i + 1)
		err = sink.Ingest(span)
		require.NoError(b, err)
	}
	b.StopTimer()
	sink.Stop()
}

func TestSampling(t *testing.T) {
	const nToFlush = 1000
	logger := logrus.StandardLogger()

	ch := make(chan splunk.Event, nToFlush)
	ts := httptest.NewServer(jsonEndpoint(t, ch))
	gsink, err := splunk.NewSplunkSpanSink(ts.URL, "00000000-0000-0000-0000-000000000000",
		"test-host", "", logger, time.Duration(0), time.Duration(0), nToFlush, 0, 10, 1*time.Second, 0)
	require.NoError(t, err)
	sink := gsink.(splunk.TestableSplunkSpanSink)
	err = sink.Start(nil)
	require.NoError(t, err)

	start := time.Unix(100000, 1000000)
	end := start.Add(5 * time.Second)
	span := &ssf.SSFSpan{
		ParentId:       4,
		StartTimestamp: start.UnixNano(),
		EndTimestamp:   end.UnixNano(),
		Service:        "test-srv",
		Name:           "test-span",
		Indicator:      false,
		Error:          true,
		Tags: map[string]string{
			"farts": "mandatory",
		},
		Metrics: []*ssf.SSFSample{
			ssf.Count("some.counter", 1, map[string]string{"purpose": "testing"}),
			ssf.Gauge("some.gauge", 20, map[string]string{"purpose": "testing"}),
		},
	}
	for i := 0; i < nToFlush; i++ {
		span.Id = int64(i + 1)
		span.TraceId = int64(i + 1)
		err = sink.Ingest(span)
		require.NoError(t, err, "error ingesting the %dth span", i)
	}

	sink.Sync()

	// Ensure nothing sends into the channel anymore:
	sink.Stop()

	// check how many events we got:
	events := 0
	for _ = range ch {
		events++
		// Don't close the receiving end until the first
		// span, to avoid failing the test by racing the
		// receiver:
		if ch != nil {
			ts.Close()
			close(ch)
			ch = nil
		}
	}
	assert.True(t, events > 0, "Should have sent around 1/10 of spans, but received zero")
	assert.True(t, events < nToFlush/2, "Should have sent less than all the spans, but received %d of %d", events, nToFlush)
	t.Logf("Received %d of %d events", events, nToFlush)
}

func TestSamplingIndicators(t *testing.T) {
	const nToFlush = 100
	logger := logrus.StandardLogger()

	ch := make(chan splunk.Event, nToFlush)
	ts := httptest.NewServer(jsonEndpoint(t, ch))
	gsink, err := splunk.NewSplunkSpanSink(ts.URL, "00000000-0000-0000-0000-000000000000",
		"test-host", "", logger, time.Duration(0), time.Duration(0), nToFlush, 0, 10, 1*time.Second, 0)
	require.NoError(t, err)
	sink := gsink.(splunk.TestableSplunkSpanSink)
	err = sink.Start(nil)
	require.NoError(t, err)

	start := time.Unix(100000, 1000000)
	end := start.Add(5 * time.Second)
	span := &ssf.SSFSpan{
		ParentId:       4,
		StartTimestamp: start.UnixNano(),
		EndTimestamp:   end.UnixNano(),
		Service:        "test-srv",
		Name:           "test-span",
		Indicator:      true,
		Error:          true,
		Tags: map[string]string{
			"farts": "mandatory",
		},
		Metrics: []*ssf.SSFSample{
			ssf.Count("some.counter", 1, map[string]string{"purpose": "testing"}),
			ssf.Gauge("some.gauge", 20, map[string]string{"purpose": "testing"}),
		},
	}
	for i := 0; i < nToFlush; i++ {
		span.Id = int64(i + 1)
		span.TraceId = int64(i + 1)
		err = sink.Ingest(span)
		require.NoError(t, err, "error ingesting the %dth span", i)
	}

	sink.Sync()

	// Ensure nothing sends into the channel anymore:
	sink.Stop()

	// check how many events we got:
	events := 0
	for _ = range ch {
		events++
		// Don't close the receiving end until the first
		// span, to avoid failing the test by racing the
		// receiver:
		if ch != nil {
			ts.Close()
			close(ch)
			ch = nil
		}
	}
	assert.Equal(t, events, nToFlush, "Should have sent all the spans, but received %d of %d", events, nToFlush)
	t.Logf("Received %d of %d events", events, nToFlush)
}
