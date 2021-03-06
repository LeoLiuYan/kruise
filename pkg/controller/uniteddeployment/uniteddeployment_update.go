/*
Copyright 2019 The Kruise Authors.

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

package uniteddeployment

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"

	appsv1alpha1 "github.com/openkruise/kruise/pkg/apis/apps/v1alpha1"
	"github.com/openkruise/kruise/pkg/util"
)

func (r *ReconcileUnitedDeployment) manageSubsetProvision(ud *appsv1alpha1.UnitedDeployment, nameToSubset *map[string]*Subset, nextReplicas, nextPartitions *map[string]int32, currentRevision, updatedRevision *appsv1.ControllerRevision, subsetType subSetType) (sets.String, error) {
	expectedSubsets := sets.String{}
	gotSubsets := sets.String{}

	for _, subset := range ud.Spec.Topology.Subsets {
		expectedSubsets.Insert(subset.Name)
	}

	for subsetName := range *nameToSubset {
		gotSubsets.Insert(subsetName)
	}
	klog.V(4).Infof("UnitedDeployment %s/%s has subsets %v, expects subsets %v", ud.Namespace, ud.Name, gotSubsets.List(), expectedSubsets.List())

	var creates []string
	for _, expectSubset := range expectedSubsets.List() {
		if gotSubsets.Has(expectSubset) {
			continue
		}

		creates = append(creates, expectSubset)
	}

	var deletes []string
	for _, gotSubset := range gotSubsets.List() {
		if expectedSubsets.Has(gotSubset) {
			continue
		}

		deletes = append(deletes, gotSubset)
	}

	var errs []error
	// manage creating
	if len(creates) > 0 {
		// do not consider deletion
		klog.V(0).Infof("UnitedDeployment %s/%s needs creating subset (%s) with name: %v", ud.Namespace, ud.Name, subsetType, creates)
		createdSubsets := make([]string, len(creates))
		for i, subset := range creates {
			createdSubsets[i] = subset
		}

		revision := currentRevision.Name
		if updatedRevision != nil {
			revision = updatedRevision.Name
		}

		var createdNum int
		var createdErr error
		createdNum, createdErr = util.SlowStartBatch(len(creates), slowStartInitialBatchSize, func(idx int) error {
			subsetName := createdSubsets[idx]

			replicas := (*nextReplicas)[subsetName]
			partition := (*nextPartitions)[subsetName]
			err := r.subSetControls[subsetType].CreateSubset(ud, subsetName, revision, replicas, partition)
			if err != nil {
				if !errors.IsTimeout(err) {
					return fmt.Errorf("fail to create Subset (%s) %s: %s", subsetType, subsetName, err.Error())
				}
			}

			return nil
		})
		if createdErr == nil {
			r.recorder.Eventf(ud.DeepCopy(), corev1.EventTypeNormal, fmt.Sprintf("Successful%s", eventTypeSubsetsUpdate), "Create %d Subset (%s)", createdNum, subsetType)
		} else {
			errs = append(errs, createdErr)
		}
	}

	// manage deleting
	if len(deletes) > 0 {
		klog.V(0).Infof("UnitedDeployment %s/%s needs deleting subset (%s) with name: [%v]", ud.Namespace, ud.Name, subsetType, deletes)
		var deleteErrs []error
		for _, subsetName := range deletes {
			subset := (*nameToSubset)[subsetName]
			if err := r.subSetControls[subsetType].DeleteSubset(subset); err != nil {
				deleteErrs = append(deleteErrs, fmt.Errorf("fail to delete Subset (%s) %s/%s for %s: %s", subsetType, subset.Namespace, subset.Name, subsetName, err))
			}
		}

		if len(deleteErrs) > 0 {
			errs = append(errs, deleteErrs...)
		} else {
			r.recorder.Eventf(ud.DeepCopy(), corev1.EventTypeNormal, fmt.Sprintf("Successful%s", eventTypeSubsetsUpdate), "Delete %d Subset (%s)", len(deletes), subsetType)
		}
	}

	// clean the other kind of subsets
	for t, control := range r.subSetControls {
		if t == subsetType {
			continue
		}

		subsets, err := control.GetAllSubsets(ud)
		if err != nil {
			errs = append(errs, fmt.Errorf("fail to list Subset of other type %s for UnitedDeployment %s/%s: %s", t, ud.Namespace, ud.Name, err))
			continue
		}

		for _, subset := range subsets {
			if err := control.DeleteSubset(subset); err != nil {
				errs = append(errs, fmt.Errorf("fail to delete Subset %s of other type %s for UnitedDeployment %s/%s: %s", subset.Name, t, ud.Namespace, ud.Name, err))
				continue
			}
		}
	}

	return expectedSubsets.Intersection(gotSubsets), utilerrors.NewAggregate(errs)
}
