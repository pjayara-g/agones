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

package gameservers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"agones.dev/agones/pkg/apis/stable"
	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	agtesting "agones.dev/agones/pkg/testing"
	"agones.dev/agones/pkg/util/webhooks"
	"github.com/heptiolabs/healthcheck"
	"github.com/mattbaird/jsonpatch"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

const (
	ipFixture       = "12.12.12.12"
	nodeFixtureName = "node1"
)

var (
	GameServerKind = metav1.GroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("GameServer"))
)

func TestControllerSyncGameServer(t *testing.T) {
	t.Parallel()

	t.Run("Creating a new GameServer", func(t *testing.T) {
		c, mocks := newFakeController()
		updateCount := 0
		podCreated := false
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: v1alpha1.GameServerSpec{
				Ports: []v1alpha1.GameServerPort{{ContainerPort: 7777}},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "container", Image: "container/image"}},
				},
				},
			},
		}

		node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Address: ipFixture, Type: corev1.NodeExternalIP}}}}

		fixture.ApplyDefaults()

		watchPods := watch.NewFake()
		mocks.KubeClient.AddWatchReactor("pods", k8stesting.DefaultWatchReactor(watchPods, nil))

		mocks.KubeClient.AddReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.NodeList{Items: []corev1.Node{node}}, nil
		})
		mocks.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			ca := action.(k8stesting.CreateAction)
			pod := ca.GetObject().(*corev1.Pod)
			pod.Spec.NodeName = node.ObjectMeta.Name
			podCreated = true
			assert.Equal(t, fixture.ObjectMeta.Name, pod.ObjectMeta.Name)
			watchPods.Add(pod)
			// wait for the change to propagate
			assert.True(t, cache.WaitForCacheSync(context.Background().Done(), mocks.KubeInformerFactory.Core().V1().Pods().Informer().HasSynced))
			return true, pod, nil
		})
		mocks.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gameServers := &v1alpha1.GameServerList{Items: []v1alpha1.GameServer{*fixture}}
			return true, gameServers, nil
		})
		mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			updateCount++
			expectedState := v1alpha1.GameServerState("notastate")
			switch updateCount {
			case 1:
				expectedState = v1alpha1.GameServerStateCreating
			case 2:
				expectedState = v1alpha1.GameServerStateStarting
			case 3:
				expectedState = v1alpha1.GameServerStateScheduled
			}

			assert.Equal(t, expectedState, gs.Status.State)
			if expectedState == v1alpha1.GameServerStateScheduled {
				assert.Equal(t, ipFixture, gs.Status.Address)
				assert.NotEmpty(t, gs.Status.Ports[0].Port)
			}

			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced, c.portAllocator.nodeSynced)
		defer cancel()

		err := c.portAllocator.syncAll()
		assert.Nil(t, err)

		err = c.syncGameServer("default/test")
		assert.Nil(t, err)
		assert.Equal(t, 3, updateCount, "update reactor should fire thrice")
		assert.True(t, podCreated, "pod should be created")
	})

	t.Run("When a GameServer has been deleted, the sync operation should be a noop", func(t *testing.T) {
		runReconcileDeleteGameServer(t, &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec:   newSingleContainerSpec(),
			Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateReady}})
	})
}

func runReconcileDeleteGameServer(t *testing.T, fixture *v1alpha1.GameServer) {
	c, mocks := newFakeController()
	agonesWatch := watch.NewFake()
	podAction := false

	mocks.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(agonesWatch, nil))
	mocks.KubeClient.AddReactor("*", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() == "update" || action.GetVerb() == "delete" || action.GetVerb() == "create" || action.GetVerb() == "patch" {
			podAction = true
		}
		return false, nil, nil
	})

	_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced)
	defer cancel()

	agonesWatch.Delete(fixture)

	err := c.syncGameServer("default/test")
	assert.Nil(t, err, fmt.Sprintf("Shouldn't be an error from syncGameServer: %+v", err))
	assert.False(t, podAction, "Nothing should happen to a Pod")
}
func TestControllerSyncGameServerWithDevIP(t *testing.T) {
	t.Parallel()

	templateDevGs := &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Namespace:   "default",
			Annotations: map[string]string{v1alpha1.DevAddressAnnotation: ipFixture},
		},
		Spec: v1alpha1.GameServerSpec{
			Ports: []v1alpha1.GameServerPort{{ContainerPort: 7777, HostPort: 7777, PortPolicy: v1alpha1.Static}},
		},
	}

	t.Run("Creating a new GameServer", func(t *testing.T) {
		c, mocks := newFakeController()
		updateCount := 0

		fixture := templateDevGs.DeepCopy()

		fixture.ApplyDefaults()

		mocks.KubeClient.AddReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return false, nil, k8serrors.NewMethodNotSupported(schema.GroupResource{}, "list nodes should not be called")
		})
		mocks.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return false, nil, k8serrors.NewMethodNotSupported(schema.GroupResource{}, "creating a pod with dev mode is not supported")
		})
		mocks.AgonesClient.AddReactor("list", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gameServers := &v1alpha1.GameServerList{Items: []v1alpha1.GameServer{*fixture}}
			return true, gameServers, nil
		})
		mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			updateCount++
			expectedState := v1alpha1.GameServerStateReady

			assert.Equal(t, expectedState, gs.Status.State)
			if expectedState == v1alpha1.GameServerStateReady {
				assert.Equal(t, ipFixture, gs.Status.Address)
				assert.NotEmpty(t, gs.Status.Ports[0].Port)
			}

			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced, c.portAllocator.nodeSynced)
		defer cancel()

		err := c.portAllocator.syncAll()
		assert.Nil(t, err)

		err = c.syncGameServer("default/test")
		assert.Nil(t, err)
		assert.Equal(t, 1, updateCount, "update reactor should fire once")
	})

	t.Run("When a GameServer has been deleted, the sync operation should be a noop", func(t *testing.T) {
		runReconcileDeleteGameServer(t, &v1alpha1.GameServer{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test",
				Namespace:   "default",
				Annotations: map[string]string{v1alpha1.DevAddressAnnotation: ipFixture},
			},
			Spec: v1alpha1.GameServerSpec{
				Ports: []v1alpha1.GameServerPort{{ContainerPort: 7777, HostPort: 7777, PortPolicy: v1alpha1.Static}},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "container", Image: "container/image"}},
				},
				},
			},
		})
	})
}

