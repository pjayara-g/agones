// Copyright 2018 Google LLC All Rights Reserved.
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

package gameserverallocations

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	multiclusterv1alpha1 "agones.dev/agones/pkg/apis/multicluster/v1alpha1"
	stablev1alpha1 "agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/gameservers"
	agtesting "agones.dev/agones/pkg/testing"
	"agones.dev/agones/pkg/util/apiserver"
	"github.com/heptiolabs/healthcheck"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

const (
	defaultNs = "default"
	n1        = "node1"
	n2        = "node2"
)

func TestControllerAllocationHandler(t *testing.T) {
	t.Parallel()

	t.Run("successful allocation", func(t *testing.T) {
		f, _, gsList := defaultFixtures(3)

		gsa := &allocationv1.GameServerAllocation{
			Spec: allocationv1.GameServerAllocationSpec{
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: f.ObjectMeta.Name}},
			}}

		c, m := newFakeController()
		gsWatch := watch.NewFake()
		m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(gsWatch, nil))
		m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &stablev1alpha1.GameServerList{Items: gsList}, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*stablev1alpha1.GameServer)
			gsWatch.Modify(gs)
			return true, gs, nil
		})

		stop, cancel := agtesting.StartInformers(m)
		defer cancel()

		go c.Run(1, stop) // nolint: errcheck
		// wait for it to be up and running
		err := wait.PollImmediate(time.Second, 10*time.Second, func() (done bool, err error) {
			return c.workerqueue.RunCount() == 1, nil
		})
		assert.NoError(t, err)

		test := func(gsa *allocationv1.GameServerAllocation, expectedState allocationv1.GameServerAllocationState) {
			buf := bytes.NewBuffer(nil)
			err := json.NewEncoder(buf).Encode(gsa)
			assert.NoError(t, err)
			r, err := http.NewRequest(http.MethodPost, "/", buf)
			r.Header.Set("Content-Type", k8sruntime.ContentTypeJSON)
			assert.NoError(t, err)
			rec := httptest.NewRecorder()
			err = c.allocationHandler(rec, r, "default")
			assert.NoError(t, err)
			ret := &allocationv1.GameServerAllocation{}
			err = json.Unmarshal(rec.Body.Bytes(), ret)
			assert.NoError(t, err)

			assert.Equal(t, gsa.Spec.Required, ret.Spec.Required)
			assert.True(t, expectedState == ret.Status.State, "Failed: %s vs %s", expectedState, ret.Status.State)
		}

		test(gsa.DeepCopy(), allocationv1.GameServerAllocationAllocated)
		test(gsa.DeepCopy(), allocationv1.GameServerAllocationAllocated)
		test(gsa.DeepCopy(), allocationv1.GameServerAllocationAllocated)
		test(gsa.DeepCopy(), allocationv1.GameServerAllocationUnAllocated)
	})

	t.Run("method not allowed", func(t *testing.T) {
		c, _ := newFakeController()
		r, err := http.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		assert.NoError(t, err)

		err = c.allocationHandler(rec, r, "default")
		assert.NoError(t, err)

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})

	t.Run("invalid gameserverallocation", func(t *testing.T) {
		c, _ := newFakeController()
		gsa := &allocationv1.GameServerAllocation{
			Spec: allocationv1.GameServerAllocationSpec{
				Scheduling: "wrong",
			}}
		buf := bytes.NewBuffer(nil)
		err := json.NewEncoder(buf).Encode(gsa)
		assert.NoError(t, err)
		r, err := http.NewRequest(http.MethodPost, "/", buf)
		r.Header.Set("Content-Type", k8sruntime.ContentTypeJSON)
		assert.NoError(t, err)
		rec := httptest.NewRecorder()
		err = c.allocationHandler(rec, r, "default")
		assert.NoError(t, err)

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

		s := &metav1.Status{}
		err = json.NewDecoder(rec.Body).Decode(s)
		assert.NoError(t, err)

		assert.Equal(t, metav1.StatusReasonInvalid, s.Reason)
	})
}

func TestControllerAllocate(t *testing.T) {
	t.Parallel()

	f, _, gsList := defaultFixtures(4)
	c, m := newFakeController()
	n := metav1.Now()
	l := map[string]string{"mode": "deathmatch"}
	a := map[string]string{"map": "searide"}
	fam := allocationv1.MetaPatch{Labels: l, Annotations: a}

	gsList[3].ObjectMeta.DeletionTimestamp = &n

	m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, &stablev1alpha1.GameServerList{Items: gsList}, nil
	})

	updated := false
	gsWatch := watch.NewFake()
	m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(gsWatch, nil))
	m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		ua := action.(k8stesting.UpdateAction)
		gs := ua.GetObject().(*stablev1alpha1.GameServer)

		updated = true
		assert.Equal(t, stablev1alpha1.GameServerStateAllocated, gs.Status.State)
		gsWatch.Modify(gs)

		return true, gs, nil
	})

	stop, cancel := agtesting.StartInformers(m)
	defer cancel()

	go c.Run(1, stop) // nolint: errcheck
	// wait for it to be up and running
	err := wait.PollImmediate(time.Second, 10*time.Second, func() (done bool, err error) {
		return c.workerqueue.RunCount() == 1, nil
	})
	assert.NoError(t, err)

	gsa := allocationv1.GameServerAllocation{ObjectMeta: metav1.ObjectMeta{Name: "gsa-1", Namespace: defaultNs},
		Spec: allocationv1.GameServerAllocationSpec{
			Required:  metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: f.ObjectMeta.Name}},
			MetaPatch: fam,
		}}
	gsa.ApplyDefaults()

	gs, err := c.allocate(&gsa)
	assert.Nil(t, err)
	assert.Equal(t, stablev1alpha1.GameServerStateAllocated, gs.Status.State)
	assert.True(t, updated)
	for key, value := range fam.Labels {
		v, ok := gs.ObjectMeta.Labels[key]
		assert.True(t, ok)
		assert.Equal(t, v, value)
	}
	for key, value := range fam.Annotations {
		v, ok := gs.ObjectMeta.Annotations[key]
		assert.True(t, ok)
		assert.Equal(t, v, value)
	}

	updated = false
	gs, err = c.allocate(&gsa)
	assert.Nil(t, err)
	assert.Equal(t, stablev1alpha1.GameServerStateAllocated, gs.Status.State)
	assert.True(t, updated)

	updated = false
	gs, err = c.allocate(&gsa)
	assert.Nil(t, err)
	assert.Equal(t, stablev1alpha1.GameServerStateAllocated, gs.Status.State)
	assert.True(t, updated)

	updated = false
	_, err = c.allocate(&gsa)
	assert.NotNil(t, err)
	assert.Equal(t, ErrNoGameServerReady, err)
	assert.False(t, updated)
}

