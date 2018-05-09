// Copyright 2018 Google Inc. All Rights Reserved.
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

package fleets

import (
	"encoding/json"
	"reflect"

	"agones.dev/agones/pkg/apis/stable"
	stablev1alpha1 "agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	getterv1alpha1 "agones.dev/agones/pkg/client/clientset/versioned/typed/stable/v1alpha1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1alpha1 "agones.dev/agones/pkg/client/listers/stable/v1alpha1"
	"agones.dev/agones/pkg/util/crd"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/webhooks"
	"agones.dev/agones/pkg/util/workerqueue"
	"github.com/heptiolabs/healthcheck"
	"github.com/mattbaird/jsonpatch"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

// Controller is a the GameServerSet controller
type Controller struct {
	logger              *logrus.Entry
	crdGetter           v1beta1.CustomResourceDefinitionInterface
	gameServerSetGetter getterv1alpha1.GameServerSetsGetter
	gameServerSetLister listerv1alpha1.GameServerSetLister
	gameServerSetSynced cache.InformerSynced
	fleetGetter         getterv1alpha1.FleetsGetter
	fleetLister         listerv1alpha1.FleetLister
	fleetSynced         cache.InformerSynced
	workerqueue         *workerqueue.WorkerQueue
	recorder            record.EventRecorder
}

// NewController returns a new fleets crd controller
func NewController(
	wh *webhooks.WebHook,
	health healthcheck.Handler,
	kubeClient kubernetes.Interface,
	extClient extclientset.Interface,
	agonesClient versioned.Interface,
	agonesInformerFactory externalversions.SharedInformerFactory) *Controller {

	gameServerSets := agonesInformerFactory.Stable().V1alpha1().GameServerSets()
	gsSetInformer := gameServerSets.Informer()

	fleets := agonesInformerFactory.Stable().V1alpha1().Fleets()
	fInformer := fleets.Informer()

	c := &Controller{
		crdGetter:           extClient.ApiextensionsV1beta1().CustomResourceDefinitions(),
		gameServerSetGetter: agonesClient.StableV1alpha1(),
		gameServerSetLister: gameServerSets.Lister(),
		gameServerSetSynced: gsSetInformer.HasSynced,
		fleetGetter:         agonesClient.StableV1alpha1(),
		fleetLister:         fleets.Lister(),
		fleetSynced:         fInformer.HasSynced,
	}

	c.logger = runtime.NewLoggerWithType(c)
	c.workerqueue = workerqueue.NewWorkerQueue(c.syncFleet, c.logger, stable.GroupName+".FleetController")
	health.AddLivenessCheck("fleet-workerqueue", healthcheck.Check(c.workerqueue.Healthy))

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(c.logger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	c.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "fleet-controller"})

	wh.AddHandler("/mutate", stablev1alpha1.Kind("Fleet"), admv1beta1.Create, c.creationMutationHandler)

	fInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.workerqueue.Enqueue,
		UpdateFunc: func(_, newObj interface{}) {
			c.workerqueue.Enqueue(newObj)
		},
	})

	gsSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.gameServerSetEventHandler,
		UpdateFunc: func(_, newObj interface{}) {
			gsSet := newObj.(*stablev1alpha1.GameServerSet)
			// ignore if already being deleted
			if gsSet.ObjectMeta.DeletionTimestamp.IsZero() {
				c.gameServerSetEventHandler(gsSet)
			}
		},
	})

	return c
}

// creationMutationHandler is the handler for the mutating webhook that sets the
// the default values on the Fleet
// Should only be called on fleet create operations.
func (c *Controller) creationMutationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	c.logger.WithField("review", review).Info("creationMutationHandler")

	obj := review.Request.Object
	fleet := &stablev1alpha1.Fleet{}
	err := json.Unmarshal(obj.Raw, fleet)
	if err != nil {
		return review, errors.Wrapf(err, "error unmarshalling original Fleet json: %s", obj.Raw)
	}

	// This is the main logic of this function
	// the rest is really just json plumbing
	fleet.ApplyDefaults()

	newFleet, err := json.Marshal(fleet)
	if err != nil {
		return review, errors.Wrapf(err, "error marshalling default applied Fleet %s to json", fleet.ObjectMeta.Name)
	}

	patch, err := jsonpatch.CreatePatch(obj.Raw, newFleet)
	if err != nil {
		return review, errors.Wrapf(err, "error creating patch for Fleet %s", fleet.ObjectMeta.Name)
	}

	json, err := json.Marshal(patch)
	if err != nil {
		return review, errors.Wrapf(err, "error creating json for patch for Fleet %s", fleet.ObjectMeta.Name)
	}

	c.logger.WithField("fleet", fleet.ObjectMeta.Name).WithField("patch", string(json)).Infof("patch created!")

	pt := admv1beta1.PatchTypeJSONPatch
	review.Response.PatchType = &pt
	review.Response.Patch = json

	return review, nil
}

