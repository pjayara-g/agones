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

package e2e

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	stablev1alpha1 "agones.dev/agones/pkg/client/clientset/versioned/typed/stable/v1alpha1"
	e2e "agones.dev/agones/test/e2e/framework"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1betaext "k8s.io/api/extensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

const (
	key           = "test-state"
	red           = "red"
	green         = "green"
	replicasCount = 3
)

func TestFleetScaleUpEditAndScaleDown(t *testing.T) {
	t.Parallel()

	//Use scaleFleetPatch (true) or scaleFleetSubresource (false)
	fixtures := []bool{true, false}

	for _, usePatch := range fixtures {
		t.Run("Use fleet Patch "+fmt.Sprint(usePatch), func(t *testing.T) {
			alpha1 := framework.AgonesClient.StableV1alpha1()

			flt := defaultFleet()
			flt.Spec.Replicas = 1
			flt, err := alpha1.Fleets(defaultNs).Create(flt)
			if assert.Nil(t, err) {
				defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
			}

			assert.Equal(t, int32(1), flt.Spec.Replicas)

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

			// scale up
			const targetScale = 3
			if usePatch {
				flt = scaleFleetPatch(t, flt, targetScale)
				assert.Equal(t, int32(targetScale), flt.Spec.Replicas)
			} else {
				flt = scaleFleetSubresource(t, flt, targetScale)
			}

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(targetScale))
			gsa := framework.CreateAndApplyAllocation(t, flt)

			framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
				return fleet.Status.AllocatedReplicas == 1
			})

			flt, err = alpha1.Fleets(defaultNs).Get(flt.ObjectMeta.GetName(), metav1.GetOptions{})
			assert.Nil(t, err)

			// Change ContainerPort to trigger creating a new GSSet
			fltCopy := flt.DeepCopy()
			fltCopy.Spec.Template.Spec.Ports[0].ContainerPort++
			flt, err = alpha1.Fleets(defaultNs).Update(fltCopy)
			assert.Nil(t, err)

			// Wait for one more GSSet to be created and ReadyReplicas created in new GSS
			err = wait.PollImmediate(1*time.Second, 15*time.Second, func() (bool, error) {
				selector := labels.SelectorFromSet(labels.Set{v1alpha1.FleetNameLabel: flt.ObjectMeta.Name})
				list, err := framework.AgonesClient.StableV1alpha1().GameServerSets(defaultNs).List(
					metav1.ListOptions{LabelSelector: selector.String()})
				if err != nil {
					return false, err
				}
				ready := false
				if len(list.Items) == 2 {
					for _, v := range list.Items {
						if v.Status.ReadyReplicas > 0 && v.Status.AllocatedReplicas == 0 {
							ready = true
						}
					}
				}
				return ready, nil
			})

			assert.Nil(t, err)

			// scale down, with allocation
			const scaleDownTarget = 1
			if usePatch {
				flt = scaleFleetPatch(t, flt, scaleDownTarget)
			} else {
				flt = scaleFleetSubresource(t, flt, scaleDownTarget)
			}
			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(0))

			// delete the allocated GameServer
			gp := int64(1)
			err = alpha1.GameServers(defaultNs).Delete(gsa.Status.GameServerName, &metav1.DeleteOptions{GracePeriodSeconds: &gp})
			assert.Nil(t, err)

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(1))

			framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
				return fleet.Status.AllocatedReplicas == 0
			})
		})
	}
}