func TestControllerAllocatePriority(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, name string, test func(t *testing.T, c *Controller, gas *allocationv1.GameServerAllocation)) {
		f, _, gsList := defaultFixtures(4)
		c, m := newFakeController()

		gsList[0].Status.NodeName = n1
		gsList[1].Status.NodeName = n2
		gsList[2].Status.NodeName = n1
		gsList[3].Status.NodeName = n1

		m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &stablev1alpha1.GameServerList{Items: gsList}, nil
		})

		gsWatch := watch.NewFake()
		m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(gsWatch, nil))
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*stablev1alpha1.GameServer)
			gsWatch.Modify(gs)

			return true, gs, nil
		})

		stop, cancel := agtesting.StartInformers(m)
		defer cancel()

		go c.Run(1, stop) // nolint: errcheck
		// wait for it to be up and running
		err := wait.PollImmediate(time.Second, 10*time.Second, func() (done bool, err error) {
			return c.workerqueue.RunCount() == 1, nil
		})
		assert.NoError(t, err)

		gas := &allocationv1.GameServerAllocation{ObjectMeta: metav1.ObjectMeta{Name: "fa-1", Namespace: defaultNs},
			Spec: allocationv1.GameServerAllocationSpec{
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: f.ObjectMeta.Name}},
			}}
		gas.ApplyDefaults()

		t.Run(name, func(t *testing.T) {
			test(t, c, gas.DeepCopy())
		})
	}

	run(t, "packed", func(t *testing.T, c *Controller, gas *allocationv1.GameServerAllocation) {
		// priority should be node1, then node2
		gs1, err := c.allocate(gas)
		assert.NoError(t, err)
		assert.Equal(t, n1, gs1.Status.NodeName)

		gs2, err := c.allocate(gas)
		assert.NoError(t, err)
		assert.Equal(t, n1, gs2.Status.NodeName)
		assert.NotEqual(t, gs1.ObjectMeta.Name, gs2.ObjectMeta.Name)

		gs3, err := c.allocate(gas)
		assert.NoError(t, err)
		assert.Equal(t, n1, gs3.Status.NodeName)
		assert.NotContains(t, []string{gs1.ObjectMeta.Name, gs2.ObjectMeta.Name}, gs3.ObjectMeta.Name)

		gs4, err := c.allocate(gas)
		assert.NoError(t, err)
		assert.Equal(t, n2, gs4.Status.NodeName)
		assert.NotContains(t, []string{gs1.ObjectMeta.Name, gs2.ObjectMeta.Name, gs3.ObjectMeta.Name}, gs4.ObjectMeta.Name)

		// should have none left
		_, err = c.allocate(gas)
		assert.Equal(t, err, ErrNoGameServerReady)
	})

	run(t, "distributed", func(t *testing.T, c *Controller, gas *allocationv1.GameServerAllocation) {
		// make a copy, to avoid the race check
		gas = gas.DeepCopy()
		gas.Spec.Scheduling = apis.Distributed

		// distributed is randomised, so no set pattern

		gs1, err := c.allocate(gas)
		assert.NoError(t, err)

		gs2, err := c.allocate(gas)
		assert.NoError(t, err)
		assert.NotEqual(t, gs1.ObjectMeta.Name, gs2.ObjectMeta.Name)

		gs3, err := c.allocate(gas)
		assert.NoError(t, err)
		assert.NotContains(t, []string{gs1.ObjectMeta.Name, gs2.ObjectMeta.Name}, gs3.ObjectMeta.Name)

		gs4, err := c.allocate(gas)
		assert.NoError(t, err)
		assert.NotContains(t, []string{gs1.ObjectMeta.Name, gs2.ObjectMeta.Name, gs3.ObjectMeta.Name}, gs4.ObjectMeta.Name)

		// should have none left
		_, err = c.allocate(gas)
		assert.Equal(t, err, ErrNoGameServerReady)
	})
}

