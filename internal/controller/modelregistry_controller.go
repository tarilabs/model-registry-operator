/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	klog "sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"text/template"

	modelregistryv1alpha1 "github.com/opendatahub-io/model-registry-operator/api/v1alpha1"
)

const modelRegistryFinalizer = "modelregistry.opendatahub.io/finalizer"

// Definitions to manage status conditions
const (
	// ConditionTypeAvailable represents the status of the Deployment reconciliation
	ConditionTypeAvailable = "Available"
	// ConditionTypeProgressing represents the status used when the custom resource is being deployed.
	ConditionTypeProgressing = "Progressing"
	// ConditionTypeDegraded represents the status used when the custom resource is deleted and the finalizer operations must occur.
	ConditionTypeDegraded = "Degraded"

	ReasonCreated     = "CreatedDeployment"
	ReasonCreating    = "CreatingDeployment"
	ReasonUpdating    = "UpdatingDeployment"
	ReasonAvailable   = "DeploymentAvailable"
	ReasonUnavailable = "DeploymentUnavailable"
)

// ModelRegistryReconciler reconciles a ModelRegistry object
type ModelRegistryReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	Log            logr.Logger
	Template       *template.Template
	EnableWebhooks bool
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ModelRegistry object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.0/pkg/reconcile
func (r *ModelRegistryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	modelRegistry := &modelregistryv1alpha1.ModelRegistry{}
	err := r.Get(ctx, req.NamespacedName, modelRegistry)
	if err != nil {
		if errors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("modelregistry resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get modelregistry")
		return ctrl.Result{}, err
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(modelRegistry, modelRegistryFinalizer) {
		log.Info("Adding Finalizer for ModelRegistry")
		if ok := controllerutil.AddFinalizer(modelRegistry, modelRegistryFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			return ctrl.Result{Requeue: true}, nil
		}

		if err = r.Update(ctx, modelRegistry); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the modelRegistry instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isMarkedToBeDeleted := modelRegistry.GetDeletionTimestamp() != nil
	if isMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(modelRegistry, modelRegistryFinalizer) {
			log.Info("Performing Finalizer Operations for modelRegistry before delete CR")

			// Let's add here an status "Degraded" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&modelRegistry.Status.Conditions, metav1.Condition{Type: ConditionTypeDegraded,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", modelRegistry.Name)})

			if err = r.Status().Update(ctx, modelRegistry); IgnoreDeletingErrors(err) != nil {
				switch t := err.(type) {
				case *errors.StatusError:
					log.Error(err, "status error", "status", t.Status())
				}
				log.Error(err, "Failed to update modelRegistry status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForModelRegistry(modelRegistry)

			// TODO(user): If you add operations to the doFinalizerOperationsForModelRegistry method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the modelRegistry Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err = r.Get(ctx, req.NamespacedName, modelRegistry); IgnoreDeletingErrors(err) != nil {
				log.Error(err, "Failed to re-fetch modelRegistry")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&modelRegistry.Status.Conditions, metav1.Condition{Type: ConditionTypeDegraded,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s were successfully accomplished", modelRegistry.Name)})

			if err = r.Status().Update(ctx, modelRegistry); IgnoreDeletingErrors(err) != nil {
				log.Error(err, "Failed to update modelRegistry status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for modelRegistry after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(modelRegistry, modelRegistryFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for modelRegistry")
				return ctrl.Result{Requeue: true}, nil
			}

			if err = r.Update(ctx, modelRegistry); IgnoreDeletingErrors(err) != nil {
				log.Error(err, "Failed to remove finalizer for modelRegistry")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// set defaults if not using webhooks
	if !r.EnableWebhooks {
		modelRegistry.Default()
	}

	params := &ModelRegistryParams{
		Name:      req.Name,
		Namespace: req.Namespace,
		Spec:      modelRegistry.Spec,
	}

	// update registry service
	result, err := r.updateRegistryResources(ctx, params, modelRegistry)
	if err != nil {
		log.Error(err, "service reconcile error")
		return ctrl.Result{}, err
	}
	log.Info("service reconciled", "status", result)
	r.logResultAsEvent(modelRegistry, result)

	// set custom resource status
	if err = r.setRegistryStatus(ctx, req, result); err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	log.Info("status reconciled")

	if result != ResourceUnchanged {
		// requeue to update status
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

func IgnoreDeletingErrors(err error) error {
	if err == nil {
		return nil
	}
	if errors.IsNotFound(err) || errors.IsConflict(err) {
		return nil
	}
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelRegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&modelregistryv1alpha1.ModelRegistry{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

//+kubebuilder:rbac:groups=modelregistry.opendatahub.io,resources=modelregistries,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=modelregistry.opendatahub.io,resources=modelregistries/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=modelregistry.opendatahub.io,resources=modelregistries/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete

func (r *ModelRegistryReconciler) updateRegistryResources(ctx context.Context, params *ModelRegistryParams, registry *modelregistryv1alpha1.ModelRegistry) (OperationResult, error) {
	var result, result2, result3 OperationResult

	var err error
	result, err = r.createOrUpdateServiceAccount(ctx, params, registry, "serviceaccount.yaml.tmpl")
	if err != nil {
		return result, err
	}

	result2, err = r.createOrUpdateService(ctx, params, registry, "service.yaml.tmpl")
	if err != nil {
		return result2, err
	}
	if result2 != ResourceUnchanged {
		result = result2
	}

	result3, err = r.createOrUpdateDeployment(ctx, params, registry, "deployment.yaml.tmpl")
	if err != nil {
		return result3, err
	}
	if result3 != ResourceUnchanged {
		result = result3
	}

	return result, nil
}

func (r *ModelRegistryReconciler) setRegistryStatus(ctx context.Context, req ctrl.Request, operationResult OperationResult) error {
	log := klog.FromContext(ctx)

	modelRegistry := &modelregistryv1alpha1.ModelRegistry{}
	if err := r.Get(ctx, req.NamespacedName, modelRegistry); err != nil {
		log.Error(err, "Failed to re-fetch modelRegistry")
		return err
	}

	status := metav1.ConditionTrue
	reason := ReasonCreated
	message := "Deployment for custom resource %s was successfully created"
	switch operationResult {
	case ResourceCreated:
		status = metav1.ConditionFalse
		reason = ReasonCreating
		message = "Creating deployment for custom resource %s"
	case ResourceUpdated:
		status = metav1.ConditionFalse
		reason = ReasonUpdating
		message = "Updating deployment for custom resource %s"
	}

	meta.SetStatusCondition(&modelRegistry.Status.Conditions, metav1.Condition{Type: ConditionTypeProgressing,
		Status: status, Reason: reason,
		Message: fmt.Sprintf(message, modelRegistry.Name)})

	// determine registry available condition
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		log.Error(err, "Failed to get modelRegistry deployment", "name", req.NamespacedName)
		return err
	}
	log.V(10).Info("Found service deployment", "name", len(deployment.Name))

	// check deployment availability
	available := false
	for _, c := range deployment.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			available = c.Status == corev1.ConditionTrue
			break
		}
	}

	if available {
		status = metav1.ConditionTrue
		reason = ReasonAvailable
		message = "Deployment for custom resource %s is available"
	} else {
		status = metav1.ConditionFalse
		reason = ReasonUnavailable
		message = "Deployment for custom resource %s is not available"
	}
	meta.SetStatusCondition(&modelRegistry.Status.Conditions, metav1.Condition{Type: ConditionTypeAvailable,
		Status: status, Reason: reason,
		Message: fmt.Sprintf(message, modelRegistry.Name)})

	if err := r.Status().Update(ctx, modelRegistry); err != nil {
		log.Error(err, "Failed to update modelRegistry status")
		return err
	}
	return nil
}

func (r *ModelRegistryReconciler) createOrUpdateDeployment(ctx context.Context, params *ModelRegistryParams,
	registry *modelregistryv1alpha1.ModelRegistry, templateName string) (result OperationResult, err error) {
	result = ResourceUnchanged
	var deployment appsv1.Deployment
	if err = r.Apply(params, templateName, &deployment); err != nil {
		return result, err
	}
	if err = ctrl.SetControllerReference(registry, &deployment, r.Scheme); err != nil {
		return result, err
	}

	result, err = r.createOrUpdate(ctx, deployment.DeepCopy(), &deployment)
	if err != nil {
		return result, err
	}
	return result, nil
}

func (r *ModelRegistryReconciler) createOrUpdateService(ctx context.Context, params *ModelRegistryParams,
	registry *modelregistryv1alpha1.ModelRegistry, templateName string) (result OperationResult, err error) {
	result = ResourceUnchanged
	var service corev1.Service
	if err = r.Apply(params, templateName, &service); err != nil {
		return result, err
	}
	if err = ctrl.SetControllerReference(registry, &service, r.Scheme); err != nil {
		return result, err
	}
	if result, err = r.createOrUpdate(ctx, service.DeepCopy(), &service); err != nil {
		return result, err
	}
	return result, nil
}

func (r *ModelRegistryReconciler) createOrUpdateServiceAccount(ctx context.Context, params *ModelRegistryParams,
	registry *modelregistryv1alpha1.ModelRegistry, templateName string) (result OperationResult, err error) {
	result = ResourceUnchanged
	var sa corev1.ServiceAccount
	if err = r.Apply(params, templateName, &sa); err != nil {
		return result, err
	}
	if err = ctrl.SetControllerReference(registry, &sa, r.Scheme); err != nil {
		return result, err
	}

	if result, err = r.createOrUpdate(ctx, sa.DeepCopy(), &sa); err != nil {
		return result, err
	}
	return result, nil
}

//go:generate go-enum -type=OperationResult
type OperationResult int

const (
	// ResourceUnchanged means that the resource has not been changed.
	ResourceUnchanged OperationResult = iota
	// ResourceCreated means that a new resource is created.
	ResourceCreated
	// ResourceUpdated means that an existing resource is updated.
	ResourceUpdated
)

func (r *ModelRegistryReconciler) createOrUpdate(ctx context.Context, currObj client.Object, newObj client.Object) (OperationResult, error) {
	log := klog.FromContext(ctx)
	result := ResourceUnchanged

	key := client.ObjectKeyFromObject(newObj)
	gvk := newObj.GetObjectKind().GroupVersionKind()
	name := newObj.GetName()

	if err := r.Client.Get(ctx, key, currObj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// create object
			result = ResourceCreated
			log.Info("creating", "kind", gvk, "name", name)
			// save last applied config in annotation similar to kubectl apply
			if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(newObj); err != nil {
				return result, err
			}
			return result, r.Client.Create(ctx, newObj)
		}
		// get error
		return result, err
	}

	// hack: envtest is missing typemeta for some reason, hence the ignores for apiVersion and kind!!!
	// create a patch by comparing objects
	patchResult, err := patch.DefaultPatchMaker.Calculate(currObj, newObj, patch.IgnoreStatusFields(),
		patch.IgnoreField("apiVersion"), patch.IgnoreField("kind"))
	if err != nil {
		return result, err
	}
	if !patchResult.IsEmpty() {
		// update object
		result = ResourceUpdated
		log.Info("updating", "kind", gvk, "name", name)
		// update last applied config in annotation
		if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(newObj); err != nil {
			return result, err
		}
		return result, r.Client.Update(ctx, newObj)
	}

	return result, nil
}

// finalizeMemcached will perform the required operations before delete the CR.
func (r *ModelRegistryReconciler) doFinalizerOperationsForModelRegistry(registry *modelregistryv1alpha1.ModelRegistry) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These, such as the Deployment created on this reconcile,
	// are defined as depended on the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(registry, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			registry.Name,
			registry.Namespace))
}

// wrapper for template parameters
type ModelRegistryParams struct {
	Name      string
	Namespace string
	Spec      modelregistryv1alpha1.ModelRegistrySpec
}

// executes given template name with params
func (r *ModelRegistryReconciler) Apply(params *ModelRegistryParams, templateName string, object interface{}) error {
	builder := strings.Builder{}
	err := r.Template.ExecuteTemplate(&builder, templateName, params)
	if err != nil {
		return fmt.Errorf("error parsing templates %w", err)
	}
	err = yaml.Unmarshal([]byte(builder.String()), object)
	if err != nil {
		return fmt.Errorf("error creating %T for model registry %s in namespace %s", object, params.Name, params.Namespace)
	}
	return nil
}

func (r *ModelRegistryReconciler) logResultAsEvent(registry *modelregistryv1alpha1.ModelRegistry, result OperationResult) {
	switch result {
	case ResourceCreated:
		r.Recorder.Event(registry, "Normal", "ServiceCreated",
			fmt.Sprintf("Created service for custom resource %s in namespace %s",
				registry.Name,
				registry.Namespace))
	case ResourceUpdated:
		r.Recorder.Event(registry, "Normal", "ServiceUpdated",
			fmt.Sprintf("Updated service for custom resource %s in namespace %s",
				registry.Name,
				registry.Namespace))
	}
}