// Run the Fleet controller. Will block until stop is closed.
// Runs threadiness number workers to process the rate limited queue
func (c *Controller) Run(threadiness int, stop <-chan struct{}) error {
	err := crd.WaitForEstablishedCRD(c.crdGetter, "fleets.stable.agones.dev", c.logger)
	if err != nil {
		return err
	}

	c.logger.Info("Wait for cache sync")
	if !cache.WaitForCacheSync(stop, c.gameServerSetSynced, c.fleetSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	c.workerqueue.Run(threadiness, stop)
	return nil
}

// gameServerSetEventHandler enqueues the owning Fleet for this GameServerSet,
// assuming that it has one
func (c *Controller) gameServerSetEventHandler(obj interface{}) {
	gsSet := obj.(*stablev1alpha1.GameServerSet)
	ref := metav1.GetControllerOf(gsSet)
	if ref == nil {
		return
	}

	fleet, err := c.fleetLister.Fleets(gsSet.ObjectMeta.Namespace).Get(ref.Name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("ref", ref).Info("Owner Fleet no longer available for syncing")
		} else {
			runtime.HandleError(c.logger.WithField("fleet", fleet.ObjectMeta.Name).WithField("ref", ref),
				errors.Wrap(err, "error retrieving GameServerSet owner"))
		}
		return
	}
	c.workerqueue.Enqueue(fleet)
}

// syncFleet synchronised the fleet CRDs and configures/updates
// backing GameServerSets
func (c *Controller) syncFleet(key string) error {
	c.logger.WithField("key", key).Info("Synchronising")

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// don't return an error, as we don't want this retried
		runtime.HandleError(c.logger.WithField("key", key), errors.Wrapf(err, "invalid resource key"))
		return nil
	}

	fleet, err := c.fleetLister.Fleets(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("key", key).Info("Fleet is no longer available for syncing")
			return nil
		}
		return errors.Wrapf(err, "error retrieving fleet %s from namespace %s", name, namespace)
	}

	list, err := ListGameServerSetsByFleetOwner(c.gameServerSetLister, fleet)
	if err != nil {
		return err
	}

	activeGsSet, rest := c.filterGameServerSetByActive(fleet, list)
	if err := c.applyDeploymentStrategy(fleet, rest); err != nil {
		return err
	}
	if err := c.deleteEmptyGameServerSets(fleet, rest); err != nil {
		return err
	}

	// if there isn't an active gameServerSet, create one (but don't persist yet)
	if activeGsSet == nil {
		c.logger.WithField("fleet", fleet.ObjectMeta.Name).Info("could not find active GameServerSet, creating")
		activeGsSet = fleet.GameServerSet()
	}

	replicas := fleet.ReplicasMinusSumAllocated(rest)
	if err := c.upsertGameServerSet(fleet, activeGsSet, replicas); err != nil {
		return err
	}
	return c.updateFleetStatus(fleet)
}

// upsertGameServerSet if the GameServerSet is new, insert it
// if the replicas minus sum allocated does not match the active
// GameServerSet, then update it
func (c *Controller) upsertGameServerSet(fleet *stablev1alpha1.Fleet, gsSet *stablev1alpha1.GameServerSet, replicas int32) error {
	if gsSet.ObjectMeta.UID == "" {
		gsSet.Spec.Replicas = replicas
		gsSet, err := c.gameServerSetGetter.GameServerSets(gsSet.ObjectMeta.Namespace).Create(gsSet)
		if err != nil {
			return errors.Wrapf(err, "error creating gameserverset for fleet %s", fleet.ObjectMeta.Name)
		}

		c.recorder.Eventf(fleet, corev1.EventTypeNormal, "CreatingGameServerSet",
			"Created GameServerSet %s", gsSet.ObjectMeta.Name)
		return nil
	}

	if replicas != gsSet.Spec.Replicas {
		gsSetCopy := gsSet.DeepCopy()
		gsSetCopy.Spec.Replicas = replicas
		gsSetCopy, err := c.gameServerSetGetter.GameServerSets(fleet.ObjectMeta.Namespace).Update(gsSetCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating replicas for gameserverset for fleet %s", fleet.ObjectMeta.Name)
		}
		c.recorder.Eventf(fleet, corev1.EventTypeNormal, "ScalingGameServerSet",
			"Scaling active GameServerSet %s from %d to %d", gsSetCopy.ObjectMeta.Name, gsSet.Spec.Replicas, gsSetCopy.Spec.Replicas)
	}

	return nil
}