// TestFleetRollingUpdate - test that the limited number of gameservers are created and deleted at a time
// maxUnavailable and maxSurge parameters check.
func TestFleetRollingUpdate(t *testing.T) {
	t.Parallel()

	//Use scaleFleetPatch (true) or scaleFleetSubresource (false)
	fixtures := []bool{true, false}
	maxSurge := []string{"25%", "10%"}

	for _, usePatch := range fixtures {
		for _, maxSurgeParam := range maxSurge {
			t.Run(fmt.Sprintf("Use fleet Patch %t %s", usePatch, maxSurgeParam), func(t *testing.T) {
				alpha1 := framework.AgonesClient.StableV1alpha1()

				flt := defaultFleet()
				flt.ApplyDefaults()
				flt.Spec.Replicas = 1
				rollingUpdatePercent := intstr.FromString(maxSurgeParam)
				flt.Spec.Strategy.RollingUpdate.MaxSurge = &rollingUpdatePercent
				flt.Spec.Strategy.RollingUpdate.MaxUnavailable = &rollingUpdatePercent

				flt, err := alpha1.Fleets(defaultNs).Create(flt)
				if assert.Nil(t, err) {
					defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
				}

				assert.Equal(t, int32(1), flt.Spec.Replicas)

				framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

				// scale up
				const targetScale = 8
				if usePatch {
					flt = scaleFleetPatch(t, flt, targetScale)
					assert.Equal(t, int32(targetScale), flt.Spec.Replicas)
				} else {
					flt = scaleFleetSubresource(t, flt, targetScale)
				}

				framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(targetScale))

				flt, err = alpha1.Fleets(defaultNs).Get(flt.ObjectMeta.GetName(), metav1.GetOptions{})
				assert.NoError(t, err)

				// Change ContainerPort to trigger creating a new GSSet
				fltCopy := flt.DeepCopy()
				fltCopy.Spec.Template.Spec.Ports[0].ContainerPort++
				flt, err = alpha1.Fleets(defaultNs).Update(fltCopy)
				assert.NoError(t, err)

				selector := labels.SelectorFromSet(labels.Set{v1alpha1.FleetNameLabel: flt.ObjectMeta.Name})
				// New GSS was created
				err = wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
					gssList, err := framework.AgonesClient.StableV1alpha1().GameServerSets(defaultNs).List(
						metav1.ListOptions{LabelSelector: selector.String()})
					if err != nil {
						return false, err
					}
					return len(gssList.Items) == 2, nil
				})
				assert.NoError(t, err)
				// Check that total number of gameservers in the system does not exceed the RollingUpdate
				// parameters (creating no more than maxSurge, deleting maxUnavailable servers at a time)
				// Wait for old GSSet to be deleted
				err = wait.PollImmediate(1*time.Second, 5*time.Minute, func() (bool, error) {
					list, err := framework.AgonesClient.StableV1alpha1().GameServers(defaultNs).List(
						metav1.ListOptions{LabelSelector: selector.String()})
					if err != nil {
						return false, err
					}

					maxSurge, err := intstr.GetValueFromIntOrPercent(flt.Spec.Strategy.RollingUpdate.MaxSurge, 100, true)
					assert.Nil(t, err)
					maxUnavailable, err := intstr.GetValueFromIntOrPercent(flt.Spec.Strategy.RollingUpdate.MaxUnavailable, 100, true)
					assert.Nil(t, err)
					target := float64(targetScale)
					if len(list.Items) > int(target+math.Ceil(target*float64(maxSurge)/100.)+math.Ceil(target*float64(maxUnavailable)/100.)) {
						err = errors.New("New replicas should be less then target + maxSurge + maxUnavailable")
					}
					if err != nil {
						return false, err
					}
					gssList, err := framework.AgonesClient.StableV1alpha1().GameServerSets(defaultNs).List(
						metav1.ListOptions{LabelSelector: selector.String()})
					if err != nil {
						return false, err
					}
					return len(gssList.Items) == 1, nil
				})

				assert.NoError(t, err)

				// scale down, with allocation
				const scaleDownTarget = 1
				if usePatch {
					flt = scaleFleetPatch(t, flt, scaleDownTarget)
				} else {
					flt = scaleFleetSubresource(t, flt, scaleDownTarget)
				}

				framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(1))

				framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
					return fleet.Status.AllocatedReplicas == 0
				})
			})
		}
	}
}

func TestScaleFleetUpAndDownWithGameServerAllocation(t *testing.T) {
	t.Parallel()

	fixtures := []bool{false, true}

	for _, usePatch := range fixtures {
		t.Run("Use fleet Patch "+fmt.Sprint(usePatch), func(t *testing.T) {
			alpha1 := framework.AgonesClient.StableV1alpha1()

			flt := defaultFleet()
			flt.Spec.Replicas = 1
			flt, err := alpha1.Fleets(defaultNs).Create(flt)
			if assert.Nil(t, err) {
				defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
			}

			assert.Equal(t, int32(1), flt.Spec.Replicas)

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

			// scale up
			const targetScale = 3
			if usePatch {
				flt = scaleFleetPatch(t, flt, targetScale)
				assert.Equal(t, int32(targetScale), flt.Spec.Replicas)
			} else {
				flt = scaleFleetSubresource(t, flt, targetScale)
			}

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(targetScale))

			// get an allocation
			gsa := &allocationv1.GameServerAllocation{ObjectMeta: metav1.ObjectMeta{GenerateName: "allocation-"},
				Spec: allocationv1.GameServerAllocationSpec{
					Required: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.FleetNameLabel: flt.ObjectMeta.Name}},
				}}

			gsa, err = framework.AgonesClient.AllocationV1().GameServerAllocations(defaultNs).Create(gsa)
			assert.Nil(t, err)
			assert.Equal(t, allocationv1.GameServerAllocationAllocated, gsa.Status.State)
			framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
				return fleet.Status.AllocatedReplicas == 1
			})

			// scale down, with allocation
			const scaleDownTarget = 1
			if usePatch {
				flt = scaleFleetPatch(t, flt, scaleDownTarget)
			} else {
				flt = scaleFleetSubresource(t, flt, scaleDownTarget)
			}

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(0))

			// delete the allocated GameServer
			gp := int64(1)
			err = alpha1.GameServers(defaultNs).Delete(gsa.Status.GameServerName, &metav1.DeleteOptions{GracePeriodSeconds: &gp})
			assert.Nil(t, err)
			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(1))

			framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
				return fleet.Status.AllocatedReplicas == 0
			})
		})
	}
}