func TestControllerWatchGameServers(t *testing.T) {
	c, m := newFakeController()
	fixture := v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"}, Spec: newSingleContainerSpec()}
	fixture.ApplyDefaults()
	pod, err := fixture.Pod()
	assert.Nil(t, err)
	pod.ObjectMeta.Name = pod.ObjectMeta.GenerateName + "-pod"

	gsWatch := watch.NewFake()
	podWatch := watch.NewFake()
	m.AgonesClient.AddWatchReactor("gameservers", k8stesting.DefaultWatchReactor(gsWatch, nil))
	m.KubeClient.AddWatchReactor("pods", k8stesting.DefaultWatchReactor(podWatch, nil))
	m.ExtClient.AddReactor("get", "customresourcedefinitions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, agtesting.NewEstablishedCRD(), nil
	})

	received := make(chan string)
	defer close(received)

	h := func(name string) error {
		assert.Equal(t, "default/test", name)
		received <- name
		return nil
	}

	c.workerqueue.SyncHandler = h
	c.creationWorkerQueue.SyncHandler = h
	c.deletionWorkerQueue.SyncHandler = h

	stop, cancel := agtesting.StartInformers(m, c.gameServerSynced)
	defer cancel()

	noStateChange := func(sync cache.InformerSynced) {
		cache.WaitForCacheSync(stop, sync)
		select {
		case <-received:
			assert.Fail(t, "Should not be queued")
		default:
		}
	}

	podSynced := m.KubeInformerFactory.Core().V1().Pods().Informer().HasSynced
	gsSynced := m.AgonesInformerFactory.Stable().V1alpha1().GameServers().Informer().HasSynced

	go func() {
		err := c.Run(1, stop)
		assert.Nil(t, err, "Run should not error")
	}()

	logrus.Info("Adding first fixture")
	gsWatch.Add(&fixture)
	assert.Equal(t, "default/test", <-received)
	podWatch.Add(pod)
	noStateChange(podSynced)

	// no state change
	gsWatch.Modify(&fixture)
	noStateChange(gsSynced)

	// add a non game pod
	nonGamePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"}}
	podWatch.Add(nonGamePod)
	noStateChange(podSynced)

	// no state change
	gsWatch.Modify(&fixture)
	noStateChange(gsSynced)

	// no state change
	gsWatch.Modify(&fixture)
	noStateChange(gsSynced)

	copyFixture := fixture.DeepCopy()
	copyFixture.Status.State = v1alpha1.GameServerStateStarting
	logrus.Info("modify copyFixture")
	gsWatch.Modify(copyFixture)
	assert.Equal(t, "default/test", <-received)

	podWatch.Delete(pod)
	assert.Equal(t, "default/test", <-received)

	// add an unscheduled game pod
	pod, err = fixture.Pod()
	assert.Nil(t, err)
	pod.ObjectMeta.Name = pod.ObjectMeta.GenerateName + "-pod2"
	podWatch.Add(pod)
	noStateChange(podSynced)

	// schedule it
	podCopy := pod.DeepCopy()
	podCopy.Spec.NodeName = nodeFixtureName

	podWatch.Modify(podCopy)
	assert.Equal(t, "default/test", <-received)
}

func TestControllerCreationMutationHandler(t *testing.T) {
	t.Parallel()

	c, _ := newFakeController()

	fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: newSingleContainerSpec()}

	raw, err := json.Marshal(fixture)
	assert.Nil(t, err)
	review := admv1beta1.AdmissionReview{
		Request: &admv1beta1.AdmissionRequest{
			Kind:      GameServerKind,
			Operation: admv1beta1.Create,
			Object: runtime.RawExtension{
				Raw: raw,
			},
		},
		Response: &admv1beta1.AdmissionResponse{Allowed: true},
	}

	result, err := c.creationMutationHandler(review)
	assert.Nil(t, err)
	assert.True(t, result.Response.Allowed)
	assert.Equal(t, admv1beta1.PatchTypeJSONPatch, *result.Response.PatchType)

	patch := &jsonpatch.ByPath{}
	err = json.Unmarshal(result.Response.Patch, patch)
	assert.Nil(t, err)

	assertContains := func(patch *jsonpatch.ByPath, op jsonpatch.JsonPatchOperation) {
		found := false
		for _, p := range *patch {
			if assert.ObjectsAreEqualValues(p, op) {
				found = true
			}
		}

		assert.True(t, found, "Could not find operation %#v in patch %v", op, *patch)
	}

	assertContains(patch, jsonpatch.JsonPatchOperation{Operation: "add", Path: "/metadata/finalizers", Value: []interface{}{"stable.agones.dev"}})
	assertContains(patch, jsonpatch.JsonPatchOperation{Operation: "add", Path: "/spec/ports/0/protocol", Value: "UDP"})
}

