/*

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

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	samplecontrollerv1alpha1 "github.com/micnncim/sample-controller/api/v1alpha1"
)

var (
	deploymentOwnerKey = ".metadata.controller"
	apiGVStr           = samplecontrollerv1alpha1.GroupVersion.String()
)

// FooReconciler reconciles a Foo object
type FooReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=samplecontroller.k8s.io,resources=foos,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=samplecontroller.k8s.io,resources=foos/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *FooReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("foo", req.NamespacedName)

	//
	// 1. Load the Foo by name.
	//

	var foo samplecontrollerv1alpha1.Foo
	if err := r.Get(ctx, req.NamespacedName, &foo); err != nil {
		log.Error(err, "unable to fetch Foo")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	//
	// 2. Clean Up old Deployment which had been owned by Foo Resource.
	//

	if err := r.cleanupOwnedResources(ctx, log, &foo); err != nil {
		return ctrl.Result{}, err
	}

	//
	// 3. Create or Update deployment object which match foo.Spec.
	//

	if err := r.createOrUpdateDeployment(ctx, req, foo, log); err != nil {
		return ctrl.Result{}, err
	}

	//
	// 4. Update foo status.
	//

	if err := r.updateFooStatus(ctx, req, foo, log); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(
		&foo,
		corev1.EventTypeNormal,
		"Updated",
		"Update foo.status.AvailableReplicas: %d", foo.Status.AvailableReplicas,
	)

	return ctrl.Result{}, nil
}

func (r *FooReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(&appsv1.Deployment{}, deploymentOwnerKey, func(rawObj runtime.Object) []string {
		deployment := rawObj.(*appsv1.Deployment)
		owner := metav1.GetControllerOf(deployment)
		if owner == nil {
			return nil
		}
		if owner.APIVersion != apiGVStr || owner.Kind != "Foo" {
			return nil
		}

		return []string{owner.Name}
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&samplecontrollerv1alpha1.Foo{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func (r *FooReconciler) cleanupOwnedResources(ctx context.Context, log logr.Logger, foo *samplecontrollerv1alpha1.Foo) error {
	log.Info("finding existing Deployments for Foo resource")

	var deployments appsv1.DeploymentList
	err := r.List(
		ctx,
		&deployments,
		client.InNamespace(foo.Namespace),
		client.MatchingFields(map[string]string{deploymentOwnerKey: foo.Name}),
	)
	if err != nil {
		return err
	}

	for _, deployment := range deployments.Items {
		if deployment.Name == foo.Spec.DeploymentName {
			continue
		}
		if err := r.Delete(ctx, &deployment); err != nil {
			log.Error(err, "failed to delete Deployment resource")
			return err
		}
		log.Info("delete deployment resource: " + deployment.Name)
		r.Recorder.Eventf(
			foo,
			corev1.EventTypeNormal,
			"Deleted",
			"Deleted deployment %q", deployment.Name,
		)
	}

	return nil
}

func (r *FooReconciler) createOrUpdateDeployment(
	ctx context.Context,
	req ctrl.Request,
	foo samplecontrollerv1alpha1.Foo,
	log logr.Logger,
) error {

	deploymentName := foo.Spec.DeploymentName
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: req.Namespace,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		replicas := int32(1)
		if foo.Spec.Replicas != nil {
			replicas = *foo.Spec.Replicas
		}
		deploy.Spec.Replicas = &replicas

		labels := map[string]string{
			"app":        "nginx",
			"controller": req.Name,
		}

		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: labels,
			}
		}

		if deploy.Spec.Template.ObjectMeta.Labels == nil {
			deploy.Spec.Template.ObjectMeta.Labels = labels
		}

		containers := []corev1.Container{
			{
				Name:  "nginx",
				Image: "nginx:latest",
			},
		}

		if deploy.Spec.Template.Spec.Containers == nil {
			deploy.Spec.Template.Spec.Containers = containers
		}

		if err := ctrl.SetControllerReference(&foo, deploy, r.Scheme); err != nil {
			log.Error(err, "unable to set ownerReference from Foo to Deployment")
			return err
		}

		return nil
	})
	if err != nil {
		log.Error(err, "unable to ensure deployment is correct")
		return err
	}
	return nil
}

func (r *FooReconciler) updateFooStatus(
	ctx context.Context,
	req ctrl.Request,
	foo samplecontrollerv1alpha1.Foo,
	log logr.Logger,
) error {

	deploymentName := foo.Spec.DeploymentName
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: req.Namespace,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		replicas := int32(1)
		if foo.Spec.Replicas != nil {
			replicas = *foo.Spec.Replicas
		}
		deploy.Spec.Replicas = &replicas

		labels := map[string]string{
			"app":        "nginx",
			"controller": req.Name,
		}

		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: labels,
			}
		}

		if deploy.Spec.Template.ObjectMeta.Labels == nil {
			deploy.Spec.Template.ObjectMeta.Labels = labels
		}

		containers := []corev1.Container{
			{
				Name:  "nginx",
				Image: "nginx:latest",
			},
		}

		if deploy.Spec.Template.Spec.Containers == nil {
			deploy.Spec.Template.Spec.Containers = containers
		}

		if err := ctrl.SetControllerReference(&foo, deploy, r.Scheme); err != nil {
			log.Error(err, "unable to set ownerReference from Foo to Deployment")
			return err
		}

		return nil
	})
	if err != nil {
		log.Error(err, "unable to ensure deployment is correct")
		return err
	}
	return nil
}