func TestFleetUpdates(t *testing.T) {
	t.Parallel()

	fixtures := map[string]func() *v1alpha1.Fleet{
		"recreate": func() *v1alpha1.Fleet {
			flt := defaultFleet()
			flt.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
			return flt
		},
		"rolling": func() *v1alpha1.Fleet {
			flt := defaultFleet()
			flt.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
			return flt
		},
	}

	for k, v := range fixtures {
		t.Run(k, func(t *testing.T) {
			alpha1 := framework.AgonesClient.StableV1alpha1()

			flt := v()
			flt.Spec.Template.ObjectMeta.Annotations = map[string]string{key: red}
			flt, err := alpha1.Fleets(defaultNs).Create(flt)
			if assert.Nil(t, err) {
				defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
			}

			err = framework.WaitForFleetGameServersCondition(flt, func(gs v1alpha1.GameServer) bool {
				return gs.ObjectMeta.Annotations[key] == red
			})
			assert.Nil(t, err)

			// if the generation has been updated, it's time to try again.
			err = wait.PollImmediate(time.Second, 10*time.Second, func() (bool, error) {
				flt, err = framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Get(flt.ObjectMeta.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				fltCopy := flt.DeepCopy()
				fltCopy.Spec.Template.ObjectMeta.Annotations[key] = green
				_, err = framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Update(fltCopy)
				if err != nil {
					logrus.WithError(err).Warn("Could not update fleet, trying again")
					return false, nil
				}

				return true, nil
			})
			assert.Nil(t, err)

			err = framework.WaitForFleetGameServersCondition(flt, func(gs v1alpha1.GameServer) bool {
				return gs.ObjectMeta.Annotations[key] == green
			})
			assert.Nil(t, err)
		})
	}
}

func TestUpdateGameServerConfigurationInFleet(t *testing.T) {
	t.Parallel()

	alpha1 := framework.AgonesClient.StableV1alpha1()

	gsSpec := defaultGameServer().Spec
	oldPort := int32(7111)
	gsSpec.Ports = []v1alpha1.GameServerPort{{
		ContainerPort: oldPort,
		Name:          "gameport",
		PortPolicy:    v1alpha1.Dynamic,
		Protocol:      corev1.ProtocolUDP,
	}}
	flt := fleetWithGameServerSpec(gsSpec)
	flt, err := alpha1.Fleets(defaultNs).Create(flt)
	assert.Nil(t, err, "could not create fleet")
	defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck

	assert.Equal(t, int32(replicasCount), flt.Spec.Replicas)

	framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

	// get an allocation
	gsa := &allocationv1.GameServerAllocation{ObjectMeta: metav1.ObjectMeta{GenerateName: "allocation-"},
		Spec: allocationv1.GameServerAllocationSpec{
			Required: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.FleetNameLabel: flt.ObjectMeta.Name}},
		}}

	gsa, err = framework.AgonesClient.AllocationV1().GameServerAllocations(defaultNs).Create(gsa)
	assert.Nil(t, err, "cloud not create gameserver allocation")
	assert.Equal(t, allocationv1.GameServerAllocationAllocated, gsa.Status.State)
	framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
		return fleet.Status.AllocatedReplicas == 1
	})

	flt, err = framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Get(flt.Name, metav1.GetOptions{})
	assert.Nil(t, err, "could not get fleet")

	// Update the configuration of the gameservers of the fleet, i.e. container port.
	// The changes should only be rolled out to gameservers in ready state, but not the allocated gameserver.
	newPort := int32(7222)
	fltCopy := flt.DeepCopy()
	fltCopy.Spec.Template.Spec.Ports[0].ContainerPort = newPort

	_, err = framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Update(fltCopy)
	assert.Nil(t, err, "could not update fleet")

	err = framework.WaitForFleetGameServersCondition(flt, func(gs v1alpha1.GameServer) bool {
		containerPort := gs.Spec.Ports[0].ContainerPort
		return (gs.Name == gsa.Status.GameServerName && containerPort == oldPort) ||
			(gs.Name != gsa.Status.GameServerName && containerPort == newPort)
	})
	assert.Nil(t, err, "gameservers don't have expected container port")
}

