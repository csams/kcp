/*
Copyright 2022 The KCP Authors.

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

package apibinding

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/kcp-dev/logicalcluster/v2"

	"k8s.io/apiextensions-apiserver/pkg/apihelpers"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clusters"
	"k8s.io/klog/v2"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	kcpclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	apisinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions/apis/v1alpha1"
	apislisters "github.com/kcp-dev/kcp/pkg/client/listers/apis/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/controller"
	"github.com/kcp-dev/kcp/pkg/informer"
	"github.com/kcp-dev/kcp/pkg/logging"
)

const (
	controllerName = "kcp-apibinding"
)

var (
	ShadowWorkspaceName = logicalcluster.New("system:bound-crds")
)

type APIBinding = apisv1alpha1.APIBinding
type Controller = controller.Controller[*APIBinding]
type Reconciler = controller.Reconciler[*APIBinding]

// NewController returns a new controller for APIBindings.
func NewController(
	crdClusterClient apiextensionclientset.Interface,
	kcpClusterClient kcpclient.Interface,
	dynamicClusterClient dynamic.Interface,
	dynamicDiscoverySharedInformerFactory *informer.DynamicDiscoverySharedInformerFactory,
	apiBindingInformer apisinformers.APIBindingInformer,
	apiExportInformer apisinformers.APIExportInformer,
	apiResourceSchemaInformer apisinformers.APIResourceSchemaInformer,
	temporaryRemoteShardApiExportInformer apisinformers.APIExportInformer, /*TODO(p0lyn0mial): replace with multi-shard informers*/
	temporaryRemoteShardApiResourceSchemaInformer apisinformers.APIResourceSchemaInformer, /*TODO(p0lyn0mial): replace with multi-shard informers*/
	crdInformer apiextensionsinformers.CustomResourceDefinitionInformer,
) (Controller, error) {
	logger := logging.WithReconciler(klog.Background(), controllerName)

	c := &reconciler{
		crdClusterClient:     crdClusterClient,
		kcpClusterClient:     kcpClusterClient,
		dynamicClusterClient: dynamicClusterClient,
		ddsif:                dynamicDiscoverySharedInformerFactory,

		apiBindingsLister: apiBindingInformer.Lister(),
		listAPIBindings: func(clusterName logicalcluster.Name) ([]*apisv1alpha1.APIBinding, error) {
			list, err := apiBindingInformer.Lister().List(labels.Everything())
			if err != nil {
				return nil, err
			}

			var ret []*apisv1alpha1.APIBinding

			for i := range list {
				if logicalcluster.From(list[i]) != clusterName {
					continue
				}

				ret = append(ret, list[i])
			}

			return ret, nil
		},
		apiBindingsIndexer: apiBindingInformer.Informer().GetIndexer(),

		getAPIExport: func(clusterName logicalcluster.Name, name string) (*apisv1alpha1.APIExport, error) {
			apiExport, err := apiExportInformer.Lister().Get(clusters.ToClusterAwareKey(clusterName, name))
			if errors.IsNotFound(err) {
				return temporaryRemoteShardApiExportInformer.Lister().Get(clusters.ToClusterAwareKey(clusterName, name))
			}
			return apiExport, err
		},
		apiExportsIndexer:                     apiExportInformer.Informer().GetIndexer(),
		temporaryRemoteShardApiExportsIndexer: temporaryRemoteShardApiExportInformer.Informer().GetIndexer(),

		getAPIResourceSchema: func(clusterName logicalcluster.Name, name string) (*apisv1alpha1.APIResourceSchema, error) {
			apiResourceSchema, err := apiResourceSchemaInformer.Lister().Get(clusters.ToClusterAwareKey(clusterName, name))
			if errors.IsNotFound(err) {
				return temporaryRemoteShardApiResourceSchemaInformer.Lister().Get(clusters.ToClusterAwareKey(clusterName, name))
			}
			return apiResourceSchema, err
		},

		createCRD: func(ctx context.Context, clusterName logicalcluster.Name, crd *apiextensionsv1.CustomResourceDefinition) (*apiextensionsv1.CustomResourceDefinition, error) {
			return crdClusterClient.ApiextensionsV1().CustomResourceDefinitions().Create(logicalcluster.WithCluster(ctx, clusterName), crd, metav1.CreateOptions{})
		},
		getCRD: func(clusterName logicalcluster.Name, name string) (*apiextensionsv1.CustomResourceDefinition, error) {
			return crdInformer.Lister().Get(clusters.ToClusterAwareKey(clusterName, name))
		},
		crdIndexer:        crdInformer.Informer().GetIndexer(),
		deletedCRDTracker: newLockedStringSet(),
		logger:            logger,
	}

	options := &controller.Options{Name: controllerName}
	ctl := controller.New[Reconciler, *APIBinding](options, apiBindingInformer.Informer(), c)
	c.ctl = ctl

	if err := apiBindingInformer.Informer().AddIndexers(cache.Indexers{
		indexAPIBindingsByWorkspaceExport: indexAPIBindingsByWorkspaceExportFunc,
	}); err != nil {
		return nil, err
	}

	crdInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return false
			}

			return logicalcluster.From(crd) == ShadowWorkspaceName
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { c.enqueueCRD(obj, logger) },
			UpdateFunc: func(_, obj interface{}) { c.enqueueCRD(obj, logger) },
			DeleteFunc: func(obj interface{}) {
				meta, err := meta.Accessor(obj)
				if err != nil {
					runtime.HandleError(err)
					return
				}

				// If something deletes one of our bound CRDs, we need to keep track of it so when we're reconciling,
				// we know we need to recreate it. This set is there to fight against stale informers still seeing
				// the deleted CRD.
				c.deletedCRDTracker.Add(meta.GetName())

				c.enqueueCRD(obj, logger)
			},
		},
	})

	if err := crdInformer.Informer().AddIndexers(cache.Indexers{
		indexByWorkspace: indexByWorkspaceFunc,
	}); err != nil {
		return nil, err
	}

	apiResourceSchemaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
	})

	apiExportInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
	})
	temporaryRemoteShardApiExportInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
	})
	temporaryRemoteShardApiResourceSchemaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger, "") },
	})

	if err := c.apiExportsIndexer.AddIndexers(cache.Indexers{
		indexAPIExportsByAPIResourceSchema: indexAPIExportsByAPIResourceSchemasFunc,
	}); err != nil {
		return nil, fmt.Errorf("error add CRD indexes: %w", err)
	}
	if err := c.temporaryRemoteShardApiExportsIndexer.AddIndexers(cache.Indexers{
		indexAPIExportsByAPIResourceSchema: indexAPIExportsByAPIResourceSchemasFunc,
	}); err != nil {
		return nil, fmt.Errorf("error adding ApiExport indexes for the root shard: %w", err)
	}

	return ctl, nil
}

