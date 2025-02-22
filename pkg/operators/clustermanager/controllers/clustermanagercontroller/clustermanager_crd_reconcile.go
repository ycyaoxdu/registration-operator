/*
 * Copyright 2022 Contributors to the Open Cluster Management project
 */

package clustermanagercontroller

import (
	"context"
	"fmt"
	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	operatorapiv1 "open-cluster-management.io/api/operator/v1"
	"open-cluster-management.io/registration-operator/manifests"
	"open-cluster-management.io/registration-operator/pkg/helpers"
	"open-cluster-management.io/registration-operator/pkg/operators/clustermanager/controllers/migrationcontroller"
	"reflect"
	migrationclient "sigs.k8s.io/kube-storage-version-migrator/pkg/clients/clientset/typed/migration/v1alpha1"
	"time"
)

var (
	// crdNames is the list of CRDs to be wiped out before deleting other resources when clusterManager is deleted.
	// The order of the list matters, the managedclusteraddon crd needs to be deleted at first so all addon related
	// manifestwork is deleted, then other manifestworks.
	crdNames = []string{
		"managedclusteraddons.addon.open-cluster-management.io",
		"manifestworks.work.open-cluster-management.io",
		"managedclusters.cluster.open-cluster-management.io",
	}

	// crdResourceFiles should be deployed in the hub cluster
	hubCRDResourceFiles = []string{
		"cluster-manager/hub/0000_00_addon.open-cluster-management.io_clustermanagementaddons.crd.yaml",
		"cluster-manager/hub/0000_00_clusters.open-cluster-management.io_managedclusters.crd.yaml",
		"cluster-manager/hub/0000_00_clusters.open-cluster-management.io_managedclustersets.crd.yaml",
		"cluster-manager/hub/0000_00_work.open-cluster-management.io_manifestworks.crd.yaml",
		"cluster-manager/hub/0000_01_addon.open-cluster-management.io_managedclusteraddons.crd.yaml",
		"cluster-manager/hub/0000_01_clusters.open-cluster-management.io_managedclustersetbindings.crd.yaml",
		"cluster-manager/hub/0000_02_clusters.open-cluster-management.io_placements.crd.yaml",
		"cluster-manager/hub/0000_02_addon.open-cluster-management.io_addondeploymentconfigs.crd.yaml",
		"cluster-manager/hub/0000_03_clusters.open-cluster-management.io_placementdecisions.crd.yaml",
		"cluster-manager/hub/0000_05_clusters.open-cluster-management.io_addonplacementscores.crd.yaml",
	}

	// removed CRD StoredVersions
	removedCRDStoredVersions = map[string]string{
		"placements.cluster.open-cluster-management.io":                "v1alpha1",
		"placementdecisions.cluster.open-cluster-management.io":        "v1alpha1",
		"managedclustersets.cluster.open-cluster-management.io":        "v1alpha1",
		"managedclustersetbindings.cluster.open-cluster-management.io": "v1alpha1",
	}
)

type crdReconcile struct {
	hubAPIExtensionClient apiextensionsclient.Interface
	hubMigrationClient    migrationclient.StorageVersionMigrationsGetter
	skipRemoveCRDs        bool

	cache    resourceapply.ResourceCache
	recorder events.Recorder
}

func (c *crdReconcile) reconcile(ctx context.Context, cm *operatorapiv1.ClusterManager, config manifests.HubConfig) (*operatorapiv1.ClusterManager, reconcileState, error) {
	// update CRD StoredVersion
	if err := c.updateStoredVersion(ctx); err != nil {
		meta.SetStatusCondition(&cm.Status.Conditions, metav1.Condition{
			Type:    clusterManagerApplied,
			Status:  metav1.ConditionFalse,
			Reason:  "CRDStoredVersionUpdateFailed",
			Message: fmt.Sprintf("Failed to update crd stored version: %v", err),
		})
		return cm, reconcileStop, err
	}

	var appliedErrs []error
	resourceResults := helpers.ApplyDirectly(
		ctx,
		nil,
		c.hubAPIExtensionClient,
		nil,
		nil,
		c.recorder,
		c.cache,
		func(name string) ([]byte, error) {
			template, err := manifests.ClusterManagerManifestFiles.ReadFile(name)
			if err != nil {
				return nil, err
			}
			objData := assets.MustCreateAssetFromTemplate(name, template, config).Data
			helpers.SetRelatedResourcesStatusesWithObj(&cm.Status.RelatedResources, objData)
			return objData, nil
		},
		hubCRDResourceFiles...,
	)
	for _, result := range resourceResults {
		if result.Error != nil {
			appliedErrs = append(appliedErrs, fmt.Errorf("%q (%T): %v", result.File, result.Type, result.Error))
		}
	}

	if len(appliedErrs) > 0 {
		meta.SetStatusCondition(&cm.Status.Conditions, metav1.Condition{
			Type:    clusterManagerApplied,
			Status:  metav1.ConditionFalse,
			Reason:  "CRDApplyFaild",
			Message: fmt.Sprintf("Failed to apply crd: %v", utilerrors.NewAggregate(appliedErrs)),
		})
		return cm, reconcileStop, utilerrors.NewAggregate(appliedErrs)
	}

	return cm, reconcileContinue, nil
}