func TestReservedGameServerInFleet(t *testing.T) {
	alpha1 := framework.AgonesClient.StableV1alpha1()

	flt := defaultFleet()
	flt.Spec.Replicas = 3
	flt, err := alpha1.Fleets(defaultNs).Create(flt)
	if assert.NoError(t, err) {
		defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
	}

	framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

	gsList, err := framework.ListGameServersFromFleet(flt)
	assert.NoError(t, err)

	assert.Len(t, gsList, int(flt.Spec.Replicas))

	// mark one as reserved
	gsCopy := gsList[0].DeepCopy()
	gsCopy.Status.State = v1alpha1.GameServerStateReserved
	_, err = alpha1.GameServers(defaultNs).Update(gsCopy)
	assert.NoError(t, err)

	// make sure counts are correct
	framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
		return fleet.Status.ReadyReplicas == 2 && fleet.Status.ReservedReplicas == 1
	})

	// scale down to 0
	flt = scaleFleetSubresource(t, flt, 0)
	framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(0))

	// one should be left behind
	framework.WaitForFleetCondition(t, flt, func(fleet *v1alpha1.Fleet) bool {
		result := fleet.Status.ReservedReplicas == 1
		logrus.WithField("reserved", fleet.Status.ReservedReplicas).WithField("result", result).Info("waiting for 1 reserved replica")
		return result
	})

	// check against gameservers directly too, just to be extra sure
	err = wait.PollImmediate(2*time.Second, 5*time.Minute, func() (done bool, err error) {
		list, err := framework.ListGameServersFromFleet(flt)
		if err != nil {
			return true, err
		}
		l := len(list)
		logrus.WithField("len", l).WithField("state", list[0].Status.State).Info("waiting for 1 reserved gs")
		return l == 1 && list[0].Status.State == v1alpha1.GameServerStateReserved, nil
	})
	assert.NoError(t, err)
}

// TestFleetGSSpecValidation is built to test Fleet's underlying Gameserver template
// validation. Gameserver Spec contained in a Fleet should be valid to create a fleet.
func TestFleetGSSpecValidation(t *testing.T) {
	t.Parallel()
	alpha1 := framework.AgonesClient.StableV1alpha1()

	// check two Containers in Gameserver Spec Template validation
	flt := defaultFleet()
	containerName := "container2"
	flt.Spec.Template.Spec.Template =
		corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "container", Image: "myImage"}, {Name: containerName, Image: "myImage2"}},
			},
		}
	flt.Spec.Template.Spec.Container = "testing"
	_, err := alpha1.Fleets(defaultNs).Create(flt)
	assert.NotNil(t, err)
	statusErr, ok := err.(*k8serrors.StatusError)
	assert.True(t, ok)
	assert.Len(t, statusErr.Status().Details.Causes, 1)
	assert.Equal(t, metav1.CauseTypeFieldValueInvalid, statusErr.Status().Details.Causes[0].Type)
	assert.Equal(t, "Could not find a container named testing", statusErr.Status().Details.Causes[0].Message)

	flt.Spec.Template.Spec.Container = ""
	_, err = alpha1.Fleets(defaultNs).Create(flt)
	assert.NotNil(t, err)
	statusErr, ok = err.(*k8serrors.StatusError)
	assert.True(t, ok)
	assert.Len(t, statusErr.Status().Details.Causes, 2)
	CausesMessages := []string{v1alpha1.ErrContainerRequired, "Could not find a container named "}
	assert.Equal(t, metav1.CauseTypeFieldValueInvalid, statusErr.Status().Details.Causes[0].Type)
	assert.Contains(t, CausesMessages, statusErr.Status().Details.Causes[0].Message)
	assert.Equal(t, metav1.CauseTypeFieldValueInvalid, statusErr.Status().Details.Causes[1].Type)
	assert.Contains(t, CausesMessages, statusErr.Status().Details.Causes[1].Message)

	// use valid name for a container, one of two defined above
	flt.Spec.Template.Spec.Container = containerName
	_, err = alpha1.Fleets(defaultNs).Create(flt)
	if assert.Nil(t, err) {
		defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
	}

	// check port configuration validation
	fltPort := defaultFleet()

	fltPort.Spec.Template.Spec.Ports = []v1alpha1.GameServerPort{{Name: "Dyn", HostPort: 5555, PortPolicy: v1alpha1.Dynamic, ContainerPort: 5555}}

	_, err = alpha1.Fleets(defaultNs).Create(fltPort)
	assert.NotNil(t, err)
	statusErr, ok = err.(*k8serrors.StatusError)
	assert.True(t, ok)
	assert.Len(t, statusErr.Status().Details.Causes, 1)
	assert.Equal(t, v1alpha1.ErrHostPortDynamic, statusErr.Status().Details.Causes[0].Message)

	fltPort.Spec.Template.Spec.Ports[0].PortPolicy = v1alpha1.Static
	fltPort.Spec.Template.Spec.Ports[0].HostPort = 0
	fltPort.Spec.Template.Spec.Ports[0].ContainerPort = 5555
	_, err = alpha1.Fleets(defaultNs).Create(fltPort)
	if assert.Nil(t, err) {
		defer alpha1.Fleets(defaultNs).Delete(fltPort.ObjectMeta.Name, nil) // nolint:errcheck
	}
}

