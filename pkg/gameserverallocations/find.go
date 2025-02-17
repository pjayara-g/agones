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
	"math/rand"

	"agones.dev/agones/pkg/apis"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	stablev1alpha1 "agones.dev/agones/pkg/apis/stable/v1alpha1"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// findGameServerForAllocation finds an optimal gameserver, given the
// set of preferred and required selectors on the GameServerAllocation. This also returns the index
// that the gameserver was found at in `list`, in case you want to remove it from the list
// Packed: will search list from start to finish
// Distributed: will search in a random order through the list
// It is assumed that all gameservers passed in, are Ready and not being deleted, and are sorted in Packed priority order
func findGameServerForAllocation(gsa *allocationv1.GameServerAllocation, list []*stablev1alpha1.GameServer) (*stablev1alpha1.GameServer, int, error) {
	type result struct {
		gs    *stablev1alpha1.GameServer
		index int
	}

	requiredSelector, err := metav1.LabelSelectorAsSelector(&gsa.Spec.Required)
	if err != nil {
		return nil, -1, errors.Wrap(err, "could not convert GameServerAllocation selector")
	}

	preferredSelector, err := gsa.Spec.PreferredSelectors()
	if err != nil {
		return nil, -1, errors.Wrap(err, "could not convert preferred selectors for GameServerAllocation")
	}

	var required *result
	preferred := make([]*result, len(preferredSelector))

	var loop func(list []*stablev1alpha1.GameServer, f func(i int, gs *stablev1alpha1.GameServer))

	// packed is forward looping, distributed is random looping
	switch gsa.Spec.Scheduling {
	case apis.Packed:
		loop = func(list []*stablev1alpha1.GameServer, f func(i int, gs *stablev1alpha1.GameServer)) {
			for i, gs := range list {
				f(i, gs)
			}
		}
	case apis.Distributed:
		// randomised looping - make a list of indices, and then randomise them
		// as we don't want to change the order of the gameserver slice
		l := len(list)
		indices := make([]int, l)
		for i := 0; i < l; i++ {
			indices[i] = i
		}
		rand.Shuffle(l, func(i, j int) {
			indices[i], indices[j] = indices[j], indices[i]
		})

		loop = func(list []*stablev1alpha1.GameServer, f func(i int, gs *stablev1alpha1.GameServer)) {
			for _, i := range indices {
				f(i, list[i])
			}
		}
	default:
		return nil, -1, errors.Errorf("scheduling strategy of '%s' is not supported", gsa.Spec.Scheduling)
	}

	loop(list, func(i int, gs *stablev1alpha1.GameServer) {
		// only search the same namespace
		if gs.ObjectMeta.Namespace != gsa.ObjectMeta.Namespace {
			return
		}

		set := labels.Set(gs.ObjectMeta.Labels)

		// first look at preferred
		for j, sel := range preferredSelector {
			if preferred[j] == nil && sel.Matches(set) {
				preferred[j] = &result{gs: gs, index: i}
			}
		}

		// then look at required
		if required == nil && requiredSelector.Matches(set) {
			required = &result{gs: gs, index: i}
		}
	})

	for _, r := range preferred {
		if r != nil {
			return r.gs, r.index, nil
		}
	}

	if required == nil {
		return nil, 0, ErrNoGameServerReady
	}

	return required.gs, required.index, nil
}
