// Copyright 2021 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package sharing

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/go-logr/logr"
	sgv1alpha1 "github.com/vmware-tanzu/carvel-secretgen-controller/pkg/apis/secretgen/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// SecretReconciler watches Secret resources.
// If a Secret is recognized to be a placeholder secret for image pull secrets
// it gets filled with a combined image pull secret that matched
// import criteria for that Secret.
type SecretReconciler struct {
	client        client.Client
	secretExports *SecretExports
	log           logr.Logger
}

var _ reconcile.Reconciler = &SecretReconciler{}

func NewSecretReconciler(client client.Client,
	secretExports *SecretExports, log logr.Logger) *SecretReconciler {
	return &SecretReconciler{client, secretExports, log}
}

func (r *SecretReconciler) AttachWatches(controller controller.Controller) error {
	err := controller.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("Watching secrets: %s", err)
	}

	return controller.Watch(&source.Kind{Type: &sgv1alpha1.SecretExport{}}, &enqueueSecretExportToSecret{
		SecretExports: r.secretExports,
		ToRequests:    handler.ToRequestsFunc(r.mapSecretExportToSecret),
		Log:           r.log,
	})
}

func (r *SecretReconciler) mapSecretExportToSecret(a handler.MapObject) []reconcile.Request {
	var secretList corev1.SecretList

	// TODO expensive call on every secret export update
	err := r.client.List(context.TODO(), &secretList)
	if err != nil {
		// TODO what should we really do here?
		r.log.Error(err, "Failed fetching list of all secrets")
		return nil
	}

	var result []reconcile.Request
	for _, secret := range secretList.Items {
		if r.predictWantToReconcile(secret) {
			result = append(result, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      secret.Name,
					Namespace: secret.Namespace,
				},
			})
		}
	}

	r.log.Info("Planning to reconcile matched secrets",
		"all", len(secretList.Items), "matched", len(result))

	return result
}

func (r *SecretReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("request", request)
	log.Info("Reconciling")

	var secret corev1.Secret

	err := r.client.Get(context.TODO(), request.NamespacedName, &secret)
	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{Requeue: true}, err
	}

	if secret.DeletionTimestamp != nil {
		// Nothing to do
		return reconcile.Result{}, nil
	}

	return r.reconcile(secret, *secret.DeepCopy(), log)
}

const (
	imagePullSecretAnnKey = "secretgen.carvel.dev/image-pull-secret"
)

func (r *SecretReconciler) predictWantToReconcile(secret corev1.Secret) bool {
	_, found := secret.Annotations[imagePullSecretAnnKey]
	return found
}

func (r *SecretReconciler) reconcile(secret, originalSecret corev1.Secret, log logr.Logger) (reconcile.Result, error) {
	if _, found := secret.Annotations[imagePullSecretAnnKey]; !found {
		return reconcile.Result{}, nil
	}

	log.Info("Reconciling secret with annotation " + imagePullSecretAnnKey)

	// Note that "type" is immutable on a secret
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		status := SecretStatus{
			Conditions: []sgv1alpha1.Condition{{
				Type:    sgv1alpha1.ReconcileFailed,
				Status:  corev1.ConditionTrue,
				Message: "Expected secret to have type=corev1.SecretTypeDockerConfigJson, but did not",
			}},
		}
		return r.updateSecret(secret, status, originalSecret)
	}

	matcher := SecretMatcher{Namespace: secret.Namespace, SecretType: secret.Type}
	secrets := r.secretExports.MatchedSecretsForImport(matcher)

	newData, err := NewCombinedDockerConfigJSON(secrets)
	if err != nil {
		return reconcile.Result{RequeueAfter: 3 * time.Second}, err
	}

	secret.Data = newData

	status := SecretStatus{
		Conditions: []sgv1alpha1.Condition{{
			Type:   sgv1alpha1.ReconcileSucceeded,
			Status: corev1.ConditionTrue,
		}},
		SecretNames: r.statusSecretNames(secrets),
	}

	return r.updateSecret(secret, status, originalSecret)
}

func (r *SecretReconciler) updateSecret(secret corev1.Secret, status SecretStatus,
	originalSecret corev1.Secret) (reconcile.Result, error) {

	const (
		statusFieldAnnKey = "secretgen.carvel.dev/status"
	)

	encodedStatus, err := json.Marshal(status)
	if err != nil {
		panic(fmt.Sprintf("Internal inconsistency: failed to marshal secret status: %s", err))
	}

	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[statusFieldAnnKey] = string(encodedStatus)

	if reflect.DeepEqual(secret, originalSecret) {
		// Nothing changed leave early
		return reconcile.Result{}, nil
	}

	// TODO bother to retry to avoid having to recalculate matched secrets?
	err = r.client.Update(context.TODO(), &secret)
	if err != nil {
		// Requeue to try to update a bit later
		return reconcile.Result{Requeue: true}, fmt.Errorf("Updating secret: %s", err)
	}

	return reconcile.Result{}, nil
}

type SecretStatus struct {
	Conditions  []sgv1alpha1.Condition `json:"conditions,omitempty"`
	SecretNames []string               `json:"secretNames,omitempty"`
}

func (*SecretReconciler) statusSecretNames(secrets []*corev1.Secret) []string {
	var result []string
	for _, secret := range secrets {
		result = append(result, secret.Namespace+"/"+secret.Name)
	}
	sort.Strings(result)
	return result
}

// enqueueSecretExportToSecret is a custom handler that is optimized for
// tracking SecretExport events. It tries to result in minimum number of
// Secret reconile requests.
type enqueueSecretExportToSecret struct {
	SecretExports *SecretExports
	ToRequests    handler.Mapper
	Log           logr.Logger
}

// Create does not do anything since SecretExport's status
// will be updated when it's ready to be consumed
func (e *enqueueSecretExportToSecret) Create(evt event.CreateEvent, q workqueue.RateLimitingInterface) {
}

// Update only enqueues when SecretExport's status has changed
func (e *enqueueSecretExportToSecret) Update(evt event.UpdateEvent, q workqueue.RateLimitingInterface) {
	typedExportOld, okOld := evt.ObjectOld.(*sgv1alpha1.SecretExport)
	typedExportNew, okNew := evt.ObjectNew.(*sgv1alpha1.SecretExport)
	if okOld && okNew && reflect.DeepEqual(typedExportOld.Status, typedExportNew.Status) {
		e.Log.Info("Skipping SecretExport update since status did not change")
		return // Skip when status of SecretExport did not change
	}
	e.mapAndEnqueue(q, handler.MapObject{Meta: evt.MetaNew, Object: evt.ObjectNew})
}

// Delete always enqueues but first clears the export cache
func (e *enqueueSecretExportToSecret) Delete(evt event.DeleteEvent, q workqueue.RateLimitingInterface) {
	// TODO this does not belong here from "layering" perspective
	// however it's currently necessary because SecretReconciler
	// may react to deleted secret export before SecretExports reconciler
	// (which also clears the shared cache).
	e.SecretExports.Unexport(&sgv1alpha1.SecretExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      evt.Meta.GetName(),
			Namespace: evt.Meta.GetNamespace(),
		},
	})
	e.mapAndEnqueue(q, handler.MapObject{Meta: evt.Meta, Object: evt.Object})
}

// Generic does not do anything
func (e *enqueueSecretExportToSecret) Generic(evt event.GenericEvent, q workqueue.RateLimitingInterface) {
}

func (e *enqueueSecretExportToSecret) mapAndEnqueue(q workqueue.RateLimitingInterface, object handler.MapObject) {
	for _, req := range e.ToRequests.Map(object) {
		q.Add(req)
	}
}