// reconciler reconciles APIBindings. It creates and maintains CRDs associated with APIResourceSchemas that are
// referenced from APIBindings. It also watches CRDs, APIResourceSchemas, and APIExports to ensure whenever
// objects related to an APIBinding are updated, the APIBinding is reconciled.
type reconciler struct {
	crdClusterClient     apiextensionclientset.Interface
	kcpClusterClient     kcpclient.Interface
	dynamicClusterClient dynamic.Interface
	ddsif                *informer.DynamicDiscoverySharedInformerFactory

	apiBindingsLister  apislisters.APIBindingLister
	listAPIBindings    func(clusterName logicalcluster.Name) ([]*apisv1alpha1.APIBinding, error)
	apiBindingsIndexer cache.Indexer

	getAPIExport                          func(clusterName logicalcluster.Name, name string) (*apisv1alpha1.APIExport, error)
	apiExportsIndexer                     cache.Indexer
	temporaryRemoteShardApiExportsIndexer cache.Indexer

	getAPIResourceSchema func(clusterName logicalcluster.Name, name string) (*apisv1alpha1.APIResourceSchema, error)

	createCRD  func(ctx context.Context, clusterName logicalcluster.Name, crd *apiextensionsv1.CustomResourceDefinition) (*apiextensionsv1.CustomResourceDefinition, error)
	getCRD     func(clusterName logicalcluster.Name, name string) (*apiextensionsv1.CustomResourceDefinition, error)
	crdIndexer cache.Indexer

	deletedCRDTracker *lockedStringSet

	ctl    Controller
	logger logr.Logger
}