func TestControllerCreationValidationHandler(t *testing.T) {
	t.Parallel()

	c, _ := newFakeController()

	t.Run("valid gameserver", func(t *testing.T) {
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: newSingleContainerSpec()}
		fixture.ApplyDefaults()

		raw, err := json.Marshal(fixture)
		assert.Nil(t, err)
		review := admv1beta1.AdmissionReview{
			Request: &admv1beta1.AdmissionRequest{
				Kind:      GameServerKind,
				Operation: admv1beta1.Create,
				Object: runtime.RawExtension{
					Raw: raw,
				},
			},
			Response: &admv1beta1.AdmissionResponse{Allowed: true},
		}

		result, err := c.creationValidationHandler(review)
		assert.Nil(t, err)
		assert.True(t, result.Response.Allowed)
	})

	t.Run("invalid gameserver", func(t *testing.T) {
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: v1alpha1.GameServerSpec{
				Container: "NOPE!",
				Ports:     []v1alpha1.GameServerPort{{ContainerPort: 7777}},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "container", Image: "container/image"},
							{Name: "container2", Image: "container/image"},
						},
					},
				},
			},
		}
		raw, err := json.Marshal(fixture)
		assert.Nil(t, err)
		review := admv1beta1.AdmissionReview{
			Request: &admv1beta1.AdmissionRequest{
				Kind:      GameServerKind,
				Operation: admv1beta1.Create,
				Object: runtime.RawExtension{
					Raw: raw,
				},
			},
			Response: &admv1beta1.AdmissionResponse{Allowed: true},
		}

		result, err := c.creationValidationHandler(review)
		assert.Nil(t, err)
		assert.False(t, result.Response.Allowed)
		assert.Equal(t, metav1.StatusFailure, review.Response.Result.Status)
		assert.Equal(t, metav1.StatusReasonInvalid, review.Response.Result.Reason)
		assert.Equal(t, review.Request.Kind.Kind, result.Response.Result.Details.Kind)
		assert.Equal(t, review.Request.Kind.Group, result.Response.Result.Details.Group)
		assert.NotEmpty(t, result.Response.Result.Details.Causes)
	})
}

func TestControllerSyncGameServerDeletionTimestamp(t *testing.T) {
	t.Parallel()

	t.Run("GameServer has a Pod", func(t *testing.T) {
		c, mocks := newFakeController()
		now := metav1.Now()
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", DeletionTimestamp: &now},
			Spec: newSingleContainerSpec()}
		fixture.ApplyDefaults()
		pod, err := fixture.Pod()
		assert.Nil(t, err)

		deleted := false
		mocks.KubeClient.AddReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PodList{Items: []corev1.Pod{*pod}}, nil
		})
		mocks.KubeClient.AddReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			deleted = true
			da := action.(k8stesting.DeleteAction)
			assert.Equal(t, pod.ObjectMeta.Name, da.GetName())
			return true, nil, nil
		})

		_, cancel := agtesting.StartInformers(mocks, c.podSynced)
		defer cancel()

		result, err := c.syncGameServerDeletionTimestamp(fixture)
		assert.NoError(t, err)
		assert.True(t, deleted, "pod should be deleted")
		assert.Equal(t, fixture, result)
		agtesting.AssertEventContains(t, mocks.FakeRecorder.Events, fmt.Sprintf("%s %s %s", corev1.EventTypeNormal,
			fixture.Status.State, "Deleting Pod "+pod.ObjectMeta.Name))
	})

	t.Run("GameServer's Pods have been deleted", func(t *testing.T) {
		c, mocks := newFakeController()
		now := metav1.Now()
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", DeletionTimestamp: &now},
			Spec: newSingleContainerSpec()}
		fixture.ApplyDefaults()

		updated := false
		mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true

			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, fixture.ObjectMeta.Name, gs.ObjectMeta.Name)
			assert.Empty(t, gs.ObjectMeta.Finalizers)

			return true, gs, nil
		})
		_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced)
		defer cancel()

		result, err := c.syncGameServerDeletionTimestamp(fixture)
		assert.Nil(t, err)
		assert.True(t, updated, "gameserver should be updated, to remove the finaliser")
		assert.Equal(t, fixture.ObjectMeta.Name, result.ObjectMeta.Name)
		assert.Empty(t, result.ObjectMeta.Finalizers)
	})

	t.Run("Local development GameServer", func(t *testing.T) {
		c, mocks := newFakeController()
		now := metav1.Now()
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default",
			Annotations:       map[string]string{v1alpha1.DevAddressAnnotation: "1.1.1.1"},
			DeletionTimestamp: &now},
			Spec: newSingleContainerSpec()}
		fixture.ApplyDefaults()

		updated := false
		mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true

			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, fixture.ObjectMeta.Name, gs.ObjectMeta.Name)
			assert.Empty(t, gs.ObjectMeta.Finalizers)

			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced)
		defer cancel()

		result, err := c.syncGameServerDeletionTimestamp(fixture)
		assert.Nil(t, err)
		assert.True(t, updated, "gameserver should be updated, to remove the finaliser")
		assert.Equal(t, fixture.ObjectMeta.Name, result.ObjectMeta.Name)
		assert.Empty(t, result.ObjectMeta.Finalizers)
	})
}