func TestControllerRunLocalAllocations(t *testing.T) {
	t.Parallel()

	t.Run("no problems", func(t *testing.T) {
		f, _, gsList := defaultFixtures(5)
		gsList[0].Status.NodeName = "special"

		c, m := newFakeController()
		m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &stablev1alpha1.GameServerList{Items: gsList}, nil
		})
		updateCount := 0
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			updateCount++

			uo := action.(k8stesting.UpdateAction)
			gs := uo.GetObject().(*stablev1alpha1.GameServer)

			return true, gs, nil
		})

		stop, cancel := agtesting.StartInformers(m, c.gameServerSynced)
		defer cancel()

		// This call initializes the cache
		err := c.syncReadyGSServerCache()
		assert.Nil(t, err)

		err = c.counter.Run(0, stop)
		assert.Nil(t, err)

		gsa := &allocationv1.GameServerAllocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: defaultNs,
			},
			Spec: allocationv1.GameServerAllocationSpec{
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: f.ObjectMeta.Name}},
			}}
		gsa.ApplyDefaults()

		// line up 3 in a batch
		j1 := request{gsa: gsa.DeepCopy(), response: make(chan response)}
		c.pendingRequests <- j1
		j2 := request{gsa: gsa.DeepCopy(), response: make(chan response)}
		c.pendingRequests <- j2
		j3 := request{gsa: gsa.DeepCopy(), response: make(chan response)}
		c.pendingRequests <- j3

		go c.runLocalAllocations(3)

		res1 := <-j1.response
		assert.NoError(t, res1.err)
		assert.NotNil(t, res1.gs)

		// since we gave gsList[0] a different nodename, it should always come first
		assert.Contains(t, []string{"gs2", "gs3", "gs4", "gs5"}, res1.gs.ObjectMeta.Name)
		assert.Equal(t, stablev1alpha1.GameServerStateAllocated, res1.gs.Status.State)

		res2 := <-j2.response
		assert.NoError(t, res2.err)
		assert.NotNil(t, res2.gs)
		assert.NotEqual(t, res1.gs.ObjectMeta.Name, res2.gs.ObjectMeta.Name)
		assert.Equal(t, stablev1alpha1.GameServerStateAllocated, res2.gs.Status.State)

		res3 := <-j3.response
		assert.NoError(t, res3.err)
		assert.NotNil(t, res3.gs)
		assert.Equal(t, stablev1alpha1.GameServerStateAllocated, res3.gs.Status.State)
		assert.NotEqual(t, res1.gs.ObjectMeta.Name, res3.gs.ObjectMeta.Name)
		assert.NotEqual(t, res2.gs.ObjectMeta.Name, res3.gs.ObjectMeta.Name)

		assert.Equal(t, 3, updateCount)
	})

	t.Run("no gameservers", func(t *testing.T) {
		c, m := newFakeController()
		stop, cancel := agtesting.StartInformers(m, c.gameServerSynced)
		defer cancel()
		c.stop = stop

		// This call initializes the cache
		err := c.syncReadyGSServerCache()
		assert.Nil(t, err)

		err = c.counter.Run(0, stop)
		assert.Nil(t, err)

		gsa := &allocationv1.GameServerAllocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: defaultNs,
			},
			Spec: allocationv1.GameServerAllocationSpec{
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: "thereisnofleet"}},
			}}
		gsa.ApplyDefaults()

		j1 := request{gsa: gsa.DeepCopy(), response: make(chan response)}
		c.pendingRequests <- j1

		go c.runLocalAllocations(3)

		res1 := <-j1.response
		assert.Nil(t, res1.gs)
		assert.Error(t, res1.err)
		assert.Equal(t, ErrNoGameServerReady, res1.err)
	})
}

func TestAllocationApiResource(t *testing.T) {
	t.Parallel()

	_, m := newFakeController()
	ts := httptest.NewServer(m.Mux)
	defer ts.Close()

	client := ts.Client()

	resp, err := client.Get(ts.URL + "/apis/" + allocationv1.SchemeGroupVersion.String())
	if !assert.Nil(t, err) {
		assert.FailNow(t, err.Error())
	}
	defer resp.Body.Close() // nolint: errcheck

	list := &metav1.APIResourceList{}
	err = json.NewDecoder(resp.Body).Decode(list)
	assert.Nil(t, err)

	assert.Len(t, list.APIResources, 1)
	assert.Equal(t, "gameserverallocation", list.APIResources[0].SingularName)
}

func TestControllerRunCacheSync(t *testing.T) {
	c, m := newFakeController()
	watch := watch.NewFake()

	m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(watch, nil))

	stop, cancel := agtesting.StartInformers(m, c.gameServerSynced)
	defer cancel()

	assertCacheEntries := func(expected int) {
		count := 0
		err := wait.PollImmediate(time.Second, 5*time.Second, func() (done bool, err error) {
			count = 0
			c.readyGameServers.Range(func(key string, gs *stablev1alpha1.GameServer) bool {
				count++
				return true
			})

			return count == expected, nil
		})

		assert.NoError(t, err, fmt.Sprintf("Should be %d values", expected))
	}

	go func() {
		err := c.Run(1, stop)
		assert.Nil(t, err)
	}()

	gs := stablev1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gs1", Namespace: "default"},
		Status:     stablev1alpha1.GameServerStatus{State: stablev1alpha1.GameServerStateStarting},
	}

	logrus.Info("adding ready game server")
	watch.Add(gs.DeepCopy())

	assertCacheEntries(0)

	gs.Status.State = stablev1alpha1.GameServerStateReady
	watch.Modify(gs.DeepCopy())

	assertCacheEntries(1)

	// try again, should be no change
	gs.Status.State = stablev1alpha1.GameServerStateReady
	watch.Modify(gs.DeepCopy())

	assertCacheEntries(1)

	// now move it to Shutdown
	gs.Status.State = stablev1alpha1.GameServerStateShutdown
	watch.Modify(gs.DeepCopy())

	assertCacheEntries(0)

	// add back in ready gameserver
	gs.Status.State = stablev1alpha1.GameServerStateReady
	watch.Modify(gs.DeepCopy())
	assertCacheEntries(0)
	gs.Status.State = stablev1alpha1.GameServerStateReady
	watch.Modify(gs.DeepCopy())
	assertCacheEntries(1)

	// update with deletion timestamp
	n := metav1.Now()
	deletedCopy := gs.DeepCopy()
	deletedCopy.ObjectMeta.DeletionTimestamp = &n
	watch.Modify(deletedCopy)
	assertCacheEntries(0)

	// add back in ready gameserver
	gs.Status.State = stablev1alpha1.GameServerStateReady
	watch.Modify(gs.DeepCopy())
	assertCacheEntries(0)
	gs.Status.State = stablev1alpha1.GameServerStateReady
	watch.Modify(gs.DeepCopy())
	assertCacheEntries(1)

	// now actually delete it
	watch.Delete(gs.DeepCopy())
	assertCacheEntries(0)
}

func TestGetRandomlySelectedGS(t *testing.T) {
	c, _ := newFakeController()
	c.topNGameServerCount = 5
	gsa := &allocationv1.GameServerAllocation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: defaultNs,
			Name:      "allocation",
		},
		Status: allocationv1.GameServerAllocationStatus{
			State: allocationv1.GameServerAllocationUnAllocated,
		},
	}

	_, _, gsList := defaultFixtures(10)

	selectedGS := c.getRandomlySelectedGS(gsa, gsList)
	assert.NotNil(t, "selectedGS can't be nil", selectedGS)
	for i := 1; i <= 5; i++ {
		expectedName := "gs" + strconv.Itoa(i)
		assert.NotEqual(t, expectedName, selectedGS.ObjectMeta.Name)
	}

	_, _, gsList = defaultFixtures(5)

	selectedGS = c.getRandomlySelectedGS(gsa, gsList)
	assert.NotNil(t, "selectedGS can't be nil", selectedGS)

	_, _, gsList = defaultFixtures(1)

	selectedGS = c.getRandomlySelectedGS(gsa, gsList)
	assert.NotNil(t, "selectedGS can't be nil", selectedGS)
	assert.Equal(t, "gs1", selectedGS.ObjectMeta.Name)
}

