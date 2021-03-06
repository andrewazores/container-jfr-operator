// Copyright (c) 2020 Red Hat, Inc.
//
// The Universal Permissive License (UPL), Version 1.0
//
// Subject to the condition set forth below, permission is hereby granted to any
// person obtaining a copy of this software, associated documentation and/or data
// (collectively the "Software"), free of charge and under any and all copyright
// rights in the Software, and any and all patent rights owned or freely
// licensable by each licensor hereunder covering either (i) the unmodified
// Software as contributed to or provided by such licensor, or (ii) the Larger
// Works (as defined below), to deal in both
//
// (a) the Software, and
// (b) any piece of software and/or hardware listed in the lrgrwrks.txt file if
// one is included with the Software (each a "Larger Work" to which the Software
// is contributed by such licensors),
//
// without restriction, including without limitation the rights to copy, create
// derivative works of, display, perform, and distribute the Software and make,
// use, sell, offer for sale, import, export, have made, and have sold the
// Software and the Larger Work(s), and to sublicense the foregoing rights on
// either these or other terms.
//
// This license is subject to the following condition:
// The above copyright notice and either this complete permission notice or at
// a minimum a reference to the UPL must be included in all copies or
// substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package recording

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"time"

	rhjmcv1alpha2 "github.com/rh-jmc-team/container-jfr-operator/pkg/apis/rhjmc/v1alpha2"
	jfrclient "github.com/rh-jmc-team/container-jfr-operator/pkg/client"
	common "github.com/rh-jmc-team/container-jfr-operator/pkg/controller/common"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_recording")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new Recording Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileRecording{scheme: mgr.GetScheme(),
		CommonReconciler: &common.CommonReconciler{
			Client: mgr.GetClient(),
		},
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("recording-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Recording
	err = c.Watch(&source.Kind{Type: &rhjmcv1alpha2.Recording{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileRecording implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileRecording{}

// ReconcileRecording reconciles a Recording object
type ReconcileRecording struct {
	scheme *runtime.Scheme
	*common.CommonReconciler
}

// Name used for Finalizer that handles Container JFR recording deletion
const recordingFinalizer = "recording.finalizer.rhjmc.redhat.com"

// Reconcile reads that state of the cluster for a Recording object and makes changes based on the state read
// and what is in the Recording.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileRecording) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	ctx := context.Background()
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Recording")

	cjfr, err := r.FindContainerJFR(ctx, request.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Keep client open to Container JFR as long as it doesn't fail
	if r.JfrClient == nil {
		jfrClient, err := r.ConnectToContainerJFR(ctx, cjfr.Namespace, cjfr.Name)
		if err != nil {
			// Need Container JFR in order to reconcile anything, requeue until it appears
			return reconcile.Result{}, err
		}
		r.JfrClient = jfrClient
	}

	// Fetch the Recording instance
	instance := &rhjmcv1alpha2.Recording{}
	err = r.Client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Check if this Recording is being deleted
	if instance.GetDeletionTimestamp() != nil && hasRecordingFinalizer(instance) {
		// Delete any persisted JFR file for this recording
		err := r.deleteSavedRecording(ctx, instance)
		if err != nil {
			log.Error(err, "failed to delete saved recording in Container JFR", "namespace",
				instance.Namespace, "name", instance.Name)
			return reconcile.Result{}, err
		}
	}

	// Look up FlightRecorder referenced by this Recording
	jfr, err := r.getFlightRecorder(ctx, instance)
	if err != nil {
		return reconcile.Result{}, err
	}
	if jfr == nil {
		// Check if this Recording is being deleted
		if instance.GetDeletionTimestamp() != nil && hasRecordingFinalizer(instance) {
			// Allow deletion to proceed, since no FlightRecorder/Service to clean up
			log.Info("no matching FlightRecorder, proceeding with recording deletion")
			r.removeRecordingFinalizer(ctx, instance)
		}
		// No matching FlightRecorder, don't requeue until FlightRecorder field is fixed
		return reconcile.Result{}, nil
	}

	// Look up service corresponding to this FlightRecorder object
	targetRef := jfr.Status.Target
	if targetRef == nil {
		// FlightRecorder status must not have been updated yet
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	targetSvc := &corev1.Service{}
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: targetRef.Namespace, Name: targetRef.Name}, targetSvc)
	if err != nil {
		return reconcile.Result{}, err
	}

	// If this recording is being deleted, and the service has no matching pods, we won't be able
	// to clean up any in-memory recordings. Remove the finalizer to allow deletion.
	if instance.GetDeletionTimestamp() != nil && hasRecordingFinalizer(instance) {
		endpoints := &corev1.Endpoints{}
		err = r.Client.Get(ctx, types.NamespacedName{Namespace: targetRef.Namespace, Name: targetRef.Name}, endpoints)
		if err != nil && !kerrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		if len(endpoints.Subsets) == 0 {
			log.Info("no available pod to clean up, proceeding with recording deletion")
			err = r.removeRecordingFinalizer(ctx, instance)
			if err != nil {
				return reconcile.Result{}, err
			}
			// Ready for deletion
			return reconcile.Result{}, nil
		}
	}

	// Tell Container JFR to connect to the target service
	jfrclient.ClientLock.Lock()
	defer jfrclient.ClientLock.Unlock()
	// FIXME If a service manages more than one pod, there's no guarantee that subsequent connections
	// over JMX are connecting to the same pod.
	err = r.ConnectToService(targetSvc, jfr.Status.Port)
	if err != nil {
		return reconcile.Result{}, err
	}
	defer r.DisconnectClient()

	// Check if this Recording is being deleted
	if instance.GetDeletionTimestamp() != nil {
		if hasRecordingFinalizer(instance) {
			// Delete in-memory recording in Container JFR
			err := r.deleteRecording(instance)
			if err != nil {
				log.Error(err, "failed to delete recording in Container JFR", "namespace", instance.Namespace,
					"name", instance.Name)
			}

			// Remove our finalizer only once our cleanup logic has succeeded
			err = r.removeRecordingFinalizer(ctx, instance)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
		// Ready for deletion
		return reconcile.Result{}, nil
	}

	// Add our finalizer, so we can clean up Container JFR resources upon deletion
	if !hasRecordingFinalizer(instance) {
		err := r.addRecordingFinalizer(ctx, instance)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Tell Container JFR to create the recording if not already done
	if instance.Status.State == nil { // Recording hasn't been created yet
		if instance.Spec.Duration.Duration == time.Duration(0) {
			log.Info("creating new continuous recording", "name", instance.Spec.Name, "eventOptions", instance.Spec.EventOptions)
			err = r.JfrClient.StartRecording(instance.Spec.Name, instance.Spec.EventOptions)
		} else {
			log.Info("creating new recording", "name", instance.Spec.Name, "duration", instance.Spec.Duration, "eventOptions", instance.Spec.EventOptions)
			err = r.JfrClient.DumpRecording(instance.Spec.Name, int(instance.Spec.Duration.Seconds()), instance.Spec.EventOptions)
		}
		if err != nil {
			log.Error(err, "failed to create new recording")
			r.CloseClient() // TODO maybe track an error state in the client instead of relying on calling this everywhere
			return reconcile.Result{}, err
		}
	} else if shouldStopRecording(instance) {
		log.Info("stopping recording", "name", instance.Spec.Name)
		err = r.JfrClient.StopRecording(instance.Spec.Name)
		if err != nil {
			log.Error(err, "failed to stop recording")
			r.CloseClient()
			return reconcile.Result{}, err
		}
	}

	// If the recording is found in Container JFR's list, update Recording.Status with the newest info
	log.Info("Looking for recordings for service", "service", targetSvc.Name, "namespace", targetSvc.Namespace)
	descriptor, err := r.findRecordingByName(instance.Spec.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	if descriptor != nil {
		state, err := validateRecordingState(descriptor.State)
		if err != nil {
			// TODO Likely an internal error, requeuing may not help. Status.Condition may be useful.
			log.Error(err, "unknown recording state observed from Container JFR")
			return reconcile.Result{}, err
		}
		instance.Status.State = state
		instance.Status.StartTime = metav1.Unix(0, descriptor.StartTime*int64(time.Millisecond))
		instance.Status.Duration = metav1.Duration{
			Duration: time.Duration(descriptor.Duration) * time.Millisecond,
		}
	}

	// TODO Download URLs returned by Container JFR's 'list' command currently
	// work when it is connected to the target JVM. To work around this,
	// we only include links to recordings that have been archived in persistent
	// storage.

	// Archive completed recording if requested and not already done
	isStopped := instance.Status.State != nil && *instance.Status.State == rhjmcv1alpha2.RecordingStateStopped
	if instance.Spec.Archive && instance.Status.DownloadURL == nil && isStopped {
		filename, err := r.JfrClient.SaveRecording(instance.Spec.Name)
		if err != nil {
			log.Error(err, "failed to save recording", "name", instance.Spec.Name)
			r.CloseClient()
			return reconcile.Result{}, err
		}

		downloadURL, err := r.findDownloadURL(*filename)
		if err != nil {
			return reconcile.Result{}, err
		}
		log.Info("updating download URL", "name", instance.Spec.Name, "url", downloadURL)
		instance.Status.DownloadURL = downloadURL
	}

	// Update Recording status
	err = r.Client.Status().Update(ctx, instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Requeue if the recording is still in progress
	result := reconcile.Result{}
	if !isStopped {
		// Check progress of recording after 10 seconds
		result.RequeueAfter = 10 * time.Second
	}

	reqLogger.Info("Recording successfully updated", "Namespace", instance.Namespace, "Name", instance.Name)
	return result, nil
}

func (r *ReconcileRecording) getFlightRecorder(ctx context.Context, recording *rhjmcv1alpha2.Recording) (*rhjmcv1alpha2.FlightRecorder, error) {
	jfrRef := recording.Spec.FlightRecorder
	if jfrRef == nil || len(jfrRef.Name) == 0 {
		// TODO set Condition for user/log error
		log.Info("FlightRecorder reference missing from Recording", "name", recording.Name,
			"namespace", recording.Namespace)
		return nil, nil
	}

	jfr := &rhjmcv1alpha2.FlightRecorder{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: recording.Namespace, Name: jfrRef.Name}, jfr)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// TODO set Condition for user, could be legitimate if service is deleted
			log.Info("FlightRecorder referenced from Recording not found", "name", jfrRef.Name,
				"namespace", recording.Namespace)
			return nil, nil
		}
		return nil, err
	}
	return jfr, nil
}

func (r *ReconcileRecording) findDownloadURL(filename string) (*string, error) {
	// Look for our saved recording in list from Container JFR
	savedRecordings, err := r.JfrClient.ListSavedRecordings()
	if err != nil {
		log.Error(err, "failed to list saved flight recordings")
		r.CloseClient()
		return nil, err
	}
	for idx, saved := range savedRecordings {
		if filename == saved.Name {
			return &savedRecordings[idx].DownloadURL, nil
		}
	}
	return nil, nil
}

func (r *ReconcileRecording) deleteRecording(recording *rhjmcv1alpha2.Recording) error {
	// Check if recording exists in Container JFR's in-memory list
	recName := recording.Spec.Name
	found, err := r.findRecordingByName(recName)
	if err != nil {
		return err
	}
	if found != nil {
		// Found matching recording, delete it
		err = r.JfrClient.DeleteRecording(recName)
		if err != nil {
			r.CloseClient()
			return err
		}
		log.Info("recording successfully deleted", "name", recName)
	}
	return nil
}

func (r *ReconcileRecording) deleteSavedRecording(ctx context.Context, recording *rhjmcv1alpha2.Recording) error {
	if recording.Status.DownloadURL != nil {
		// Grab JFR file base name
		downloadURL, err := url.Parse(*recording.Status.DownloadURL)
		if err != nil {
			return err
		}
		jfrFile := path.Base(downloadURL.Path)

		// Look for this JFR file within Container JFR's list of saved recordings
		found, err := r.findDownloadURL(jfrFile)
		if err != nil {
			return err
		}

		if found != nil {
			// JFR file exists, so delete it
			err = r.JfrClient.DeleteSavedRecording(jfrFile)
			if err != nil {
				r.CloseClient()
				return err
			}
			log.Info("saved recording successfully deleted", "file", jfrFile)
		}
	}
	return nil
}

func (r *ReconcileRecording) addRecordingFinalizer(ctx context.Context, recording *rhjmcv1alpha2.Recording) error {
	log.Info("adding finalizer for recording", "namespace", recording.Namespace, "name", recording.Name)
	finalizers := append(recording.GetFinalizers(), recordingFinalizer)
	recording.SetFinalizers(finalizers)

	err := r.Client.Update(ctx, recording)
	if err != nil {
		log.Error(err, "failed to add finalizer to recording", "namespace", recording.Namespace,
			"name", recording.Name)
		return err
	}
	return nil
}

func (r *ReconcileRecording) removeRecordingFinalizer(ctx context.Context, recording *rhjmcv1alpha2.Recording) error {
	finalizers := recording.GetFinalizers()
	foundIdx := -1
	for idx, finalizer := range finalizers {
		if finalizer == recordingFinalizer {
			foundIdx = idx
			break
		}
	}

	if foundIdx >= 0 {
		// Remove our finalizer from the slice
		finalizers = append(finalizers[:foundIdx], finalizers[foundIdx+1:]...)
		recording.SetFinalizers(finalizers)
		err := r.Client.Update(ctx, recording)
		if err != nil {
			log.Error(err, "failed to remove finalizer from recording", "namespace", recording.Namespace,
				"name", recording.Name)
			return err
		}
	}
	return nil
}

func (r *ReconcileRecording) findRecordingByName(name string) (*jfrclient.RecordingDescriptor, error) {
	// Get an updated list of in-memory flight recordings
	descriptors, err := r.JfrClient.ListRecordings()
	if err != nil {
		log.Error(err, "failed to list flight recordings", "name", name)
		r.CloseClient()
		return nil, err
	}

	for idx, recording := range descriptors {
		if recording.Name == name {
			return &descriptors[idx], nil
		}
	}
	return nil, nil
}

func validateRecordingState(state string) (*rhjmcv1alpha2.RecordingState, error) {
	convState := rhjmcv1alpha2.RecordingState(state)
	switch convState {
	case rhjmcv1alpha2.RecordingStateCreated,
		rhjmcv1alpha2.RecordingStateRunning,
		rhjmcv1alpha2.RecordingStateStopping,
		rhjmcv1alpha2.RecordingStateStopped:
		return &convState, nil
	}
	return nil, fmt.Errorf("unknown recording state %s", state)
}

func shouldStopRecording(recording *rhjmcv1alpha2.Recording) bool {
	// Need to know user's request, and current state of recording
	requested := recording.Spec.State
	current := recording.Status.State
	if requested == nil || current == nil {
		return false
	}

	// Should stop if user wants recording stopped and we're not already doing/done so
	return *requested == rhjmcv1alpha2.RecordingStateStopped && *current != rhjmcv1alpha2.RecordingStateStopped &&
		*current != rhjmcv1alpha2.RecordingStateStopping
}

func hasRecordingFinalizer(recording *rhjmcv1alpha2.Recording) bool {
	for _, finalizer := range recording.GetFinalizers() {
		if finalizer == recordingFinalizer {
			return true
		}
	}
	return false
}