// TestFleetNameValidation is built to test Fleet Name length validation,
// Fleet Name should have at most 63 chars.
func TestFleetNameValidation(t *testing.T) {
	t.Parallel()
	alpha1 := framework.AgonesClient.StableV1alpha1()

	flt := defaultFleet()
	nameLen := validation.LabelValueMaxLength + 1
	bytes := make([]byte, nameLen)
	for i := 0; i < nameLen; i++ {
		bytes[i] = 'f'
	}
	flt.Name = string(bytes)
	_, err := alpha1.Fleets(defaultNs).Create(flt)
	assert.NotNil(t, err)
	statusErr, ok := err.(*k8serrors.StatusError)
	assert.True(t, ok)
	assert.True(t, len(statusErr.Status().Details.Causes) > 0)
	assert.Equal(t, metav1.CauseTypeFieldValueInvalid, statusErr.Status().Details.Causes[0].Type)
	goodFlt := defaultFleet()
	goodFlt.Name = string(bytes[0 : nameLen-1])
	goodFlt, err = alpha1.Fleets(defaultNs).Create(goodFlt)
	if assert.Nil(t, err) {
		defer alpha1.Fleets(defaultNs).Delete(goodFlt.ObjectMeta.Name, nil) // nolint:errcheck
	}
}

func assertSuccessOrUpdateConflict(t *testing.T, err error) {
	if !k8serrors.IsConflict(err) {
		// update conflicts are sometimes ok, we simply lost the race.
		assert.Nil(t, err)
	}
}

// TestGameServerAllocationDuringGameServerDeletion is built to specifically
// test for race conditions of allocations when doing scale up/down,
// rolling updates, etc. Failures may not happen ALL the time -- as that is the
// nature of race conditions.
func TestGameServerAllocationDuringGameServerDeletion(t *testing.T) {
	t.Parallel()

	testAllocationRaceCondition := func(t *testing.T, fleet func() *v1alpha1.Fleet, deltaSleep time.Duration, delta func(t *testing.T, flt *v1alpha1.Fleet)) {
		alpha1 := framework.AgonesClient.StableV1alpha1()

		flt := fleet()
		flt.ApplyDefaults()
		size := int32(10)
		flt.Spec.Replicas = size
		flt, err := alpha1.Fleets(defaultNs).Create(flt)
		if assert.Nil(t, err) {
			defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
		}

		assert.Equal(t, size, flt.Spec.Replicas)

		framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

		var allocs []string

		wg := sync.WaitGroup{}
		wg.Add(2)
		go func() {
			for {
				// this gives room for fleet scaling to go down - makes it more likely for the race condition to fire
				time.Sleep(100 * time.Millisecond)
				gsa := &allocationv1.GameServerAllocation{ObjectMeta: metav1.ObjectMeta{GenerateName: "allocation-"},
					Spec: allocationv1.GameServerAllocationSpec{
						Required: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.FleetNameLabel: flt.ObjectMeta.Name}},
					}}
				gsa, err = framework.AgonesClient.AllocationV1().GameServerAllocations(defaultNs).Create(gsa)
				if err != nil || gsa.Status.State == allocationv1.GameServerAllocationUnAllocated {
					logrus.WithError(err).Info("Allocation ended")
					break
				}
				logrus.WithField("gs", gsa.Status.GameServerName).Info("Allocated")
				allocs = append(allocs, gsa.Status.GameServerName)
			}
			wg.Done()
		}()
		go func() {
			// this tends to force the scaling to happen as we are fleet allocating
			time.Sleep(deltaSleep)
			// call the function that makes the change to the fleet
			logrus.Info("Applying delta function")
			delta(t, flt)
			wg.Done()
		}()

		wg.Wait()
		assert.NotEmpty(t, allocs)

		for _, name := range allocs {
			gsCheck, err := alpha1.GameServers(defaultNs).Get(name, metav1.GetOptions{})
			assert.Nil(t, err)
			assert.True(t, gsCheck.ObjectMeta.DeletionTimestamp.IsZero())
		}
	}

	t.Run("scale down", func(t *testing.T) {
		t.Parallel()

		testAllocationRaceCondition(t, defaultFleet, time.Second,
			func(t *testing.T, flt *v1alpha1.Fleet) {
				const targetScale = int32(0)
				flt = scaleFleetPatch(t, flt, targetScale)
				assert.Equal(t, targetScale, flt.Spec.Replicas)
			})
	})

	t.Run("recreate update", func(t *testing.T) {
		t.Parallel()

		fleet := func() *v1alpha1.Fleet {
			flt := defaultFleet()
			flt.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
			flt.Spec.Template.ObjectMeta.Annotations = map[string]string{key: red}

			return flt
		}

		testAllocationRaceCondition(t, fleet, time.Second,
			func(t *testing.T, flt *v1alpha1.Fleet) {
				flt, err := framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Get(flt.ObjectMeta.Name, metav1.GetOptions{})
				assert.Nil(t, err)
				fltCopy := flt.DeepCopy()
				fltCopy.Spec.Template.ObjectMeta.Annotations[key] = green
				_, err = framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Update(fltCopy)
				assertSuccessOrUpdateConflict(t, err)
			})
	})

	t.Run("rolling update", func(t *testing.T) {
		t.Parallel()

		fleet := func() *v1alpha1.Fleet {
			flt := defaultFleet()
			flt.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
			flt.Spec.Template.ObjectMeta.Annotations = map[string]string{key: red}

			return flt
		}

		testAllocationRaceCondition(t, fleet, time.Duration(0),
			func(t *testing.T, flt *v1alpha1.Fleet) {
				flt, err := framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Get(flt.ObjectMeta.Name, metav1.GetOptions{})
				assert.Nil(t, err)
				fltCopy := flt.DeepCopy()
				fltCopy.Spec.Template.ObjectMeta.Annotations[key] = green
				_, err = framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Update(fltCopy)
				assertSuccessOrUpdateConflict(t, err)
			})
	})
}