func TestControllerAllocationUpdateWorkers(t *testing.T) {
	t.Run("no error", func(t *testing.T) {
		c, m := newFakeController()

		updated := false
		gs1 := &stablev1alpha1.GameServer{
			ObjectMeta: metav1.ObjectMeta{Name: "gs1"},
		}
		r := response{
			request: request{
				gsa:      &allocationv1.GameServerAllocation{},
				response: make(chan response),
			},
			gs: gs1,
		}

		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			updated = true

			uo := action.(k8stesting.UpdateAction)
			gs := uo.GetObject().(*stablev1alpha1.GameServer)

			assert.Equal(t, gs1.ObjectMeta.Name, gs.ObjectMeta.Name)
			assert.Equal(t, stablev1alpha1.GameServerStateAllocated, gs.Status.State)

			return true, gs, nil
		})

		updateQueue := c.allocationUpdateWorkers(1)

		go func() {
			updateQueue <- r
		}()

		r = <-r.request.response

		assert.True(t, updated)
		assert.NoError(t, r.err)
		assert.Equal(t, gs1.ObjectMeta.Name, r.gs.ObjectMeta.Name)
		assert.Equal(t, stablev1alpha1.GameServerStateAllocated, r.gs.Status.State)

		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "Allocated")

		// make sure we can do more allocations than number of workers
		gs2 := &stablev1alpha1.GameServer{
			ObjectMeta: metav1.ObjectMeta{Name: "gs1"},
		}
		r = response{
			request: request{
				gsa:      &allocationv1.GameServerAllocation{},
				response: make(chan response),
			},
			gs: gs2,
		}

		go func() {
			updateQueue <- r
		}()

		r = <-r.request.response

		assert.True(t, updated)
		assert.NoError(t, r.err)
		assert.Equal(t, gs2.ObjectMeta.Name, r.gs.ObjectMeta.Name)
		assert.Equal(t, stablev1alpha1.GameServerStateAllocated, r.gs.Status.State)

		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "Allocated")
	})

	t.Run("error on update", func(t *testing.T) {
		c, m := newFakeController()

		updated := false
		gs1 := &stablev1alpha1.GameServer{
			ObjectMeta: metav1.ObjectMeta{Name: "gs1"},
		}
		key, err := cache.MetaNamespaceKeyFunc(gs1)
		assert.NoError(t, err)

		_, ok := c.readyGameServers.Load(key)
		assert.False(t, ok)

		r := response{
			request: request{
				gsa:      &allocationv1.GameServerAllocation{},
				response: make(chan response),
			},
			gs: gs1,
		}

		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			updated = true

			uo := action.(k8stesting.UpdateAction)
			gs := uo.GetObject().(*stablev1alpha1.GameServer)
			assert.Equal(t, gs1.ObjectMeta.Name, gs.ObjectMeta.Name)
			assert.Equal(t, stablev1alpha1.GameServerStateAllocated, gs.Status.State)

			return true, gs, errors.New("something went wrong")
		})

		updateQueue := c.allocationUpdateWorkers(1)

		go func() {
			updateQueue <- r
		}()

		r = <-r.request.response

		assert.True(t, updated)
		assert.Error(t, r.err)
		assert.Equal(t, gs1, r.gs)
		agtesting.AssertNoEvent(t, m.FakeRecorder.Events)

		var cached *stablev1alpha1.GameServer
		cached, ok = c.readyGameServers.Load(key)
		assert.True(t, ok)
		assert.Equal(t, gs1.ObjectMeta.Name, cached.ObjectMeta.Name)
	})
}

func TestControllerListSortedReadyGameServers(t *testing.T) {
	t.Parallel()

	gs1 := stablev1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "gs1", Namespace: defaultNs, UID: "1"}, Status: stablev1alpha1.GameServerStatus{NodeName: "node1", State: stablev1alpha1.GameServerStateReady}}
	gs2 := stablev1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "gs2", Namespace: defaultNs, UID: "2"}, Status: stablev1alpha1.GameServerStatus{NodeName: "node2", State: stablev1alpha1.GameServerStateReady}}
	gs3 := stablev1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "gs3", Namespace: defaultNs, UID: "3"}, Status: stablev1alpha1.GameServerStatus{NodeName: "node1", State: stablev1alpha1.GameServerStateAllocated}}
	gs4 := stablev1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "gs4", Namespace: defaultNs, UID: "4"}, Status: stablev1alpha1.GameServerStatus{NodeName: "node2", State: stablev1alpha1.GameServerStateReady}}

	fixtures := map[string]struct {
		list []stablev1alpha1.GameServer
		test func(*testing.T, []*stablev1alpha1.GameServer)
	}{
		"most allocated": {
			// node1: 1 ready, 1 allocated, node2: 1 ready
			list: []stablev1alpha1.GameServer{gs1, gs2, gs3},
			test: func(t *testing.T, list []*stablev1alpha1.GameServer) {
				assert.Len(t, list, 2)
				if !assert.Equal(t, []*stablev1alpha1.GameServer{&gs1, &gs2}, list) {
					for _, gs := range list {
						logrus.WithField("name", gs.Name).Info("game server")
					}
				}
			},
		},
		"list ready": {
			// node1: 1 ready, node2: 2 ready
			list: []stablev1alpha1.GameServer{gs1, gs2, gs4},
			test: func(t *testing.T, list []*stablev1alpha1.GameServer) {
				assert.Len(t, list, 3)
				assert.Contains(t, []string{"gs2", "gs4"}, list[0].ObjectMeta.Name)
				assert.Contains(t, []string{"gs2", "gs4"}, list[1].ObjectMeta.Name)
				assert.Equal(t, &gs1, list[2])
			},
		},
		"lexicographical (node name)": {
			list: []stablev1alpha1.GameServer{gs2, gs1},
			test: func(t *testing.T, list []*stablev1alpha1.GameServer) {
				assert.Len(t, list, 2)
				if !assert.Equal(t, []*stablev1alpha1.GameServer{&gs1, &gs2}, list) {
					for _, gs := range list {
						logrus.WithField("name", gs.Name).Info("game server")
					}
				}
			},
		},
	}

	for k, v := range fixtures {
		t.Run(k, func(t *testing.T) {
			c, m := newFakeController()

			gsList := v.list

			m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, &stablev1alpha1.GameServerList{Items: gsList}, nil
			})

			stop, cancel := agtesting.StartInformers(m, c.gameServerSynced)
			defer cancel()

			// This call initializes the cache
			err := c.syncReadyGSServerCache()
			assert.Nil(t, err)

			err = c.counter.Run(0, stop)
			assert.Nil(t, err)

			list := c.listSortedReadyGameServers()

			v.test(t, list)
		})
	}
}