// applyDeploymentStrategy applies the Fleet > Spec > Deployment strategy to all the non-active
// GameServerSets that are passed in
func (c *Controller) applyDeploymentStrategy(fleet *stablev1alpha1.Fleet, list []*stablev1alpha1.GameServerSet) error {
	switch fleet.Spec.Strategy.Type {
	case appsv1.RecreateDeploymentStrategyType:
		return c.recreateDeployment(list)
	case appsv1.RollingUpdateDeploymentStrategyType:
		// in this PR, we are only implementing Recreate.
		// we will do this next
		panic("this is not implemented!")
	}

	return errors.Errorf("unexpected deployment strategy type: %s", fleet.Spec.Strategy.Type)
}

// deleteEmptyGameServerSets deletes all GameServerServerSets
// That have `Status > Replicas` of 0
func (c *Controller) deleteEmptyGameServerSets(fleet *stablev1alpha1.Fleet, list []*stablev1alpha1.GameServerSet) error {
	p := metav1.DeletePropagationBackground
	for _, gsSet := range list {
		if gsSet.Status.Replicas == 0 {
			err := c.gameServerSetGetter.GameServerSets(gsSet.ObjectMeta.Namespace).Delete(gsSet.ObjectMeta.Name, &metav1.DeleteOptions{PropagationPolicy: &p})
			if err != nil {
				return errors.Wrapf(err, "error updating gameserverset %s", gsSet.ObjectMeta.Name)
			}

			c.recorder.Eventf(fleet, corev1.EventTypeNormal, "DeletingGameServerSet", "Deleting inactive GameServerSet %s", gsSet.ObjectMeta.Name)
		}
	}

	return nil
}

// recreateDeployment applies the recreate deployment strategy to all non-active
// GameServerSets
func (c *Controller) recreateDeployment(list []*stablev1alpha1.GameServerSet) error {
	for _, gsSet := range list {
		if gsSet.Spec.Replicas != 0 {
			c.logger.WithField("gameserverset", gsSet.ObjectMeta.Name).Info("applying recreate deployment: scaling to 0")
			gsSetCopy := gsSet.DeepCopy()
			gsSetCopy.Spec.Replicas = 0
			if _, err := c.gameServerSetGetter.GameServerSets(gsSetCopy.ObjectMeta.Namespace).Update(gsSetCopy); err != nil {
				return errors.Wrapf(err, "error updating gameserverset %s", gsSetCopy.ObjectMeta.Name)
			}
		}
	}

	return nil
}

// updateFleetStatus gets the GameServerSets for this Fleet and then
// calculates the counts for the status, and updates the Fleet
func (c *Controller) updateFleetStatus(fleet *stablev1alpha1.Fleet) error {
	list, err := ListGameServerSetsByFleetOwner(c.gameServerSetLister, fleet)
	if err != nil {
		return err
	}

	fCopy := fleet.DeepCopy()
	fCopy.Status.Replicas = 0
	fCopy.Status.ReadyReplicas = 0
	fCopy.Status.AllocatedReplicas = 0

	for _, gsSet := range list {
		fCopy.Status.Replicas += gsSet.Status.Replicas
		fCopy.Status.ReadyReplicas += gsSet.Status.ReadyReplicas
		fCopy.Status.AllocatedReplicas += gsSet.Status.AllocatedReplicas
	}

	_, err = c.fleetGetter.Fleets(fCopy.Namespace).Update(fCopy)
	return errors.Wrapf(err, "error updating status of fleet %s", fCopy.ObjectMeta.Name)
}

// filterGameServerSetByActive returns the active GameServerSet (or nil if it
// doesn't exist) and then the rest of the GameServerSets that are controlled
// by this Fleet
func (c *Controller) filterGameServerSetByActive(fleet *stablev1alpha1.Fleet, list []*stablev1alpha1.GameServerSet) (*stablev1alpha1.GameServerSet, []*stablev1alpha1.GameServerSet) {
	var active *stablev1alpha1.GameServerSet
	var rest []*stablev1alpha1.GameServerSet

	for _, gsSet := range list {
		if reflect.DeepEqual(gsSet.Spec.Template, fleet.Spec.Template) {
			active = gsSet
		} else {
			rest = append(rest, gsSet)
		}
	}

	return active, rest
}