// TestCreateFleetAndUpdateScaleSubresource is built to
// test scale subresource usage and its ability to change Fleet Replica size.
// Both scaling up and down.
func TestCreateFleetAndUpdateScaleSubresource(t *testing.T) {
	alpha1 := framework.AgonesClient.StableV1alpha1()

	flt := defaultFleet()
	const initialReplicas int32 = 1
	flt.Spec.Replicas = initialReplicas
	flt, err := alpha1.Fleets(defaultNs).Create(flt)
	if assert.Nil(t, err) {
		defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
	}
	assert.Equal(t, initialReplicas, flt.Spec.Replicas)
	framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

	newReplicas := initialReplicas * 2
	scaleFleetSubresource(t, flt, newReplicas)
	framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(newReplicas))

	scaleFleetSubresource(t, flt, initialReplicas)
	framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(initialReplicas))
}

// TestScaleUpAndDownInParallelStressTest creates N fleets, half of which start with replicas=0
// and the other half with 0 and scales them up/down 3 times in parallel expecting it to reach
// the desired number of ready replicas each time.
// This test is also used as a stress test with 'make stress-test-e2e', in which case it creates
// many more fleets of bigger sizes and runs many more repetitions.
func TestScaleUpAndDownInParallelStressTest(t *testing.T) {
	t.Parallel()

	alpha1 := framework.AgonesClient.StableV1alpha1()
	fleetCount := 2
	fleetSize := int32(10)
	repeatCount := 3
	deadline := time.Now().Add(1 * time.Minute)

	logrus.WithField("fleetCount", fleetCount).
		WithField("fleetSize", fleetSize).
		WithField("repeatCount", repeatCount).
		WithField("deadline", deadline).
		Info("starting scale up/down test")

	if framework.StressTestLevel > 0 {
		fleetSize = 10 * int32(framework.StressTestLevel)
		repeatCount = 10
		fleetCount = 10
		deadline = time.Now().Add(45 * time.Minute)
	}

	var fleets []*v1alpha1.Fleet

	scaleUpStats := framework.NewStatsCollector(fmt.Sprintf("fleet_%v_scale_up", fleetSize))
	scaleDownStats := framework.NewStatsCollector(fmt.Sprintf("fleet_%v_scale_down", fleetSize))

	defer scaleUpStats.Report()
	defer scaleDownStats.Report()

	for fleetNumber := 0; fleetNumber < fleetCount; fleetNumber++ {
		flt := defaultFleet()
		flt.ObjectMeta.GenerateName = fmt.Sprintf("scale-fleet-%v-", fleetNumber)
		if fleetNumber%2 == 0 {
			// even-numbered fleets starts at fleetSize and are scaled down to zero and back.
			flt.Spec.Replicas = fleetSize
		} else {
			// odd-numbered fleets starts at zero and are scaled up to fleetSize and back.
			flt.Spec.Replicas = 0
		}

		flt, err := alpha1.Fleets(defaultNs).Create(flt)
		if assert.Nil(t, err) {
			defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
		}
		fleets = append(fleets, flt)
	}

	// wait for initial fleet conditions.
	for fleetNumber, flt := range fleets {
		if fleetNumber%2 == 0 {
			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(fleetSize))
		} else {
			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(0))
		}
	}

	var wg sync.WaitGroup

	for fleetNumber, flt := range fleets {
		wg.Add(1)
		go func(fleetNumber int, flt *v1alpha1.Fleet) {
			defer wg.Done()
			defer func() {
				if err := recover(); err != nil {
					t.Errorf("recovered panic: %v", err)
				}
			}()

			if fleetNumber%2 == 0 {
				scaleDownStats.ReportDuration(scaleAndWait(t, flt, 0), nil)
			}
			for i := 0; i < repeatCount; i++ {
				if time.Now().After(deadline) {
					break
				}
				scaleUpStats.ReportDuration(scaleAndWait(t, flt, fleetSize), nil)
				scaleDownStats.ReportDuration(scaleAndWait(t, flt, 0), nil)
			}
		}(fleetNumber, flt)
	}

	wg.Wait()
}