func TestMultiClusterAllocationFromLocal(t *testing.T) {
	t.Parallel()
	t.Run("Handle allocation request locally", func(t *testing.T) {
		c, m := newFakeController()
		fleetName := addReactorForGameServer(&m)

		m.AgonesClient.AddReactor("list", "gameserverallocationpolicies", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &multiclusterv1alpha1.GameServerAllocationPolicyList{
				Items: []multiclusterv1alpha1.GameServerAllocationPolicy{
					{
						Spec: multiclusterv1alpha1.GameServerAllocationPolicySpec{
							Priority: 1,
							Weight:   200,
							ConnectionInfo: multiclusterv1alpha1.ClusterConnectionInfo{
								AllocationEndpoints: []string{"localhost"},
								ClusterName:         "multicluster",
								SecretName:          "localhostsecret",
							},
						},
						ObjectMeta: metav1.ObjectMeta{
							Labels:    map[string]string{"cluster": "onprem"},
							Namespace: defaultNs,
						},
					},
				},
			}, nil
		})

		stop, cancel := agtesting.StartInformers(m)
		defer cancel()

		go c.Run(1, stop) // nolint: errcheck
		// wait for it to be up and running
		err := wait.PollImmediate(time.Second, 10*time.Second, func() (done bool, err error) {
			return c.workerqueue.RunCount() == 1, nil
		})
		assert.NoError(t, err)

		gsa := &allocationv1.GameServerAllocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   defaultNs,
				Name:        "alloc1",
				ClusterName: "multicluster",
			},
			Spec: allocationv1.GameServerAllocationSpec{
				MultiClusterSetting: allocationv1.MultiClusterSetting{
					Enabled: true,
					PolicySelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"cluster": "onprem",
						},
					},
				},
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: fleetName}},
			},
		}

		ret, err := executeAllocation(gsa, c)
		assert.NoError(t, err)
		assert.Equal(t, gsa.Spec.Required, ret.Spec.Required)
		expectedState := allocationv1.GameServerAllocationAllocated
		assert.True(t, expectedState == ret.Status.State, "Failed: %s vs %s", expectedState, ret.Status.State)
	})

	t.Run("Missing multicluster policy", func(t *testing.T) {
		c, m := newFakeController()
		fleetName := addReactorForGameServer(&m)

		m.AgonesClient.AddReactor("list", "gameserverallocationpolicies", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &multiclusterv1alpha1.GameServerAllocationPolicyList{
				Items: []multiclusterv1alpha1.GameServerAllocationPolicy{},
			}, nil
		})

		stop, cancel := agtesting.StartInformers(m)
		defer cancel()

		go c.Run(1, stop) // nolint: errcheck
		// wait for it to be up and running
		err := wait.PollImmediate(time.Second, 10*time.Second, func() (done bool, err error) {
			return c.workerqueue.RunCount() == 1, nil
		})
		assert.NoError(t, err)

		gsa := &allocationv1.GameServerAllocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   defaultNs,
				Name:        "alloc1",
				ClusterName: "multicluster",
			},
			Spec: allocationv1.GameServerAllocationSpec{
				MultiClusterSetting: allocationv1.MultiClusterSetting{
					Enabled: true,
					PolicySelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"cluster": "onprem",
						},
					},
				},
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: fleetName}},
			},
		}

		_, err = executeAllocation(gsa, c)
		assert.Error(t, err)
	})
}

