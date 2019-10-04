// Copyright 2019 Red Hat, Inc. and/or its affiliates
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kogitoapp

import (
	"fmt"
	"github.com/kiegroup/kogito-cloud-operator/pkg/client/meta"
	"github.com/kiegroup/kogito-cloud-operator/pkg/client/openshift"
	kogitores "github.com/kiegroup/kogito-cloud-operator/pkg/controller/kogitoapp/resource"
	"github.com/kiegroup/kogito-cloud-operator/pkg/controller/kogitoapp/shared"
	"github.com/kiegroup/kogito-cloud-operator/pkg/resource"
	"time"

	"github.com/kiegroup/kogito-cloud-operator/pkg/client/kubernetes"

	"github.com/kiegroup/kogito-cloud-operator/pkg/apis/app/v1alpha1"
	kogitocli "github.com/kiegroup/kogito-cloud-operator/pkg/client"
	"github.com/kiegroup/kogito-cloud-operator/pkg/controller/kogitoapp/status"
	"github.com/kiegroup/kogito-cloud-operator/pkg/logger"
	oappsv1 "github.com/openshift/api/apps/v1"
	obuildv1 "github.com/openshift/api/build/v1"
	oimagev1 "github.com/openshift/api/image/v1"
	routev1 "github.com/openshift/api/route/v1"
	buildv1 "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	imagev1 "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	cachev1 "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logger.GetLogger("controller_kogitoapp")

// Add creates a new KogitoApp Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	imageClient, err := imagev1.NewForConfig(mgr.GetConfig())
	if err != nil {
		panic(fmt.Sprintf("Error getting image client: %v", err))
	}
	buildClient, err := buildv1.NewForConfig(mgr.GetConfig())
	if err != nil {
		panic(fmt.Sprintf("Error getting build client: %v", err))
	}

	client := &kogitocli.Client{
		ControlCli: mgr.GetClient(),
		BuildCli:   buildClient,
		ImageCli:   imageClient,
	}

	return &ReconcileKogitoApp{
		client: client,
		scheme: mgr.GetScheme(),
		cache:  mgr.GetCache(),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("kogitoapp-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource KogitoApp
	err = c.Watch(&source.Kind{Type: &v1alpha1.KogitoApp{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	watchOwnedObjects := []runtime.Object{
		&oappsv1.DeploymentConfig{},
		&corev1.Service{},
		&routev1.Route{},
		&obuildv1.BuildConfig{},
		&oimagev1.ImageStream{},
	}
	ownerHandler := &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &v1alpha1.KogitoApp{},
	}
	for _, watchObject := range watchOwnedObjects {
		err = c.Watch(&source.Kind{Type: watchObject}, ownerHandler)
		if err != nil {
			return err
		}
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileKogitoApp{}

// ReconcileKogitoApp reconciles a KogitoApp object
type ReconcileKogitoApp struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client *kogitocli.Client
	scheme *runtime.Scheme
	cache  cachev1.Cache
}

// Reconcile reads that state of the cluster for a KogitoApp object and makes changes based on the state read
// and what is in the KogitoApp.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileKogitoApp) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("Reconciling KogitoApp")

	// Fetch the KogitoApp instance
	instance := &v1alpha1.KogitoApp{}
	if exists, err := kubernetes.ResourceC(r.client).FetchWithKey(request.NamespacedName, instance); err != nil {
		return reconcile.Result{}, err
	} else if !exists {
		return reconcile.Result{}, nil
	}

	if instance.Spec.Runtime != v1alpha1.SpringbootRuntimeType {
		instance.Spec.Runtime = v1alpha1.QuarkusRuntimeType
	}

	r.setDefaultBuildLimits(instance)

	log.Infof("Checking if all resources for '%s' are created", instance.Name)
	// create resources in the cluster that do not exist
	kogitoResources, err := kogitores.BuildOrFetchObjects(&kogitores.Context{
		KogitoApp: instance,
		FactoryContext: resource.FactoryContext{
			Client: r.client,
			PreCreate: func(object meta.ResourceObject) error {
				if object != nil {
					log.Debugf("Setting controller reference pre create for '%s' kind '%s'", object.GetName(), object.GetObjectKind().GroupVersionKind().Kind)
					return controllerutil.SetControllerReference(instance, object, r.scheme)
				}
				return nil
			},
		},
	})
	if err != nil {
		return reconcile.Result{}, err
	}

	if kogitoResources.BuildConfigS2IStatus.IsNew {
		log.Infof("Buildconfigs are created, triggering build %s", kogitoResources.BuildConfigS2I.Name)
		if _, err := openshift.BuildConfigC(r.client).TriggerBuild(kogitoResources.BuildConfigS2I, instance.Name); err != nil {
			return reconcile.Result{}, err
		}
	}

	var resourcesUpdateResult *kogitores.UpdateResourcesResult
	log.Infof("Handling changes in Kogito App '%s'", instance.Name)
	resourcesUpdateResult = kogitores.ManageResources(instance, kogitoResources, r.client)

	log.Infof("Handling Status updates on '%s'", instance.Name)
	statusUpdateResult := status.ManageStatus(instance, kogitoResources, r.client, resourcesUpdateResult, r.cache, request.NamespacedName)

	if statusUpdateResult.Err != nil {
		log.Infof("Reconcile for '%s' finished with error", instance.Name)
		return reconcile.Result{}, err
	} else if statusUpdateResult.RequeueAfter {
		log.Infof("Reconcile for '%s' finished with requeue in 30 seconds", instance.Name)
		return reconcile.Result{RequeueAfter: time.Duration(30) * time.Second}, nil
	} else if statusUpdateResult.Updated {
		log.Infof("Reconcile for '%s' finished with requeue", instance.Name)
		return reconcile.Result{Requeue: true}, nil
	}
	log.Infof("Reconcile for '%s' successfully finished", instance.Name)
	return reconcile.Result{}, nil
}

func (r *ReconcileKogitoApp) setDefaultBuildLimits(instance *v1alpha1.KogitoApp) {
	if &instance.Spec.Build.Resources == nil {
		instance.Spec.Build.Resources = v1alpha1.Resources{}
	}
	if len(instance.Spec.Build.Resources.Limits) == 0 {
		if instance.Spec.Build.Native {
			instance.Spec.Build.Resources.Limits = kogitores.DefaultBuildS2INativeLimits
		} else {
			instance.Spec.Build.Resources.Limits = kogitores.DefaultBuildS2IJVMLimits
		}
	} else {
		if !shared.ContainsResource(v1alpha1.ResourceCPU, instance.Spec.Build.Resources.Limits) {
			if instance.Spec.Build.Native {
				instance.Spec.Build.Resources.Limits = append(instance.Spec.Build.Resources.Limits, kogitores.DefaultBuildS2INativeCPULimit)
			} else {
				instance.Spec.Build.Resources.Limits = append(instance.Spec.Build.Resources.Limits, kogitores.DefaultBuildS2IJVMCPULimit)
			}
		}
		if !shared.ContainsResource(v1alpha1.ResourceMemory, instance.Spec.Build.Resources.Limits) {
			if instance.Spec.Build.Native {
				instance.Spec.Build.Resources.Limits = append(instance.Spec.Build.Resources.Limits, kogitores.DefaultBuildS2INativeMemoryLimit)
			} else {
				instance.Spec.Build.Resources.Limits = append(instance.Spec.Build.Resources.Limits, kogitores.DefaultBuildS2IJVMMemoryLimit)
			}
		}
	}
}