// enqueueAPIExport enqueues maps an APIExport to APIBindings for enqueuing.
func (c *reconciler) enqueueAPIExport(obj interface{}, logger logr.Logger, logSuffix string) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	bindingsForExport, err := c.apiBindingsIndexer.ByIndex(indexAPIBindingsByWorkspaceExport, key)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	for _, binding := range bindingsForExport {
		b, ok := binding.(*APIBinding)
		if !ok {
			panic("APIBinding controller expected *APIBinding from enqueueAPIExport")
		}
		c.ctl.Enqueue(b, logging.WithObject(logger, obj.(*apisv1alpha1.APIExport)), fmt.Sprintf(" because of APIExport%s", logSuffix))
	}
}

// enqueueCRD maps a CRD to APIResourceSchema for enqueuing.
func (c *reconciler) enqueueCRD(obj interface{}, logger logr.Logger) {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		runtime.HandleError(fmt.Errorf("obj is supposed to be a CustomResourceDefinition, but is %T", obj))
		return
	}
	logger = logging.WithObject(logger, crd).WithValues(
		"groupResource", fmt.Sprintf("%s.%s", crd.Spec.Names.Plural, crd.Spec.Group),
		"established", apihelpers.IsCRDConditionTrue(crd, apiextensionsv1.Established),
	)

	if crd.Annotations[apisv1alpha1.AnnotationSchemaClusterKey] == "" || crd.Annotations[apisv1alpha1.AnnotationSchemaNameKey] == "" {
		logger.V(4).Info("skipping CRD because does not belong to an APIResourceSchema")
		return
	}

	clusterName := logicalcluster.New(crd.Annotations[apisv1alpha1.AnnotationSchemaClusterKey])
	apiResourceSchema, err := c.getAPIResourceSchema(clusterName, crd.Annotations[apisv1alpha1.AnnotationSchemaNameKey])
	if err != nil {
		runtime.HandleError(err)
		return
	}

	// this log here is kind of redundant normally. But we are seeing missing CRD update events
	// and hence stale APIBindings. So this might help to undersand what's going on.
	logger.V(4).Info("queueing APIResourceSchema because of CRD", "key", clusters.ToClusterAwareKey(clusterName, apiResourceSchema.Name))

	c.enqueueAPIResourceSchema(apiResourceSchema, logger, " because of CRD")
}

// enqueueAPIResourceSchema maps an APIResourceSchema to APIExports for enqueuing.
func (c *reconciler) enqueueAPIResourceSchema(obj interface{}, logger logr.Logger, logSuffix string) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	apiExports, err := c.apiExportsIndexer.ByIndex(indexAPIExportsByAPIResourceSchema, key)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	if len(apiExports) == 0 {
		apiExports, err = c.temporaryRemoteShardApiExportsIndexer.ByIndex(indexAPIExportsByAPIResourceSchema, key)
		if err != nil {
			runtime.HandleError(err)
			return
		}
	}

	for _, export := range apiExports {
		c.enqueueAPIExport(export, logging.WithObject(logger, obj.(*apisv1alpha1.APIResourceSchema)), fmt.Sprintf(" because of APIResourceSchema%s", logSuffix))
	}
}
