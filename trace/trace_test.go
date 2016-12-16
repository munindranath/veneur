package trace

import (
	"context"
	"log"
	"net"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stripe/veneur/ssf"
)

const ε = .00002

func Test_StartTrace(t *testing.T) {
	const resource = "Robert'); DROP TABLE students;"
	const expectedParent int64 = 0
	start := time.Now()
	trace := StartTrace(resource)
	end := time.Now()

	between := end.After(trace.Start) && trace.Start.After(start)

	assert.Equal(t, trace.TraceId, trace.SpanId)
	assert.Equal(t, trace.ParentId, expectedParent)
	assert.Equal(t, trace.Resource, resource)
	assert.True(t, between)
}

func Test_Record(t *testing.T) {
	const resource = "Robert'); DROP TABLE students;"
	const metricName = "veneur.trace.test"
	const serviceName = "veneur-test"
	Service = serviceName

	// arbitrary
	const BufferSize = 1087152

	traceAddr, err := net.ResolveUDPAddr("udp", localVeneurAddress)
	assert.NoError(t, err)
	serverConn, err := net.ListenUDP("udp", traceAddr)
	assert.NoError(t, err)

	err = serverConn.SetReadBuffer(BufferSize)
	assert.NoError(t, err)

	respChan := make(chan []byte)
	kill := make(chan struct{})

	go func() {
		buf := make([]byte, BufferSize)
		n, _, err := serverConn.ReadFrom(buf)
		assert.NoError(t, err)

		buf = buf[:n]
		respChan <- buf
	}()

	go func() {
		<-time.After(5 * time.Second)
		kill <- struct{}{}
	}()

	trace := StartTrace(resource)
	trace.Status = ssf.SSFSample_CRITICAL
	trace.Record(metricName, nil)
	end := time.Now()

	select {
	case _ = <-kill:
		assert.Fail(t, "timed out waiting for socket read")
	case resp := <-respChan:
		// Because this is marshalled using protobuf,
		// we can't expect the representation to be immutable
		// and cannot test the marshalled payload directly
		log.Printf("resp is %v", string(resp))

		sample := &ssf.SSFSample{}
		err := proto.Unmarshal(resp, sample)
		assert.NoError(t, err)
		log.Printf("resp is %#v", sample)

		timestamp := time.Unix(sample.Timestamp/1e9, 0)

		assert.Equal(t, trace.Start.Unix(), timestamp.Unix())

		// We don't know the exact duration, but we can assert on the interval
		assert.True(t, sample.Value > 0, "Expected positive Value (duration)")
		upperBound := end.Sub(trace.Start).Nanoseconds()
		assert.True(t, sample.Value < upperBound, "Expected value (%d) to be less than upper bound %d", sample.Value, upperBound)
		assert.InEpsilon(t, sample.SampleRate, 0.1, ε)

		assert.Equal(t, sample.Resource, resource)
		assert.Equal(t, sample.Name, metricName)
		assert.Equal(t, sample.Status, ssf.SSFSample_CRITICAL)
		assert.Equal(t, sample.Metric, ssf.SSFSample_TRACE)
		assert.Equal(t, sample.Service, serviceName)
	}

}

func Test_Attach(t *testing.T) {
	const resource = "Robert'); DROP TABLE students;"
	ctx := context.Background()

	parent := ctx.Value(traceKey)
	assert.Nil(t, parent, "Expected not to find parent in context before attaching")

	trace := StartTrace(resource)
	ctx2 := trace.Attach(ctx)

	parent = ctx2.Value(traceKey).(*Trace)
	assert.NotNil(t, parent, "Expected not to find parent in context before attaching")
}

func Test_SpanFromContext(t *testing.T) {
	_ = context.Background()
}