func TestControllerSyncGameServerPortAllocationState(t *testing.T) {
	t.Parallel()

	t.Run("Gameserver with port allocation state", func(t *testing.T) {
		t.Parallel()
		c, mocks := newFakeController()
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: v1alpha1.GameServerSpec{
				Ports: []v1alpha1.GameServerPort{{ContainerPort: 7777}},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "container", Image: "container/image"}},
					},
				},
			},
			Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStatePortAllocation},
		}
		fixture.ApplyDefaults()
		mocks.KubeClient.AddReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.NodeList{Items: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName}}}}, nil
		})

		updated := false

		mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, fixture.ObjectMeta.Name, gs.ObjectMeta.Name)
			port := gs.Spec.Ports[0]
			assert.Equal(t, v1alpha1.Dynamic, port.PortPolicy)
			assert.NotEqual(t, fixture.Spec.Ports[0].HostPort, port.HostPort)
			assert.True(t, 10 <= port.HostPort && port.HostPort <= 20, "%s not in range", port.HostPort)

			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced, c.portAllocator.nodeSynced)
		defer cancel()
		err := c.portAllocator.syncAll()
		assert.Nil(t, err)

		result, err := c.syncGameServerPortAllocationState(fixture)
		assert.Nil(t, err, "sync should not error")
		assert.True(t, updated, "update should occur")
		port := result.Spec.Ports[0]
		assert.Equal(t, v1alpha1.Dynamic, port.PortPolicy)
		assert.NotEqual(t, fixture.Spec.Ports[0].HostPort, port.HostPort)
		assert.True(t, 10 <= port.HostPort && port.HostPort <= 20, "%s not in range", port.HostPort)
	})

	t.Run("Gameserver with unknown state", func(t *testing.T) {
		testNoChange(t, "Unknown", func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return c.syncGameServerPortAllocationState(fixture)
		})
	})

	t.Run("GameServer with non zero deletion datetime", func(t *testing.T) {
		testWithNonZeroDeletionTimestamp(t, func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return c.syncGameServerPortAllocationState(fixture)
		})
	})
}

func TestControllerSyncGameServerCreatingState(t *testing.T) {
	t.Parallel()

	newFixture := func() *v1alpha1.GameServer {
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateCreating}}
		fixture.ApplyDefaults()
		return fixture
	}

	t.Run("Syncing from Created State, with no issues", func(t *testing.T) {
		c, m := newFakeController()
		fixture := newFixture()
		podCreated := false
		gsUpdated := false

		var pod *corev1.Pod
		m.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			podCreated = true
			ca := action.(k8stesting.CreateAction)
			pod = ca.GetObject().(*corev1.Pod)
			assert.True(t, metav1.IsControlledBy(pod, fixture))
			return true, pod, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gsUpdated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, v1alpha1.GameServerStateStarting, gs.Status.State)
			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(m, c.gameServerSynced)
		defer cancel()

		gs, err := c.syncGameServerCreatingState(fixture)

		logrus.Printf("err: %+v", err)
		assert.Nil(t, err)
		assert.True(t, podCreated, "Pod should have been created")

		assert.Equal(t, v1alpha1.GameServerStateStarting, gs.Status.State)
		assert.True(t, gsUpdated, "GameServer should have been updated")
		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "Pod")
	})

	t.Run("Previously started sync, created Pod, but didn't move to Starting", func(t *testing.T) {
		c, m := newFakeController()
		fixture := newFixture()
		podCreated := false
		gsUpdated := false
		pod, err := fixture.Pod()
		assert.Nil(t, err)

		m.KubeClient.AddReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PodList{Items: []corev1.Pod{*pod}}, nil
		})
		m.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			podCreated = true
			return true, nil, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gsUpdated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, v1alpha1.GameServerStateStarting, gs.Status.State)
			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(m, c.gameServerSynced)
		defer cancel()

		gs, err := c.syncGameServerCreatingState(fixture)
		assert.Equal(t, v1alpha1.GameServerStateStarting, gs.Status.State)
		assert.Nil(t, err)
		assert.False(t, podCreated, "Pod should not have been created")
		assert.True(t, gsUpdated, "GameServer should have been updated")
	})

	t.Run("creates an invalid podspec", func(t *testing.T) {
		c, mocks := newFakeController()
		fixture := newFixture()
		podCreated := false
		gsUpdated := false

		mocks.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			podCreated = true
			return true, nil, k8serrors.NewInvalid(schema.GroupKind{}, "test", field.ErrorList{})
		})
		mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gsUpdated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, v1alpha1.GameServerStateError, gs.Status.State)
			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced)
		defer cancel()

		gs, err := c.syncGameServerCreatingState(fixture)
		assert.Nil(t, err)

		assert.True(t, podCreated, "attempt should have been made to create a pod")
		assert.True(t, gsUpdated, "GameServer should be updated")
		assert.Equal(t, v1alpha1.GameServerStateError, gs.Status.State)
	})

	t.Run("GameServer with unknown state", func(t *testing.T) {
		testNoChange(t, "Unknown", func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return c.syncGameServerCreatingState(fixture)
		})
	})

	t.Run("GameServer with non zero deletion datetime", func(t *testing.T) {
		testWithNonZeroDeletionTimestamp(t, func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return c.syncGameServerCreatingState(fixture)
		})
	})
}

