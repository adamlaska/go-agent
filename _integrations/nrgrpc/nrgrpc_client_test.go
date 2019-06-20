package nrgrpc

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	newrelic "github.com/newrelic/go-agent"
	"github.com/newrelic/go-agent/_integrations/nrgrpc/testapp"
	"github.com/newrelic/go-agent/internal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func TestGetURL(t *testing.T) {
	testcases := []struct {
		method   string
		target   string
		expected string
	}{
		{
			method:   "/TestApplication/DoUnaryUnary",
			target:   "",
			expected: "",
		},
		{
			method:   "/TestApplication/DoUnaryUnary",
			target:   ":8080",
			expected: "grpc://:8080/TestApplication/DoUnaryUnary",
		},
		{
			method:   "/TestApplication/DoUnaryUnary",
			target:   "localhost:8080",
			expected: "grpc://localhost:8080/TestApplication/DoUnaryUnary",
		},
		{
			method:   "/TestApplication/DoUnaryUnary",
			target:   "dns:///localhost:8080",
			expected: "grpc://localhost:8080/TestApplication/DoUnaryUnary",
		},
		{
			method:   "/TestApplication/DoUnaryUnary",
			target:   "unix:/path/to/socket",
			expected: "grpc://localhost/TestApplication/DoUnaryUnary",
		},
		{
			method:   "/TestApplication/DoUnaryUnary",
			target:   "unix:///path/to/socket",
			expected: "grpc://localhost/TestApplication/DoUnaryUnary",
		},
	}

	for _, test := range testcases {
		actual := getURL(test.method, test.target)
		if actual != test.expected {
			t.Errorf("incorrect URL:\n\tmethod=%s,\n\ttarget=%s,\n\texpected=%s,\n\tactual=%s",
				test.method, test.target, test.expected, actual)
		}
		if _, err := url.Parse(actual); nil != err {
			t.Error("the resultant url is not a url!", err)
		}
	}
}

var conn *grpc.ClientConn

func TestMain(m *testing.M) {
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	testapp.RegisterTestApplicationServer(s, &testapp.Server{})
	go func() {
		if err := s.Serve(lis); err != nil {
			panic(err)
		}
	}()

	var err error
	bufDialer := func(string, time.Duration) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err = grpc.Dial("bufnet",
		grpc.WithDialer(bufDialer),
		grpc.WithInsecure(),
		grpc.WithBlock(), // create the connection synchronously
		grpc.WithUnaryInterceptor(UnaryClientInterceptor),
	)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	os.Exit(m.Run())
}

func testApp(t *testing.T) newrelic.Application {
	cfg := newrelic.NewConfig("appname", "0123456789012345678901234567890123456789")
	cfg.Enabled = false
	cfg.DistributedTracer.Enabled = true
	cfg.TransactionTracer.SegmentThreshold = 0
	cfg.TransactionTracer.Threshold.IsApdexFailing = false
	cfg.TransactionTracer.Threshold.Duration = 0
	app, err := newrelic.NewApplication(cfg)
	if nil != err {
		t.Fatal(err)
	}
	replyfn := func(reply *internal.ConnectReply) {
		reply.AdaptiveSampler = internal.SampleEverything{}
		reply.AccountID = "123"
		reply.TrustedAccountKey = "123"
	}
	internal.HarvestTesting(app, replyfn)
	return app
}