// Creates a fleet and one GameServer with Packed scheduling.
// Scale to two GameServers with Distributed scheduling.
// The old GameServer has Scheduling set to 5 and the new one has it set to Distributed.
func TestUpdateFleetScheduling(t *testing.T) {
	t.Parallel()
	t.Run("Updating Spec.Scheduling on fleet should be updated in GameServer",
		func(t *testing.T) {
			alpha1 := framework.AgonesClient.StableV1alpha1()

			flt := defaultFleet()
			flt.Spec.Replicas = 1
			flt.Spec.Scheduling = apis.Packed
			flt, err := alpha1.Fleets(defaultNs).Create(flt)

			if assert.Nil(t, err) {
				defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
			}

			assert.Equal(t, int32(1), flt.Spec.Replicas)
			assert.Equal(t, apis.Packed, flt.Spec.Scheduling)

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

			const targetScale = 2
			flt = schedulingFleetPatch(t, flt, apis.Distributed, targetScale)
			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(targetScale))

			assert.Equal(t, int32(targetScale), flt.Spec.Replicas)
			assert.Equal(t, apis.Distributed, flt.Spec.Scheduling)

			err = framework.WaitForFleetGameServerListCondition(flt,
				func(gsList []v1alpha1.GameServer) bool {
					return countFleetScheduling(gsList, apis.Distributed) == 1 &&
						countFleetScheduling(gsList, apis.Packed) == 1
				})
			assert.Nil(t, err)
		})
}

// TestFleetRecreateGameServers tests various gameserver shutdown scenarios to ensure
// that recreation happens as expected
func TestFleetRecreateGameServers(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		f func(t *testing.T, list *v1alpha1.GameServerList)
	}{
		"pod deletion": {f: func(t *testing.T, list *v1alpha1.GameServerList) {
			podClient := framework.KubeClient.CoreV1().Pods(defaultNs)

			for _, gs := range list.Items {
				pod, err := podClient.Get(gs.ObjectMeta.Name, metav1.GetOptions{})
				assert.NoError(t, err)

				assert.True(t, metav1.IsControlledBy(pod, &gs))

				err = podClient.Delete(pod.ObjectMeta.Name, nil)
				assert.NoError(t, err)
			}
		}},
		"gameserver shutdown": {f: func(t *testing.T, list *v1alpha1.GameServerList) {
			for _, gs := range list.Items {
				var reply string
				reply, err := e2e.SendGameServerUDP(&gs, "EXIT")
				if err != nil {
					t.Fatalf("Could not message GameServer: %v", err)
				}

				assert.Equal(t, "ACK: EXIT\n", reply)
			}
		}},
		"gameserver unhealthy": {f: func(t *testing.T, list *v1alpha1.GameServerList) {
			for _, gs := range list.Items {
				var reply string
				reply, err := e2e.SendGameServerUDP(&gs, "UNHEALTHY")
				if err != nil {
					t.Fatalf("Could not message GameServer: %v", err)
				}

				assert.Equal(t, "ACK: UNHEALTHY\n", reply)
			}
		}},
	}

	for k, v := range tests {
		t.Run(k, func(t *testing.T) {
			alpha1 := framework.AgonesClient.StableV1alpha1()
			flt := defaultFleet()
			// add more game servers, to hunt for race conditions
			flt.Spec.Replicas = 10

			flt, err := alpha1.Fleets(defaultNs).Create(flt)
			if assert.Nil(t, err) {
				defer alpha1.Fleets(defaultNs).Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
			}

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))

			list, err := listGameServers(flt, alpha1)
			assert.NoError(t, err)
			assert.Len(t, list.Items, int(flt.Spec.Replicas))

			// apply deletion function
			logrus.Info("applying deletion function")
			v.f(t, list)

			for i, gs := range list.Items {
				err = wait.Poll(time.Second, 5*time.Minute, func() (done bool, err error) {
					_, err = alpha1.GameServers(defaultNs).Get(gs.ObjectMeta.Name, metav1.GetOptions{})

					if err != nil && k8serrors.IsNotFound(err) {
						logrus.Infof("gameserver %d/%d not found", i+1, flt.Spec.Replicas)
						return true, nil
					}

					return false, err
				})
				assert.NoError(t, err)
			}

			framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(flt.Spec.Replicas))
		})
	}
}