func TestControllerSyncGameServerStartingState(t *testing.T) {
	t.Parallel()

	newFixture := func() *v1alpha1.GameServer {
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateStarting}}
		fixture.ApplyDefaults()
		return fixture
	}

	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Address: ipFixture, Type: corev1.NodeExternalIP}}}}

	t.Run("sync from Stating state, with no issues", func(t *testing.T) {
		c, m := newFakeController()
		gsFixture := newFixture()
		gsFixture.ApplyDefaults()
		pod, err := gsFixture.Pod()
		pod.Spec.NodeName = nodeFixtureName
		assert.Nil(t, err)
		gsUpdated := false

		m.KubeClient.AddReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.NodeList{Items: []corev1.Node{node}}, nil
		})
		m.KubeClient.AddReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PodList{Items: []corev1.Pod{*pod}}, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gsUpdated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, v1alpha1.GameServerStateScheduled, gs.Status.State)
			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(m, c.gameServerSynced, c.podSynced, c.nodeSynced)
		defer cancel()

		gs, err := c.syncGameServerStartingState(gsFixture)
		assert.Nil(t, err)

		assert.True(t, gsUpdated)
		assert.Equal(t, gs.Status.NodeName, node.ObjectMeta.Name)
		assert.Equal(t, gs.Status.Address, ipFixture)

		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "Address and port populated")
		assert.NotEmpty(t, gs.Status.Ports)
	})

	t.Run("GameServer with unknown state", func(t *testing.T) {
		testNoChange(t, "Unknown", func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return c.syncGameServerStartingState(fixture)
		})
	})

	t.Run("GameServer with non zero deletion datetime", func(t *testing.T) {
		testWithNonZeroDeletionTimestamp(t, func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return c.syncGameServerStartingState(fixture)
		})
	})
}

func TestControllerCreateGameServerPod(t *testing.T) {
	t.Parallel()

	newFixture := func() *v1alpha1.GameServer {
		fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateCreating}}
		fixture.ApplyDefaults()
		return fixture
	}

	t.Run("create pod, with no issues", func(t *testing.T) {
		c, m := newFakeController()
		fixture := newFixture()
		created := false

		m.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			created = true
			ca := action.(k8stesting.CreateAction)
			pod := ca.GetObject().(*corev1.Pod)

			assert.Equal(t, fixture.ObjectMeta.Name, pod.ObjectMeta.Name)
			assert.Equal(t, fixture.ObjectMeta.Namespace, pod.ObjectMeta.Namespace)
			assert.Equal(t, "sdk-service-account", pod.Spec.ServiceAccountName)
			assert.Equal(t, "gameserver", pod.ObjectMeta.Labels[stable.GroupName+"/role"])
			assert.Equal(t, fixture.ObjectMeta.Name, pod.ObjectMeta.Labels[v1alpha1.GameServerPodLabel])
			assert.True(t, metav1.IsControlledBy(pod, fixture))
			gsContainer := pod.Spec.Containers[0]
			assert.Equal(t, fixture.Spec.Ports[0].HostPort, gsContainer.Ports[0].HostPort)
			assert.Equal(t, fixture.Spec.Ports[0].ContainerPort, gsContainer.Ports[0].ContainerPort)
			assert.Equal(t, corev1.Protocol("UDP"), gsContainer.Ports[0].Protocol)
			assert.Equal(t, "/gshealthz", gsContainer.LivenessProbe.HTTPGet.Path)
			assert.Equal(t, gsContainer.LivenessProbe.HTTPGet.Port, intstr.FromInt(8080))
			assert.Equal(t, intstr.FromInt(8080), gsContainer.LivenessProbe.HTTPGet.Port)
			assert.Equal(t, fixture.Spec.Health.InitialDelaySeconds, gsContainer.LivenessProbe.InitialDelaySeconds)
			assert.Equal(t, fixture.Spec.Health.PeriodSeconds, gsContainer.LivenessProbe.PeriodSeconds)
			assert.Equal(t, fixture.Spec.Health.FailureThreshold, gsContainer.LivenessProbe.FailureThreshold)

			assert.Len(t, pod.Spec.Containers, 2, "Should have a sidecar container")

			assert.Len(t, pod.Spec.Containers[0].VolumeMounts, 1)
			assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount", pod.Spec.Containers[0].VolumeMounts[0].MountPath)

			assert.Equal(t, pod.Spec.Containers[1].Image, c.sidecarImage)
			assert.Equal(t, pod.Spec.Containers[1].Resources.Limits.Cpu(), &c.sidecarCPULimit)
			assert.Equal(t, pod.Spec.Containers[1].Resources.Requests.Cpu(), &c.sidecarCPURequest)
			assert.Len(t, pod.Spec.Containers[1].Env, 2, "2 env vars")
			assert.Equal(t, "GAMESERVER_NAME", pod.Spec.Containers[1].Env[0].Name)
			assert.Equal(t, fixture.ObjectMeta.Name, pod.Spec.Containers[1].Env[0].Value)
			assert.Equal(t, "POD_NAMESPACE", pod.Spec.Containers[1].Env[1].Name)
			return true, pod, nil
		})

		gs, err := c.createGameServerPod(fixture)

		assert.Nil(t, err)
		assert.Equal(t, fixture.Status.State, gs.Status.State)
		assert.True(t, created)
		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "Pod")
	})

	t.Run("service account", func(t *testing.T) {
		c, m := newFakeController()
		fixture := newFixture()
		fixture.Spec.Template.Spec.ServiceAccountName = "foobar"

		created := false

		m.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			created = true
			ca := action.(k8stesting.CreateAction)
			pod := ca.GetObject().(*corev1.Pod)
			assert.Len(t, pod.Spec.Containers, 2, "Should have a sidecar container")
			assert.Empty(t, pod.Spec.Containers[0].VolumeMounts)

			return true, pod, nil
		})

		_, err := c.createGameServerPod(fixture)
		assert.Nil(t, err)
		assert.True(t, created)
	})

	t.Run("invalid podspec", func(t *testing.T) {
		c, mocks := newFakeController()
		fixture := newFixture()
		podCreated := false
		gsUpdated := false

		mocks.KubeClient.AddReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			podCreated = true
			return true, nil, k8serrors.NewInvalid(schema.GroupKind{}, "test", field.ErrorList{})
		})
		mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gsUpdated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, v1alpha1.GameServerStateError, gs.Status.State)
			return true, gs, nil
		})

		gs, err := c.createGameServerPod(fixture)
		assert.Nil(t, err)

		assert.True(t, podCreated, "attempt should have been made to create a pod")
		assert.True(t, gsUpdated, "GameServer should be updated")
		assert.Equal(t, v1alpha1.GameServerStateError, gs.Status.State)
	})
}

