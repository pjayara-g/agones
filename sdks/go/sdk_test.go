// Copyright 2017 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sdk

import (
	"testing"
	"time"

	"agones.dev/agones/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestSDK(t *testing.T) {
	sm := &sdkMock{
		hm: &healthMock{},
	}
	s := SDK{
		ctx:    context.Background(),
		client: sm,
		health: sm.hm,
	}

	//gate
	assert.False(t, sm.ready)
	assert.False(t, sm.shutdown)
	assert.False(t, sm.hm.healthy)

	err := s.Ready()
	assert.Nil(t, err)
	assert.True(t, sm.ready)
	assert.False(t, sm.shutdown)

	err = s.Health()
	assert.Nil(t, err)
	assert.True(t, sm.hm.healthy)

	err = s.Allocate()
	assert.NoError(t, err)
	assert.True(t, sm.allocated)

	err = s.Shutdown()
	assert.Nil(t, err)
	assert.True(t, sm.ready)
	assert.True(t, sm.shutdown)

	gs, err := s.GameServer()
	assert.Nil(t, err)
	assert.NotNil(t, gs)
}

func TestSDKWatchGameServer(t *testing.T) {
	sm := &sdkMock{
		wm: &watchMock{msgs: make(chan *sdk.GameServer, 5)},
	}
	s := SDK{
		ctx:    context.Background(),
		client: sm,
	}

	fixture := &sdk.GameServer{ObjectMeta: &sdk.GameServer_ObjectMeta{Name: "test-server"}}

	updated := make(chan struct{}, 5)

	err := s.WatchGameServer(func(gs *sdk.GameServer) {
		assert.Equal(t, fixture.ObjectMeta.Name, gs.ObjectMeta.Name)
		updated <- struct{}{}
	})
	assert.Nil(t, err)

	sm.wm.msgs <- fixture

	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		assert.FailNow(t, "update handler should have fired")
	}
}

func TestSDKSetLabel(t *testing.T) {
	t.Parallel()
	sm := &sdkMock{
		labels: map[string]string{},
	}
	s := SDK{
		ctx:    context.Background(),
		client: sm,
	}

	expected := "bar"
	err := s.SetLabel("foo", expected)
	assert.Nil(t, err)
	assert.Equal(t, expected, sm.labels["foo"])
}

func TestSDKSetAnnotation(t *testing.T) {
	t.Parallel()
	sm := &sdkMock{
		annotations: map[string]string{},
	}
	s := SDK{
		ctx:    context.Background(),
		client: sm,
	}

	expected := "bar"
	err := s.SetAnnotation("foo", expected)
	assert.Nil(t, err)
	assert.Equal(t, expected, sm.annotations["foo"])
}

var _ sdk.SDKClient = &sdkMock{}
var _ sdk.SDK_HealthClient = &healthMock{}
var _ sdk.SDK_WatchGameServerClient = &watchMock{}

type sdkMock struct {
	ready       bool
	shutdown    bool
	allocated   bool
	reserved    *sdk.Duration
	hm          *healthMock
	wm          *watchMock
	labels      map[string]string
	annotations map[string]string
}

func (m *sdkMock) SetLabel(ctx context.Context, in *sdk.KeyValue, opts ...grpc.CallOption) (*sdk.Empty, error) {
	m.labels[in.Key] = in.Value
	return &sdk.Empty{}, nil
}

func (m *sdkMock) SetAnnotation(ctx context.Context, in *sdk.KeyValue, opts ...grpc.CallOption) (*sdk.Empty, error) {
	m.annotations[in.Key] = in.Value
	return &sdk.Empty{}, nil
}

func (m *sdkMock) WatchGameServer(ctx context.Context, in *sdk.Empty, opts ...grpc.CallOption) (sdk.SDK_WatchGameServerClient, error) {
	return m.wm, nil
}

func (m *sdkMock) GetGameServer(ctx context.Context, in *sdk.Empty, opts ...grpc.CallOption) (*sdk.GameServer, error) {
	return &sdk.GameServer{}, nil
}

func (m *sdkMock) Ready(ctx context.Context, e *sdk.Empty, opts ...grpc.CallOption) (*sdk.Empty, error) {
	m.ready = true
	return e, nil
}

func (m *sdkMock) Allocate(ctx context.Context, e *sdk.Empty, opts ...grpc.CallOption) (*sdk.Empty, error) {
	m.allocated = true
	return e, nil
}

func (m *sdkMock) Shutdown(ctx context.Context, e *sdk.Empty, opts ...grpc.CallOption) (*sdk.Empty, error) {
	m.shutdown = true
	return e, nil
}

func (m *sdkMock) Health(ctx context.Context, opts ...grpc.CallOption) (sdk.SDK_HealthClient, error) {
	return m.hm, nil
}

func (m *sdkMock) Reserve(ctx context.Context, in *sdk.Duration, opts ...grpc.CallOption) (*sdk.Empty, error) {
	m.reserved = in
	return &sdk.Empty{}, nil
}

type healthMock struct {
	healthy bool
}

func (h *healthMock) Send(*sdk.Empty) error {
	h.healthy = true
	return nil
}

func (h *healthMock) CloseAndRecv() (*sdk.Empty, error) {
	panic("implement me")
}

func (h *healthMock) Header() (metadata.MD, error) {
	panic("implement me")
}

func (h *healthMock) Trailer() metadata.MD {
	panic("implement me")
}

func (h *healthMock) CloseSend() error {
	panic("implement me")
}

func (h *healthMock) Context() context.Context {
	panic("implement me")
}

func (h *healthMock) SendMsg(m interface{}) error {
	panic("implement me")
}

func (h *healthMock) RecvMsg(m interface{}) error {
	panic("implement me")
}

type watchMock struct {
	msgs chan *sdk.GameServer
}

func (wm *watchMock) Recv() (*sdk.GameServer, error) {
	return <-wm.msgs, nil
}

func (*watchMock) Header() (metadata.MD, error) {
	panic("implement me")
}

func (*watchMock) Trailer() metadata.MD {
	panic("implement me")
}

func (*watchMock) CloseSend() error {
	panic("implement me")
}

func (*watchMock) Context() context.Context {
	panic("implement me")
}

func (*watchMock) SendMsg(m interface{}) error {
	panic("implement me")
}

func (*watchMock) RecvMsg(m interface{}) error {
	panic("implement me")
}
