// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package requestbatcher

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sync/errgroup"
)

type batchResp struct {
	br *roachpb.BatchResponse
	pe *roachpb.Error
}

type batchSend struct {
	ba       roachpb.BatchRequest
	respChan chan<- batchResp
}

type chanSender chan batchSend

func (c chanSender) Send(
	ctx context.Context, ba roachpb.BatchRequest,
) (*roachpb.BatchResponse, *roachpb.Error) {
	respChan := make(chan batchResp)
	select {
	case c <- batchSend{ba: ba, respChan: respChan}:
	case <-ctx.Done():
		return nil, roachpb.NewError(ctx.Err())
	}
	resp := <-respChan
	return resp.br, resp.pe
}

func TestBatcherSend(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.Background())
	sc := make(chanSender)
	b := New(Config{
		MaxIdle:         50 * time.Millisecond,
		MaxWait:         50 * time.Millisecond,
		MaxMsgsPerBatch: 3,
		Sender:          sc,
		Stopper:         stopper,
	})
	var g errgroup.Group
	sendRequest := func(rangeID roachpb.RangeID, request roachpb.Request) {
		g.Go(func() error {
			_, err := b.Send(context.Background(), rangeID, request)
			return err
		})
	}
	// Send 3 requests to range 2 and 2 to range 1.
	// The 3rd range 2 request will trigger immediate sending due to the
	// MaxMsgsPerBatch configuration. The range 1 batch will be sent after the
	// MaxWait timeout expires.
	sendRequest(1, &roachpb.GetRequest{})
	sendRequest(2, &roachpb.GetRequest{})
	sendRequest(1, &roachpb.GetRequest{})
	sendRequest(2, &roachpb.GetRequest{})
	sendRequest(2, &roachpb.GetRequest{})
	// Wait for the range 2 request and ensure it contains 3 requests.
	s := <-sc
	assert.Len(t, s.ba.Requests, 3)
	s.respChan <- batchResp{}
	// Wait for the range 1 request and ensure it contains 2 requests.
	s = <-sc
	assert.Len(t, s.ba.Requests, 2)
	s.respChan <- batchResp{}
	// Make sure everything gets a response.
	if err := g.Wait(); err != nil {
		t.Fatalf("expected no errors, got %v", err)
	}
}

func TestSendAfterStopped(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	sc := make(chanSender)
	b := New(Config{
		Sender:  sc,
		Stopper: stopper,
	})
	stopper.Stop(context.Background())
	_, err := b.Send(context.Background(), 1, &roachpb.GetRequest{})
	assert.Equal(t, err, stop.ErrUnavailable)
}

func TestSendAfterCanceled(t *testing.T) {
	defer leaktest.AfterTest(t)()
	sc := make(chanSender)
	stopper := stop.NewStopper()
	defer stopper.Stop(context.Background())
	b := New(Config{
		Sender:  sc,
		Stopper: stopper,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Send(ctx, 1, &roachpb.GetRequest{})
	assert.Equal(t, err, ctx.Err())
}

func TestStopDuringSend(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	sc := make(chanSender, 1)
	b := New(Config{
		Sender:  sc,
		Stopper: stopper,
		MaxWait: 10 * time.Millisecond,
		MaxIdle: 10 * time.Millisecond,
	})
	errChan := make(chan error)
	go func() {
		_, err := b.Send(context.Background(), 1, &roachpb.GetRequest{})
		errChan <- err
	}()
	r := <-sc
	go stopper.Stop(context.Background())
	assert.Equal(t, <-errChan, stop.ErrUnavailable)
	r.respChan <- batchResp{}
}

func TestPanicWithNilStopper(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("failed to panic with a nil Stopper")
		}
	}()
	New(Config{Sender: make(chanSender)})
}

func TestTimeoutDisabled(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.Background())
	sc := make(chanSender)
	b := New(Config{
		MaxMsgsPerBatch: 2,
		Sender:          sc,
		Stopper:         stopper,
	})
	var g errgroup.Group
	sendRequest := func(rangeID roachpb.RangeID, request roachpb.Request) {
		g.Go(func() error {
			_, err := b.Send(context.Background(), rangeID, request)
			return err
		})
	}
	// Send 3 requests to range 2 and 2 to range 1.
	// The 3rd range 2 request will trigger immediate sending due to the
	// MaxMsgsPerBatch configuration. The range 1 batch will be sent after the
	// MaxWait timeout expires.
	sendRequest(1, &roachpb.GetRequest{})
	select {
	case <-sc:
		t.Fatalf("RequestBatcher should not sent based on time")
	case <-time.After(10 * time.Millisecond):
	}
	sendRequest(1, &roachpb.GetRequest{})
	s := <-sc
	assert.Len(t, s.ba.Requests, 2)
	s.respChan <- batchResp{}
	// Make sure everything gets a response.
	if err := g.Wait(); err != nil {
		t.Fatalf("expected no errors, got %v", err)
	}
}

func TestPanicWithNilSender(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("failed to panic with a nil Sender")
		}
	}()
	New(Config{Stopper: stop.NewStopper()})
}