func TestMultiClusterAllocationFromRemote(t *testing.T) {
	const clusterName = "remotecluster"
	t.Parallel()
	t.Run("Handle allocation request remotely", func(t *testing.T) {
		c, m := newFakeController()
		fleetName := addReactorForGameServer(&m)

		// Mock server
		expectedGSAName := "mocked"
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serverResponse := allocationv1.GameServerAllocation{
				ObjectMeta: metav1.ObjectMeta{
					Name: expectedGSAName,
				},
			}
			response, _ := json.Marshal(serverResponse)
			_, _ = w.Write(response)
		}))
		defer server.Close()

		// Set client CA for server
		certpool := x509.NewCertPool()
		certpool.AppendCertsFromPEM(clientCert)
		server.TLS.ClientCAs = certpool
		server.TLS.ClientAuth = tls.RequireAndVerifyClientCert

		// Allocation policy reactor
		secretName := clusterName + "secret"
		m.AgonesClient.AddReactor("list", "gameserverallocationpolicies", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &multiclusterv1alpha1.GameServerAllocationPolicyList{
				Items: []multiclusterv1alpha1.GameServerAllocationPolicy{
					{
						Spec: multiclusterv1alpha1.GameServerAllocationPolicySpec{
							Priority: 1,
							Weight:   200,
							ConnectionInfo: multiclusterv1alpha1.ClusterConnectionInfo{
								AllocationEndpoints: []string{server.URL, "non-existing"},
								ClusterName:         clusterName,
								SecretName:          secretName,
							},
						},
						ObjectMeta: metav1.ObjectMeta{
							Namespace: defaultNs,
						},
					},
				},
			}, nil
		})

		m.KubeClient.AddReactor("list", "secrets",
			func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, getTestSecret(secretName, server.TLS.Certificates[0].Certificate[0]), nil
			})

		stop, cancel := agtesting.StartInformers(m, c.allocationPolicySynced, c.secretSynced, c.gameServerSynced)
		defer cancel()

		// This call initializes the cache
		err := c.syncReadyGSServerCache()
		assert.Nil(t, err)

		err = c.counter.Run(0, stop)
		assert.Nil(t, err)

		gsa := &allocationv1.GameServerAllocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   defaultNs,
				Name:        "alloc1",
				ClusterName: "localcluster",
			},
			Spec: allocationv1.GameServerAllocationSpec{
				MultiClusterSetting: allocationv1.MultiClusterSetting{
					Enabled: true,
				},
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: fleetName}},
			},
		}

		result, err := executeAllocation(gsa, c)
		if assert.NoError(t, err) {
			assert.Equal(t, expectedGSAName, result.ObjectMeta.Name)
		}
	})

	t.Run("Remote server returns error", func(t *testing.T) {
		c, m := newFakeController()
		fleetName := addReactorForGameServer(&m)

		// Mock server to return error
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "test error message", 500)
		}))
		defer server.Close()

		// Set client CA for server
		certpool := x509.NewCertPool()
		certpool.AppendCertsFromPEM(clientCert)
		server.TLS.ClientCAs = certpool
		server.TLS.ClientAuth = tls.RequireAndVerifyClientCert

		// Allocation policy reactor
		secretName := clusterName + "secret"
		m.AgonesClient.AddReactor("list", "gameserverallocationpolicies", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &multiclusterv1alpha1.GameServerAllocationPolicyList{
				Items: []multiclusterv1alpha1.GameServerAllocationPolicy{
					{
						Spec: multiclusterv1alpha1.GameServerAllocationPolicySpec{
							Priority: 1,
							Weight:   200,
							ConnectionInfo: multiclusterv1alpha1.ClusterConnectionInfo{
								AllocationEndpoints: []string{server.URL},
								ClusterName:         clusterName,
								SecretName:          secretName,
							},
						},
						ObjectMeta: metav1.ObjectMeta{
							Namespace: defaultNs,
						},
					},
				},
			}, nil
		})

		m.KubeClient.AddReactor("list", "secrets",
			func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, getTestSecret(secretName, server.TLS.Certificates[0].Certificate[0]), nil
			})

		stop, cancel := agtesting.StartInformers(m, c.allocationPolicySynced, c.secretSynced, c.gameServerSynced)
		defer cancel()

		// This call initializes the cache
		err := c.syncReadyGSServerCache()
		assert.Nil(t, err)

		err = c.counter.Run(0, stop)
		assert.Nil(t, err)

		gsa := &allocationv1.GameServerAllocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   defaultNs,
				Name:        "alloc1",
				ClusterName: "localcluster",
			},
			Spec: allocationv1.GameServerAllocationSpec{
				MultiClusterSetting: allocationv1.MultiClusterSetting{
					Enabled: true,
				},
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: fleetName}},
			},
		}

		_, err = executeAllocation(gsa, c)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "test error message")
	})

	t.Run("First server fails and second server succeeds", func(t *testing.T) {
		c, m := newFakeController()
		fleetName := addReactorForGameServer(&m)

		// Mock server to return error
		unhealthyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "test error message", 500)
		}))
		defer unhealthyServer.Close()

		// Set client CA for unhealthy server
		certpool := x509.NewCertPool()
		certpool.AppendCertsFromPEM(clientCert)
		unhealthyServer.TLS.ClientCAs = certpool
		unhealthyServer.TLS.ClientAuth = tls.RequireAndVerifyClientCert

		// Mock healthyServer
		expectedGSAName := "mocked"
		healthyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serverResponse := allocationv1.GameServerAllocation{
				ObjectMeta: metav1.ObjectMeta{
					Name: expectedGSAName,
				},
			}
			response, _ := json.Marshal(serverResponse)
			_, _ = w.Write(response)
		}))
		defer healthyServer.Close()
		healthyServer.TLS = unhealthyServer.TLS

		// Allocation policy reactor
		secretName := clusterName + "secret"
		m.AgonesClient.AddReactor("list", "gameserverallocationpolicies", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &multiclusterv1alpha1.GameServerAllocationPolicyList{
				Items: []multiclusterv1alpha1.GameServerAllocationPolicy{
					{
						Spec: multiclusterv1alpha1.GameServerAllocationPolicySpec{
							Priority: 1,
							Weight:   200,
							ConnectionInfo: multiclusterv1alpha1.ClusterConnectionInfo{
								AllocationEndpoints: []string{unhealthyServer.URL, healthyServer.URL},
								ClusterName:         clusterName,
								SecretName:          secretName,
							},
						},
						ObjectMeta: metav1.ObjectMeta{
							Namespace: defaultNs,
						},
					},
				},
			}, nil
		})

		m.KubeClient.AddReactor("list", "secrets",
			func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, getTestSecret(secretName, unhealthyServer.TLS.Certificates[0].Certificate[0]), nil
			})

		stop, cancel := agtesting.StartInformers(m, c.allocationPolicySynced, c.secretSynced, c.gameServerSynced)
		defer cancel()

		// This call initializes the cache
		err := c.syncReadyGSServerCache()
		assert.Nil(t, err)

		err = c.counter.Run(0, stop)
		assert.Nil(t, err)

		gsa := &allocationv1.GameServerAllocation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   defaultNs,
				Name:        "alloc1",
				ClusterName: "localcluster",
			},
			Spec: allocationv1.GameServerAllocationSpec{
				MultiClusterSetting: allocationv1.MultiClusterSetting{
					Enabled: true,
				},
				Required: metav1.LabelSelector{MatchLabels: map[string]string{stablev1alpha1.FleetNameLabel: fleetName}},
			},
		}

		result, err := executeAllocation(gsa, c)
		if assert.NoError(t, err) {
			assert.Equal(t, expectedGSAName, result.ObjectMeta.Name)
		}
	})
}

