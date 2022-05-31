/*
Copyright 2021 The KCP Authors.

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

package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kcp-dev/logicalcluster"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clusters"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
)

// reconcileResource is responsible for setting the cluster for a resource of
// any type, to match the cluster where its namespace is assigned.
func (c *Controller) reconcileResource(ctx context.Context, lclusterName logicalcluster.Name, obj *unstructured.Unstructured, gvr *schema.GroupVersionResource) error {
	if gvr.Group == "networking.k8s.io" && gvr.Resource == "ingresses" {
		klog.V(4).Infof("Skipping reconciliation of ingress %s/%s", obj.GetNamespace(), obj.GetName())
		return nil
	}

	klog.V(2).Infof("Reconciling GVR %q %s|%s/%s", gvr.String(), lclusterName, obj.GetNamespace(), obj.GetName())

	// If the resource is not namespaced (incl if the resource is itself a
	// namespace), ignore it.
	if obj.GetNamespace() == "" {
		klog.V(4).Infof("GVR %q %s|%s had no namespace; ignoring", gvr.String(), logicalcluster.From(obj).String(), obj.GetName())
		return nil
	}

	// Align the resource's assigned cluster with the namespace's assigned
	// cluster.
	// First, get the namespace object (from the cached lister).
	ns, err := c.namespaceLister.Get(clusters.ToClusterAwareKey(lclusterName, obj.GetNamespace()))
	if apierrors.IsNotFound(err) {
		// Namespace was deleted; this resource will eventually get deleted too, so ignore
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reconciling resource %s|%s/%s: error getting namespace: %w", lclusterName, obj.GetNamespace(), obj.GetName(), err)
	}

	annotationPatch, labelPatch := computePlacement(ns, obj)

	// create patch
	if len(labelPatch) == 0 && len(annotationPatch) == 0 {
		return nil
	}

	patch := map[string]interface{}{}
	if len(labelPatch) > 0 {
		if err := unstructured.SetNestedField(patch, labelPatch, "metadata", "labels"); err != nil {
			klog.Errorf("unexpected unstructured error: %v", err)
			return err // should never happen
		}
	}
	if len(annotationPatch) > 0 {
		if err := unstructured.SetNestedField(patch, labelPatch, "metadata", "annotations"); err != nil {
			klog.Errorf("unexpected unstructured error: %v", err)
			return err // should never happen
		}
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		klog.Errorf("unexpected marshal error: %v", err)
		return err
	}

	if _, err := c.dynClient.Cluster(lclusterName).Resource(*gvr).Namespace(ns.Name).
		Patch(ctx, obj.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		return err
	}

	annotationsString, err := json.Marshal(annotationPatch)
	if err != nil {
		klog.Errorf("unexpected marshal error for %#v: %v", annotationPatch, err)
		return err
	}
	labelsString, err := json.Marshal(labelPatch)
	if err != nil {
		klog.Errorf("unexpected marshal error %#v: %v", labelPatch, err)
		return err
	}
	klog.V(2).Infof("Patched cluster assignment for %q %s|%s/%s: labels=%v, annotations=%v", gvr, lclusterName, ns.Name, obj.GetName(), string(labelsString), string(annotationsString))
	return nil
}

// computePlacement computes the patch against annotations and labels. Nil means to remove the key.
func computePlacement(ns *corev1.Namespace, obj metav1.Object) (annotationPatch map[string]interface{}, labelPatch map[string]interface{}) {
	nsLocations, nsDeleting := locations(ns.Annotations, ns.Labels, true)
	objLocations, objDeleting := locations(obj.GetAnnotations(), obj.GetLabels(), false)
	if objLocations.Equal(nsLocations) && objDeleting.Equal(nsDeleting) {
		// already correctly assigned.
		return
	}

	// create merge patch
	annotationPatch = map[string]interface{}{}
	labelPatch = map[string]interface{}{}
	for _, loc := range objLocations.Difference(nsLocations).List() {
		// location was removed from namespace, but is still on the object
		labelPatch[workloadv1alpha1.InternalClusterResourceStateLabelPrefix+loc] = nil
		if _, found := obj.GetAnnotations()[workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix+loc]; found {
			annotationPatch[workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix+loc] = nil
		}
	}
	for _, loc := range nsLocations.Intersection(nsLocations).List() {
		if nsTimestamp, found := ns.Annotations[workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix+loc]; found && validRFC3339(nsTimestamp) {
			objTimestamp, found := obj.GetAnnotations()[workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix+loc]
			if !found || !validRFC3339(objTimestamp) {
				annotationPatch[workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix+loc] = nsTimestamp
			}
		}
	}
	for _, loc := range nsLocations.Difference(objLocations).List() {
		// location was missing on the object
		// TODO(sttts): add way to go into pending state first, maybe with a namespace annotation
		labelPatch[workloadv1alpha1.InternalClusterResourceStateLabelPrefix+loc] = string(workloadv1alpha1.ResourceStateSync)
	}

	if len(annotationPatch) == 0 {
		annotationPatch = nil
	}
	if len(labelPatch) == 0 {
		labelPatch = nil
	}

	return
}

func (c *Controller) reconcileGVR(gvr schema.GroupVersionResource) error {
	listers, _ := c.ddsif.Listers()
	lister, found := listers[gvr]
	if !found {
		return fmt.Errorf("informer for %q is not synced; re-enqueueing", gvr)
	}

	// Update all resources in the namespaces with cluster assignment.
	objs, err := lister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, obj := range objs {
		c.enqueueResource(gvr, obj)
	}
	return nil
}

func validRFC3339(ts string) bool {
	_, err := time.Parse(time.RFC3339, ts)
	return err == nil
}