func TestUnaryClientInterceptor(t *testing.T) {
	app := testApp(t)
	txn := app.StartTransaction("UnaryUnary", nil, nil)
	ctx := newrelic.NewContext(context.Background(), txn)

	client := testapp.NewTestApplicationClient(conn)
	resp, err := client.DoUnaryUnary(ctx, &testapp.Message{})
	if nil != err {
		t.Fatal("client call to DoUnaryUnary failed", err)
	}
	var hdrs map[string][]string
	err = json.Unmarshal([]byte(resp.Text), &hdrs)
	if nil != err {
		t.Fatal("cannot unmarshall client response", err)
	}
	if hdr, ok := hdrs["newrelic"]; !ok || len(hdr) != 1 || "" == hdr[0] {
		t.Error("distributed trace header not sent", hdrs)
	}
	txn.End()

	app.(internal.Expect).ExpectMetrics(t, []internal.WantMetric{
		{Name: "OtherTransaction/Go/UnaryUnary", Scope: "", Forced: true, Data: nil},
		{Name: "OtherTransaction/all", Scope: "", Forced: true, Data: nil},
		{Name: "OtherTransactionTotalTime", Scope: "", Forced: true, Data: nil},
		{Name: "OtherTransactionTotalTime/Go/UnaryUnary", Scope: "", Forced: false, Data: nil},
		{Name: "DurationByCaller/Unknown/Unknown/Unknown/Unknown/all", Scope: "", Forced: false, Data: nil},
		{Name: "DurationByCaller/Unknown/Unknown/Unknown/Unknown/allOther", Scope: "", Forced: false, Data: nil},
		{Name: "External/all", Scope: "", Forced: true, Data: nil},
		{Name: "External/allOther", Scope: "", Forced: true, Data: nil},
		{Name: "External/bufnet/all", Scope: "", Forced: false, Data: nil},
		// FIXME: should be External/bufnet/gRPC/TestApplication/DoUnaryUnary
		{Name: "External/bufnet/all", Scope: "OtherTransaction/Go/UnaryUnary", Forced: false, Data: nil},
		{Name: "Supportability/DistributedTrace/CreatePayload/Success", Scope: "", Forced: true, Data: nil},
	})
	app.(internal.Expect).ExpectSpanEvents(t, []internal.WantEvent{
		{
			Intrinsics: map[string]interface{}{
				"category":      "generic",
				"name":          "OtherTransaction/Go/UnaryUnary",
				"nr.entryPoint": true,
			},
			UserAttributes:  map[string]interface{}{},
			AgentAttributes: map[string]interface{}{},
		},
		{
			Intrinsics: map[string]interface{}{
				"category":  "http",
				"component": "http",                // FIXME: should be gRPC
				"name":      "External/bufnet/all", // FIXME: should be External/bufnet/gRPC/TestApplication/DoUnaryUnary
				"parentId":  internal.MatchAnything,
				"span.kind": "client",
			},
			UserAttributes: map[string]interface{}{},
			AgentAttributes: map[string]interface{}{
				"http.url": "grpc://bufnet/TestApplication/DoUnaryUnary",
				// FIXME: also include "http.method": "TestApplication/DoUnaryUnary"
			},
		},
	})
	app.(internal.Expect).ExpectTxnTraces(t, []internal.WantTxnTrace{{
		MetricName: "OtherTransaction/Go/UnaryUnary",
		Root: internal.WantTraceSegment{
			SegmentName: "ROOT",
			Attributes:  map[string]interface{}{},
			Children: []internal.WantTraceSegment{{
				SegmentName: "OtherTransaction/Go/UnaryUnary",
				Attributes:  map[string]interface{}{"exclusive_duration_millis": internal.MatchAnything},
				Children: []internal.WantTraceSegment{
					{
						SegmentName: "External/bufnet/all", // FIXME: should be External/bufnet/gRPC/TestApplication/DoUnaryUnary
						Attributes: map[string]interface{}{
							"http.url": "grpc://bufnet/TestApplication/DoUnaryUnary",
						},
					},
				},
			}},
		},
	}})
}

func TestClientUnaryMetadata(t *testing.T) {
	// Test that metadata on the outgoing request are presevered
	app := testApp(t)
	txn := app.StartTransaction("metadata", nil, nil)
	ctx := newrelic.NewContext(context.Background(), txn)

	md := metadata.New(map[string]string{
		"testing":  "hello world",
		"newrelic": "payload",
	})
	ctx = metadata.NewOutgoingContext(ctx, md)

	client := testapp.NewTestApplicationClient(conn)
	resp, err := client.DoUnaryUnary(ctx, &testapp.Message{})
	if nil != err {
		t.Fatal("client call to DoUnaryUnary failed", err)
	}
	var hdrs map[string][]string
	err = json.Unmarshal([]byte(resp.Text), &hdrs)
	if nil != err {
		t.Fatal("cannot unmarshall client response", err)
	}
	if hdr, ok := hdrs["newrelic"]; !ok || len(hdr) != 1 || "" == hdr[0] || "payload" == hdr[0] {
		t.Error("distributed trace header not sent", hdrs)
	}
	if hdr, ok := hdrs["testing"]; !ok || len(hdr) != 1 || "hello world" != hdr[0] {
		t.Error("testing header not sent", hdrs)
	}
}

func TestNilTxn(t *testing.T) {
	client := testapp.NewTestApplicationClient(conn)
	resp, err := client.DoUnaryUnary(context.Background(), &testapp.Message{})
	if nil != err {
		t.Fatal("client call to DoUnaryUnary failed", err)
	}
	var hdrs map[string][]string
	err = json.Unmarshal([]byte(resp.Text), &hdrs)
	if nil != err {
		t.Fatal("cannot unmarshall client response", err)
	}
	if _, ok := hdrs["newrelic"]; ok {
		t.Error("distributed trace header sent", hdrs)
	}
}