func TestControllerApplyGameServerAddressAndPort(t *testing.T) {
	t.Parallel()
	c, m := newFakeController()

	gsFixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateRequestReady}}
	gsFixture.ApplyDefaults()
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Address: ipFixture, Type: corev1.NodeExternalIP}}}}
	pod, err := gsFixture.Pod()
	assert.Nil(t, err)
	pod.Spec.NodeName = node.ObjectMeta.Name

	m.KubeClient.AddReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.NodeList{Items: []corev1.Node{node}}, nil
	})

	_, cancel := agtesting.StartInformers(m, c.gameServerSynced)
	defer cancel()

	gs, err := c.applyGameServerAddressAndPort(gsFixture, pod)
	assert.Nil(t, err)
	assert.Equal(t, gs.Spec.Ports[0].HostPort, gs.Status.Ports[0].Port)
	assert.Equal(t, ipFixture, gs.Status.Address)
	assert.Equal(t, node.ObjectMeta.Name, gs.Status.NodeName)
}

func TestControllerSyncGameServerRequestReadyState(t *testing.T) {
	t.Parallel()

	t.Run("GameServer with ReadyRequest State", func(t *testing.T) {
		c, m := newFakeController()

		gsFixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateRequestReady}}
		gsFixture.ApplyDefaults()
		gsFixture.Status.NodeName = "node"
		pod, err := gsFixture.Pod()
		assert.Nil(t, err)
		gsUpdated := false

		m.KubeClient.AddReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PodList{Items: []corev1.Pod{*pod}}, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gsUpdated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, v1alpha1.GameServerStateReady, gs.Status.State)
			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(m, c.podSynced)
		defer cancel()

		gs, err := c.syncGameServerRequestReadyState(gsFixture)
		assert.Nil(t, err, "should not error")
		assert.True(t, gsUpdated, "GameServer wasn't updated")
		assert.Equal(t, v1alpha1.GameServerStateReady, gs.Status.State)
		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "SDK.Ready() complete")
	})

	t.Run("GameServer without an Address, but RequestReady State", func(t *testing.T) {
		c, m := newFakeController()

		gsFixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateRequestReady}}
		gsFixture.ApplyDefaults()
		pod, err := gsFixture.Pod()
		pod.Spec.NodeName = nodeFixtureName
		assert.Nil(t, err)
		gsUpdated := false

		ipFixture := "12.12.12.12"
		nodeFixture := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Address: ipFixture, Type: corev1.NodeExternalIP}}}}

		m.KubeClient.AddReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.NodeList{Items: []corev1.Node{nodeFixture}}, nil
		})

		m.KubeClient.AddReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PodList{Items: []corev1.Pod{*pod}}, nil
		})
		m.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gsUpdated = true
			ua := action.(k8stesting.UpdateAction)
			gs := ua.GetObject().(*v1alpha1.GameServer)
			assert.Equal(t, v1alpha1.GameServerStateReady, gs.Status.State)
			return true, gs, nil
		})

		_, cancel := agtesting.StartInformers(m, c.podSynced, c.nodeSynced)
		defer cancel()

		gs, err := c.syncGameServerRequestReadyState(gsFixture)
		assert.Nil(t, err, "should not error")
		assert.True(t, gsUpdated, "GameServer wasn't updated")
		assert.Equal(t, v1alpha1.GameServerStateReady, gs.Status.State)

		assert.Equal(t, gs.Status.NodeName, nodeFixture.ObjectMeta.Name)
		assert.Equal(t, gs.Status.Address, ipFixture)

		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "Address and port populated")
		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "SDK.Ready() complete")
	})

	for _, s := range []v1alpha1.GameServerState{"Unknown", v1alpha1.GameServerStateUnhealthy} {
		name := fmt.Sprintf("GameServer with %s state", s)
		t.Run(name, func(t *testing.T) {
			testNoChange(t, s, func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
				return c.syncGameServerRequestReadyState(fixture)
			})
		})
	}

	t.Run("GameServer with non zero deletion datetime", func(t *testing.T) {
		testWithNonZeroDeletionTimestamp(t, func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return c.syncGameServerRequestReadyState(fixture)
		})
	})
}