func listGameServers(flt *v1alpha1.Fleet, getter stablev1alpha1.GameServersGetter) (*v1alpha1.GameServerList, error) {
	selector := labels.SelectorFromSet(labels.Set{v1alpha1.FleetNameLabel: flt.ObjectMeta.Name})
	return getter.GameServers(defaultNs).List(metav1.ListOptions{LabelSelector: selector.String()})
}

// Counts the number of gameservers with the specified scheduling strategy in a fleet
func countFleetScheduling(gsList []v1alpha1.GameServer, scheduling apis.SchedulingStrategy) int {
	count := 0
	for _, gs := range gsList {
		if gs.Spec.Scheduling == scheduling {
			count++
		}
	}
	return count
}

// Patches fleet with scheduling and scale values
func schedulingFleetPatch(t *testing.T,
	f *v1alpha1.Fleet,
	scheduling apis.SchedulingStrategy,
	scale int32) *v1alpha1.Fleet {

	patch := fmt.Sprintf(`[{ "op": "replace", "path": "/spec/scheduling", "value": "%s" },
	                       { "op": "replace", "path": "/spec/replicas", "value": %d }]`,
		scheduling, scale)

	logrus.WithField("fleet", f.ObjectMeta.Name).
		WithField("scheduling", scheduling).
		WithField("scale", scale).
		WithField("patch", patch).
		Info("updating scheduling")

	fltRes, err := framework.AgonesClient.
		StableV1alpha1().
		Fleets(defaultNs).
		Patch(f.ObjectMeta.Name, types.JSONPatchType, []byte(patch))

	assert.Nil(t, err)
	return fltRes
}

func scaleAndWait(t *testing.T, flt *v1alpha1.Fleet, fleetSize int32) time.Duration {
	t0 := time.Now()
	scaleFleetSubresource(t, flt, fleetSize)
	framework.WaitForFleetCondition(t, flt, e2e.FleetReadyCount(fleetSize))
	return time.Since(t0)
}

// scaleFleetPatch creates a patch to apply to a Fleet.
// Easier for testing, as it removes object generational issues.
func scaleFleetPatch(t *testing.T, f *v1alpha1.Fleet, scale int32) *v1alpha1.Fleet {
	patch := fmt.Sprintf(`[{ "op": "replace", "path": "/spec/replicas", "value": %d }]`, scale)
	logrus.WithField("fleet", f.ObjectMeta.Name).WithField("scale", scale).WithField("patch", patch).Info("Scaling fleet")

	fltRes, err := framework.AgonesClient.StableV1alpha1().Fleets(defaultNs).Patch(f.ObjectMeta.Name, types.JSONPatchType, []byte(patch))
	assert.Nil(t, err)
	return fltRes
}

// scaleFleetSubresource uses scale subresource to change Replicas size of the Fleet.
// Returns the same f as in parameter, just to keep signature in sync with scaleFleetPatch
func scaleFleetSubresource(t *testing.T, f *v1alpha1.Fleet, scale int32) *v1alpha1.Fleet {
	logrus.WithField("fleet", f.ObjectMeta.Name).WithField("scale", scale).Info("Scaling fleet")

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		alpha1 := framework.AgonesClient.StableV1alpha1()
		// GetScale returns current Scale object with resourceVersion which is opaque object
		// and it will be used to create new Scale object
		opts := metav1.GetOptions{}
		sc, err := alpha1.Fleets(defaultNs).GetScale(f.ObjectMeta.Name, opts)
		if err != nil {
			return err
		}

		sc2 := newScale(f.Name, scale, sc.ObjectMeta.ResourceVersion)
		_, err = alpha1.Fleets(defaultNs).UpdateScale(f.ObjectMeta.Name, sc2)
		return err
	})

	if err != nil {
		t.Fatal("could not update the scale subresource")
	}
	return f
}

// defaultFleet returns a default fleet configuration
func defaultFleet() *v1alpha1.Fleet {
	gs := defaultGameServer()
	return fleetWithGameServerSpec(gs.Spec)
}

// fleetWithGameServerSpec returns a fleet with specified gameserver spec
func fleetWithGameServerSpec(gsSpec v1alpha1.GameServerSpec) *v1alpha1.Fleet {
	return &v1alpha1.Fleet{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "simple-fleet-", Namespace: defaultNs},
		Spec: v1alpha1.FleetSpec{
			Replicas: replicasCount,
			Template: v1alpha1.GameServerTemplateSpec{
				Spec: gsSpec,
			},
		},
	}
}

// newScale returns a scale with specified Replicas spec
func newScale(fleetName string, newReplicas int32, resourceVersion string) *v1betaext.Scale {
	return &v1betaext.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: fleetName, Namespace: defaultNs, ResourceVersion: resourceVersion},
		Spec: v1betaext.ScaleSpec{
			Replicas: newReplicas,
		},
	}
}