func TestCreateRestClientError(t *testing.T) {
	t.Parallel()
	t.Run("Missing secret", func(t *testing.T) {
		c, _ := newFakeController()
		_, err := c.createRemoteClusterRestClient(defaultNs, "secret-name")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "secret-name")
	})
	t.Run("Missing cert", func(t *testing.T) {
		c, m := newFakeController()

		m.KubeClient.AddReactor("list", "secrets",
			func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, &corev1.SecretList{
					Items: []corev1.Secret{{
						Data: map[string][]byte{
							"tls.crt": clientCert,
						},
						ObjectMeta: metav1.ObjectMeta{
							Name:      "secret-name",
							Namespace: defaultNs,
						},
					}}}, nil
			})

		_, cancel := agtesting.StartInformers(m, c.secretSynced)
		defer cancel()

		_, err := c.createRemoteClusterRestClient(defaultNs, "secret-name")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing client certificate key pair in secret secret-name")
	})
	t.Run("Bad client cert", func(t *testing.T) {
		c, m := newFakeController()

		m.KubeClient.AddReactor("list", "secrets",
			func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, &corev1.SecretList{
					Items: []corev1.Secret{{
						Data: map[string][]byte{
							"tls.crt": []byte("XXX"),
							"tls.key": []byte("XXX"),
						},
						ObjectMeta: metav1.ObjectMeta{
							Name:      "secret-name",
							Namespace: defaultNs,
						},
					}}}, nil
			})

		_, cancel := agtesting.StartInformers(m, c.secretSynced)
		defer cancel()

		_, err := c.createRemoteClusterRestClient(defaultNs, "secret-name")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to find any PEM data in certificate input")
	})
	t.Run("Bad CA cert", func(t *testing.T) {
		c, m := newFakeController()

		m.KubeClient.AddReactor("list", "secrets",
			func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, getTestSecret("secret-name", []byte("XXX")), nil
			})

		_, cancel := agtesting.StartInformers(m, c.secretSynced)
		defer cancel()

		_, err := c.createRemoteClusterRestClient(defaultNs, "secret-name")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "certificate")
	})
}

func executeAllocation(gsa *allocationv1.GameServerAllocation, c *Controller) (*allocationv1.GameServerAllocation, error) {
	r, err := createRequest(gsa)
	if err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	if err = c.allocationHandler(rec, r, defaultNs); err != nil {
		return nil, err
	}

	ret := &allocationv1.GameServerAllocation{}
	err = json.Unmarshal(rec.Body.Bytes(), ret)
	return ret, err
}

func addReactorForGameServer(m *agtesting.Mocks) string {
	f, _, gsList := defaultFixtures(3)
	gsWatch := watch.NewFake()
	m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(gsWatch, nil))
	m.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, &stablev1alpha1.GameServerList{Items: gsList}, nil
	})
	m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		ua := action.(k8stesting.UpdateAction)
		gs := ua.GetObject().(*stablev1alpha1.GameServer)
		gsWatch.Modify(gs)
		return true, gs, nil
	})
	return f.ObjectMeta.Name
}

func createRequest(gsa *allocationv1.GameServerAllocation) (*http.Request, error) {
	buf := bytes.NewBuffer(nil)
	if err := json.NewEncoder(buf).Encode(gsa); err != nil {
		return nil, err
	}

	r, err := http.NewRequest(http.MethodPost, "/", buf)
	r.Header.Set("Content-Type", k8sruntime.ContentTypeJSON)

	if err != nil {
		return nil, err
	}
	return r, nil
}

func defaultFixtures(gsLen int) (*stablev1alpha1.Fleet, *stablev1alpha1.GameServerSet, []stablev1alpha1.GameServer) {
	f := &stablev1alpha1.Fleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fleet-1",
			Namespace: defaultNs,
			UID:       "1234",
		},
		Spec: stablev1alpha1.FleetSpec{
			Replicas: 5,
			Template: stablev1alpha1.GameServerTemplateSpec{},
		},
	}
	f.ApplyDefaults()
	gsSet := f.GameServerSet()
	gsSet.ObjectMeta.Name = "gsSet1"
	var gsList []stablev1alpha1.GameServer
	for i := 1; i <= gsLen; i++ {
		gs := gsSet.GameServer()
		gs.ObjectMeta.Name = "gs" + strconv.Itoa(i)
		gs.Status.State = stablev1alpha1.GameServerStateReady
		gsList = append(gsList, *gs)
	}
	return f, gsSet, gsList
}

// newFakeController returns a controller, backed by the fake Clientset
func newFakeController() (*Controller, agtesting.Mocks) {
	m := agtesting.NewMocks()
	m.Mux = http.NewServeMux()
	counter := gameservers.NewPerNodeCounter(m.KubeInformerFactory, m.AgonesInformerFactory)
	api := apiserver.NewAPIServer(m.Mux)
	c := NewController(api, healthcheck.NewHandler(), counter, 1, m.KubeClient, m.KubeInformerFactory, m.AgonesClient, m.AgonesInformerFactory)
	c.recorder = m.FakeRecorder
	return c, m
}

func getTestSecret(secretName string, serverCert []byte) *corev1.SecretList {
	return &corev1.SecretList{
		Items: []corev1.Secret{
			{
				Data: map[string][]byte{
					"ca.crt":  serverCert,
					"tls.key": clientKey,
					"tls.crt": clientCert,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: defaultNs,
				},
			},
		},
	}
}