func TestControllerSyncGameServerShutdownState(t *testing.T) {
	t.Parallel()

	t.Run("GameServer with a Shutdown state", func(t *testing.T) {
		c, mocks := newFakeController()
		gsFixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateShutdown}}
		gsFixture.ApplyDefaults()
		checkDeleted := false

		mocks.AgonesClient.AddReactor("delete", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			checkDeleted = true
			assert.Equal(t, "default", action.GetNamespace())
			da := action.(k8stesting.DeleteAction)
			assert.Equal(t, "test", da.GetName())

			return true, nil, nil
		})

		_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced)
		defer cancel()

		err := c.syncGameServerShutdownState(gsFixture)
		assert.Nil(t, err)
		assert.True(t, checkDeleted, "GameServer should be deleted")
		assert.Contains(t, <-mocks.FakeRecorder.Events, "Deletion started")
	})

	t.Run("GameServer with unknown state", func(t *testing.T) {
		testNoChange(t, "Unknown", func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return fixture, c.syncGameServerShutdownState(fixture)
		})
	})

	t.Run("GameServer with non zero deletion datetime", func(t *testing.T) {
		testWithNonZeroDeletionTimestamp(t, func(c *Controller, fixture *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
			return fixture, c.syncGameServerShutdownState(fixture)
		})
	})
}

func TestControllerAddress(t *testing.T) {
	t.Parallel()

	fixture := map[string]struct {
		node            corev1.Node
		expectedAddress string
	}{
		"node with external ip": {
			node:            corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Address: "12.12.12.12", Type: corev1.NodeExternalIP}}}},
			expectedAddress: "12.12.12.12",
		},
		"node with an internal ip": {
			node:            corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Address: "11.11.11.11", Type: corev1.NodeInternalIP}}}},
			expectedAddress: "11.11.11.11",
		},
		"node with internal and external ip": {
			node: corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeFixtureName},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Address: "9.9.9.8", Type: corev1.NodeExternalIP},
					{Address: "12.12.12.12", Type: corev1.NodeInternalIP},
				}}},
			expectedAddress: "9.9.9.8",
		},
	}

	dummyGS := &v1alpha1.GameServer{}
	dummyGS.Name = "some-gs"

	for name, fixture := range fixture {
		t.Run(name, func(t *testing.T) {
			c, mocks := newFakeController()
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod"},
				Spec: corev1.PodSpec{NodeName: fixture.node.ObjectMeta.Name}}

			mocks.KubeClient.AddReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.PodList{Items: []corev1.Pod{pod}}, nil
			})
			mocks.KubeClient.AddReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.NodeList{Items: []corev1.Node{fixture.node}}, nil
			})

			v1 := mocks.KubeInformerFactory.Core().V1()
			nodeSynced := v1.Nodes().Informer().HasSynced
			podSynced := v1.Pods().Informer().HasSynced
			_, cancel := agtesting.StartInformers(mocks, c.gameServerSynced, podSynced, nodeSynced)
			defer cancel()

			addr, err := c.address(dummyGS, &pod)
			assert.Nil(t, err)
			assert.Equal(t, fixture.expectedAddress, addr)
		})
	}
}

func TestControllerGameServerPod(t *testing.T) {
	t.Parallel()

	setup := func() (*Controller, *v1alpha1.GameServer, *watch.FakeWatcher, <-chan struct{}, context.CancelFunc) {
		c, mocks := newFakeController()
		fakeWatch := watch.NewFake()
		mocks.KubeClient.AddWatchReactor("pods", k8stesting.DefaultWatchReactor(fakeWatch, nil))
		stop, cancel := agtesting.StartInformers(mocks, c.gameServerSynced)
		gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "gameserver",
			Namespace: defaultNs, UID: "1234"}, Spec: newSingleContainerSpec()}
		gs.ApplyDefaults()
		return c, gs, fakeWatch, stop, cancel
	}

	t.Run("no pod exists", func(t *testing.T) {
		c, gs, _, stop, cancel := setup()
		defer cancel()

		cache.WaitForCacheSync(stop, c.gameServerSynced)
		_, err := c.gameServerPod(gs)
		assert.Error(t, err)
		assert.True(t, k8serrors.IsNotFound(err))
	})

	t.Run("a pod exists", func(t *testing.T) {
		c, gs, fakeWatch, stop, cancel := setup()

		defer cancel()
		pod, err := gs.Pod()
		assert.Nil(t, err)

		fakeWatch.Add(pod.DeepCopy())
		cache.WaitForCacheSync(stop, c.gameServerSynced)
		pod2, err := c.gameServerPod(gs)
		assert.NoError(t, err)
		assert.Equal(t, pod, pod2)

		fakeWatch.Delete(pod.DeepCopy())
		cache.WaitForCacheSync(stop, c.gameServerSynced)
		_, err = c.gameServerPod(gs)
		assert.Error(t, err)
		assert.True(t, k8serrors.IsNotFound(err))
	})

	t.Run("a pod exists, but isn't owned by the gameserver", func(t *testing.T) {
		c, gs, fakeWatch, stop, cancel := setup()
		defer cancel()

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: gs.ObjectMeta.Name, Labels: map[string]string{v1alpha1.GameServerPodLabel: gs.ObjectMeta.Name, "owned": "false"}}}
		fakeWatch.Add(pod.DeepCopy())

		// gate
		cache.WaitForCacheSync(stop, c.podSynced)
		pod, err := c.podGetter.Pods(defaultNs).Get(pod.ObjectMeta.Name, metav1.GetOptions{})
		assert.NoError(t, err)
		assert.NotNil(t, pod)

		_, err = c.gameServerPod(gs)
		assert.Error(t, err)
		assert.True(t, k8serrors.IsNotFound(err))
	})

	t.Run("dev gameserver pod", func(t *testing.T) {
		c, _ := newFakeController()

		gs := &v1alpha1.GameServer{
			ObjectMeta: metav1.ObjectMeta{Name: "gameserver", Namespace: defaultNs,
				Annotations: map[string]string{
					v1alpha1.DevAddressAnnotation: "1.1.1.1",
				},
				UID: "1234"},

			Spec: newSingleContainerSpec()}

		pod, err := c.gameServerPod(gs)
		assert.NoError(t, err)
		assert.Empty(t, pod.ObjectMeta.Name)
	})
}