func (c *crdReconcile) clean(ctx context.Context, cm *operatorapiv1.ClusterManager, config manifests.HubConfig) (*operatorapiv1.ClusterManager, reconcileState, error) {
	if c.skipRemoveCRDs {
		return cm, reconcileContinue, nil
	}
	// Remove crd
	for _, name := range crdNames {
		err := c.removeCRD(ctx, name)
		if err != nil {
			// TODO add condition
			return cm, reconcileStop, err
		}
		c.recorder.Eventf("CRDDeleted", "crd %s is deleted", name)
	}

	for _, file := range hubCRDResourceFiles {
		err := helpers.CleanUpStaticObject(
			ctx,
			nil,
			c.hubAPIExtensionClient,
			nil,
			func(name string) ([]byte, error) {
				template, err := manifests.ClusterManagerManifestFiles.ReadFile(name)
				if err != nil {
					return nil, err
				}
				return assets.MustCreateAssetFromTemplate(name, template, config).Data, nil
			},
			file,
		)
		if err != nil {
			// TODO add condition
			return cm, reconcileContinue, err
		}
	}

	return cm, reconcileContinue, nil
}

// updateStoredVersion update(remove) deleted api version from CRD status.StoredVersions
func (c *crdReconcile) updateStoredVersion(ctx context.Context) error {
	for name, version := range removedCRDStoredVersions {
		// Check migration status before update CRD stored version
		// If CRD's StorageVersionMigration is not found, it means that the previous or the current release CRD doesn't need migration, and can contiue to update the CRD's stored version.
		// If CRD's StorageVersionMigration is found and the status is success, it means that the current CRs were migrated successfully, and can contiue to update the CRD's stored version.
		// Other cases, for example, the migration failed, we should not contiue to update the stored version, that will caused the stored old version CRs inconsistent with latest CRD.
		svmStatus, err := migrationcontroller.IsStorageVersionMigrationSucceeded(c.hubMigrationClient, name)
		if svmStatus == false && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to updateStoredVersion as StorageVersionMigrations %v: %v", name, err)
		}

		// retrieve CRD
		crd, err := c.hubAPIExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			klog.Warningf("faield to get CRD %v: %v", crd.Name, err)
			continue
		}

		// remove v1alpha1 from its status
		oldStoredVersions := crd.Status.StoredVersions
		newStoredVersions := make([]string, 0, len(oldStoredVersions))
		for _, stored := range oldStoredVersions {
			if stored != version {
				newStoredVersions = append(newStoredVersions, stored)
			}
		}

		if !reflect.DeepEqual(oldStoredVersions, newStoredVersions) {
			crd.Status.StoredVersions = newStoredVersions
			// update the status sub-resource
			crd, err = c.hubAPIExtensionClient.ApiextensionsV1().CustomResourceDefinitions().UpdateStatus(ctx, crd, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
			klog.V(4).Infof("updated CRD %v status storedVersions: %v", crd.Name, crd.Status.StoredVersions)
		}
	}

	return nil
}

// removeCRD removes crd, and check if crd resource is removed. Since the related cr is still being deleted,
// it will check the crd existence after deletion, and only return nil when crd is not found.
func (c *crdReconcile) removeCRD(ctx context.Context, name string) error {
	err := c.hubAPIExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Delete(
		ctx, name, metav1.DeleteOptions{})
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}

	_, err = c.hubAPIExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}

	return helpers.NewRequeueError(fmt.Sprintf("crd %s is still deleting", name), 3*time.Second)
}