var clientCert = []byte(`-----BEGIN CERTIFICATE-----
MIIDuzCCAqOgAwIBAgIUduDWtqpUsp3rZhCEfUrzI05laVIwDQYJKoZIhvcNAQEL
BQAwbTELMAkGA1UEBhMCR0IxDzANBgNVBAgMBkxvbmRvbjEPMA0GA1UEBwwGTG9u
ZG9uMRgwFgYDVQQKDA9HbG9iYWwgU2VjdXJpdHkxFjAUBgNVBAsMDUlUIERlcGFy
dG1lbnQxCjAIBgNVBAMMASowHhcNMTkwNTAyMjIzMDQ3WhcNMjkwNDI5MjIzMDQ3
WjBtMQswCQYDVQQGEwJHQjEPMA0GA1UECAwGTG9uZG9uMQ8wDQYDVQQHDAZMb25k
b24xGDAWBgNVBAoMD0dsb2JhbCBTZWN1cml0eTEWMBQGA1UECwwNSVQgRGVwYXJ0
bWVudDEKMAgGA1UEAwwBKjCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEB
AKGDasjadVwe0bXUEQfZCkMEAkzn0qTud3RYytympmaS0c01SWFNZwPRO0rpdIOZ
fyXVXVOAhgmgCR6QuXySmyQIoYl/D6tVhc5r9FyWPIBtzQKCJTX0mZOZwMn22qvo
bfnDnVsZ1Ny3RLZIF3um3xovvePXyg1z7D/NvCogNuYpyUUEITPZX6ss5ods/U78
BxLhKrT8iyu61ZC+ZegbHQqFRngbeb348gE1JwKTslDfe4oH7tZ+bNDZxnGcvh9j
eyagpM0zys4gFfQf/vfD2aEsUJ+GesUQC6uGVoGnTFshFhBsAK6vpIQ4ZQujaJ0r
NKgJ/ccBJFiJXMCR44yWFY0CAwEAAaNTMFEwHQYDVR0OBBYEFEe1gDd8JpzgnvOo
1AEloAXxmxHCMB8GA1UdIwQYMBaAFEe1gDd8JpzgnvOo1AEloAXxmxHCMA8GA1Ud
EwEB/wQFMAMBAf8wDQYJKoZIhvcNAQELBQADggEBAI5GyuakVgunerCGCSN7Ghsr
ys9vJytbyT+BLmxNBPSXWQwcm3g9yCDdgf0Y3q3Eef7IEZu4I428318iLzhfKln1
ua4fxvmTFKJ65lQKNkc6Y4e3w1t+C2HOl6fOIVT231qsCoM5SAwQQpqAzEUj6kZl
x+3avw9KSlXqR/mCAkePyoKvprxeb6RVDdq92Ug0qzoAHLpvIkuHdlF0dNp6/kO0
1pVL0BqW+6UTimSSvH8F/cMeYKbkhpE1u2c/NtNwsR2jN4M9kl3KHqkynk67PfZv
pwlCqZx4M8FpdfCbOZeRLzClUBdD5qzev0L3RNUx7UJzEIN+4LCBv37DIojNOyA=
-----END CERTIFICATE-----`)

var clientKey = []byte(`-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQChg2rI2nVcHtG1
1BEH2QpDBAJM59Kk7nd0WMrcpqZmktHNNUlhTWcD0TtK6XSDmX8l1V1TgIYJoAke
kLl8kpskCKGJfw+rVYXOa/RcljyAbc0CgiU19JmTmcDJ9tqr6G35w51bGdTct0S2
SBd7pt8aL73j18oNc+w/zbwqIDbmKclFBCEz2V+rLOaHbP1O/AcS4Sq0/IsrutWQ
vmXoGx0KhUZ4G3m9+PIBNScCk7JQ33uKB+7WfmzQ2cZxnL4fY3smoKTNM8rOIBX0
H/73w9mhLFCfhnrFEAurhlaBp0xbIRYQbACur6SEOGULo2idKzSoCf3HASRYiVzA
keOMlhWNAgMBAAECggEAaRPDjEq8IaOXUdFXByEIERNxn7EOlOjj5FjEGgt9pKwO
PJBXXitqQsyD47fAasGZO/b1EZdDHM32QOFtG4OR1T6cQYTdn90zAVmwj+/aCr/k
qaYcKV8p7yIPkBW+rCq6Kc0++X7zwmilFmYOiQ7GhRXcV3gTZu8tG1FxAoMU1GYA
WoGiu+UsEm0MFIOwV/DOukDaj6j4Q9wD0tqi2MsjrugjDI8/mSx5mlvo3yZHubl0
ChQaWZyUlL2B40mQJc3qsRZzso3sbU762L6G6npQJ19dHgsBfBBs/Q4/DdeqcOb4
Q9OZ8Q3Q5nXQ7359Sh94LvLOoaWecRTBPGaRvGAGLQKBgQDTOZPEaJJI9heUQ0Ar
VvUuyjILv8CG+PV+rGZ7+yMFCUlmM/m9I0IIc3WbmxxiRypBv46zxpczQHwWZRf2
7IUZdyrBXRtNoaXbWh3dSgqa7WuHGUzqmn+98sQDodewCyGon8LG9atyge8vFo/l
N0Y21duYj4NeJod82Y0RAKsuzwKBgQDDwCuvbq0FkugklUr5WLFrYTzWrTYPio5k
ID6Ku57yaZNVRv52FTF3Ac5LoKGCv8iPg+x0SiTmCbW2DF2ohvTuJy1H/unJ4bYG
B9vEVOiScbvrvuQ6iMgfxNUCEEQvmn6+uc+KHVwPixY4j6/q1ZLXLPbjqXYHPYi+
lx9ZG0As4wKBgDj52QAr7Pm9WBLoKREHvc9HP0SoDrjZwu7Odj6POZ0MKj5lWsJI
FnHNIzY8GuXvqFhf4ZBgyzxJ8q7fyh0TI7wAxwmtocXJCsImhtPAOygbTtv8WSEX
V8nXCESqjVGxTvz7S0D716llny0met4rkMcN3NREMf1di0KENGcXtRVFAoGBAKs3
bD5/NNF6RJizCKf+fvjoTVmMmYuQaqmDVpDsOMPZumfNuAa61NA+AR4/OuXtL9Tv
1COHMq0O8yRvvoAIwzWHiOC/Q+g0B41Q1FXu2po05uT1zBSyzTCUbqfmaG2m2ZOj
XLd2pK5nvqDsdTeXZV/WUYCiGb2Ngg0Ki/3ZixF3AoGACwPxxoAWkuD6T++35Vdt
OxAh/qyGMtgfvdBJPfA3u4digTckBDTwYBhrmvC2Vuc4cpb15RYuUT/M+c3gS3P0
q+2uLIuwciETPD7psK76NsQM3ZL/IEaZB3VMxbMMFn/NQRbmntTd/twZ42zieX+R
2VpXYUjoRcuir2oU0wh3Hic=
-----END PRIVATE KEY-----`)