func TestControllerAddGameServerHealthCheck(t *testing.T) {
	c, _ := newFakeController()
	fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateCreating}}
	fixture.ApplyDefaults()

	assert.False(t, fixture.Spec.Health.Disabled)
	pod, err := fixture.Pod()
	assert.Nil(t, err, "Error: %v", err)
	c.addGameServerHealthCheck(fixture, pod)

	assert.Len(t, pod.Spec.Containers, 1)
	probe := pod.Spec.Containers[0].LivenessProbe
	assert.NotNil(t, probe)
	assert.Equal(t, "/gshealthz", probe.HTTPGet.Path)
	assert.Equal(t, intstr.IntOrString{IntVal: 8080}, probe.HTTPGet.Port)
	assert.Equal(t, fixture.Spec.Health.FailureThreshold, probe.FailureThreshold)
	assert.Equal(t, fixture.Spec.Health.InitialDelaySeconds, probe.InitialDelaySeconds)
	assert.Equal(t, fixture.Spec.Health.PeriodSeconds, probe.PeriodSeconds)
}

func TestIsGameServerPod(t *testing.T) {

	t.Run("it is a game server pod", func(t *testing.T) {
		gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "gameserver", UID: "1234"}, Spec: newSingleContainerSpec()}
		gs.ApplyDefaults()
		pod, err := gs.Pod()
		assert.Nil(t, err)

		assert.True(t, isGameServerPod(pod))
	})

	t.Run("it is not a game server pod", func(t *testing.T) {
		pod := &corev1.Pod{}
		assert.False(t, isGameServerPod(pod))
	})

}

// testNoChange runs a test with a state that doesn't exist, to ensure a handler
// doesn't do process anything beyond the state it is meant to handle.
func testNoChange(t *testing.T, state v1alpha1.GameServerState, f func(*Controller, *v1alpha1.GameServer) (*v1alpha1.GameServer, error)) {
	c, mocks := newFakeController()
	fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: state}}
	fixture.ApplyDefaults()
	updated := false
	mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updated = true
		return true, nil, nil
	})

	result, err := f(c, fixture)
	assert.Nil(t, err, "sync should not error")
	assert.False(t, updated, "update should occur")
	assert.Equal(t, fixture, result)
}

// testWithNonZeroDeletionTimestamp runs a test with a given state, but
// the DeletionTimestamp set to Now()
func testWithNonZeroDeletionTimestamp(t *testing.T, f func(*Controller, *v1alpha1.GameServer) (*v1alpha1.GameServer, error)) {
	c, mocks := newFakeController()
	now := metav1.Now()
	fixture := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", DeletionTimestamp: &now},
		Spec: newSingleContainerSpec(), Status: v1alpha1.GameServerStatus{State: v1alpha1.GameServerStateShutdown}}
	fixture.ApplyDefaults()
	updated := false
	mocks.AgonesClient.AddReactor("update", "gameservers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updated = true
		return true, nil, nil
	})

	result, err := f(c, fixture)
	assert.Nil(t, err, "sync should not error")
	assert.False(t, updated, "update should occur")
	assert.Equal(t, fixture, result)
}

// newFakeController returns a controller, backed by the fake Clientset
func newFakeController() (*Controller, agtesting.Mocks) {
	m := agtesting.NewMocks()
	wh := webhooks.NewWebHook(http.NewServeMux())
	c := NewController(wh, healthcheck.NewHandler(),
		10, 20, "sidecar:dev", false,
		resource.MustParse("0.05"), resource.MustParse("0.1"), "sdk-service-account",
		m.KubeClient, m.KubeInformerFactory, m.ExtClient, m.AgonesClient, m.AgonesInformerFactory)
	c.recorder = m.FakeRecorder
	return c, m
}

func newSingleContainerSpec() v1alpha1.GameServerSpec {
	return v1alpha1.GameServerSpec{
		Ports: []v1alpha1.GameServerPort{{ContainerPort: 7777, HostPort: 9999, PortPolicy: v1alpha1.Static}},
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "container", Image: "container/image"}},
			},
		},
	}
}
