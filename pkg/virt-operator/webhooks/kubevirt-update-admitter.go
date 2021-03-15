/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package webhooks

import (
	"encoding/json"
	"fmt"
	"reflect"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"
	webhookutils "kubevirt.io/kubevirt/pkg/util/webhooks"
	validating_webhooks "kubevirt.io/kubevirt/pkg/util/webhooks/validating-webhooks"
)

// KubeVirtUpdateAdmitter validates KubeVirt updates
type KubeVirtUpdateAdmitter struct {
	Client kubecli.KubevirtClient
}

// NewKubeVirtUpdateAdmitter creates a KubeVirtUpdateAdmitter
func NewKubeVirtUpdateAdmitter(client kubecli.KubevirtClient) *KubeVirtUpdateAdmitter {
	return &KubeVirtUpdateAdmitter{
		Client: client,
	}
}

func (admitter *KubeVirtUpdateAdmitter) Admit(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	// Get new and old KubeVirt from admission response
	var results []metav1.StatusCause
	newKV, oldKV, err := getAdmissionReviewKubeVirt(ar)
	if err != nil {
		return webhookutils.ToAdmissionResponseError(err)
	}

	if resp := webhookutils.ValidateSchema(v1.KubeVirtGroupVersionKind, ar.Request.Object.Raw); resp != nil {
		return resp
	}

	results = validateCustomizeComponents(newKV.Spec.CustomizeComponents)

	if reflect.DeepEqual(newKV.Spec.Workloads, oldKV.Spec.Workloads) {
		return validating_webhooks.NewAdmissionResponse(results)
	}

	// reject update if it will move a virt-handler pod from a node that has
	// a vmi running on it
	causes, err := admitter.validateWorkloadPlacementUpdate()
	if err != nil {
		return webhookutils.ToAdmissionResponseError(err)
	}

	results = append(results, causes...)

	return validating_webhooks.NewAdmissionResponse(results)
}

func (admitter *KubeVirtUpdateAdmitter) validateWorkloadPlacementUpdate() ([]metav1.StatusCause, error) {
	vmis, err := admitter.Client.VirtualMachineInstance(corev1.NamespaceAll).List(&metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	if len(vmis.Items) > 0 {
		return []metav1.StatusCause{
			{
				Type:    metav1.CauseTypeFieldValueNotSupported,
				Message: "can't update placement of workload pods while there are running vms",
			},
		}, nil
	}

	return []metav1.StatusCause{}, nil
}

func getAdmissionReviewKubeVirt(ar *admissionv1.AdmissionReview) (new *v1.KubeVirt, old *v1.KubeVirt, err error) {
	if !webhookutils.ValidateRequestResource(ar.Request.Resource, KubeVirtGroupVersionResource.Group, KubeVirtGroupVersionResource.Resource) {
		return nil, nil, fmt.Errorf("expect resource to be '%s'", KubeVirtGroupVersionResource)
	}

	raw := ar.Request.Object.Raw
	newKV := v1.KubeVirt{}

	err = json.Unmarshal(raw, &newKV)
	if err != nil {
		return nil, nil, err
	}

	if ar.Request.Operation == admissionv1.Update {
		raw := ar.Request.OldObject.Raw
		oldKV := v1.KubeVirt{}
		err = json.Unmarshal(raw, &oldKV)
		if err != nil {
			return nil, nil, err
		}

		return &newKV, &oldKV, nil
	}

	return &newKV, nil, nil
}

func validateCustomizeComponents(customization v1.CustomizeComponents) []metav1.StatusCause {
	patches := customization.Patches
	statuses := []metav1.StatusCause{}

	for _, patch := range patches {
		if json.Valid([]byte(patch.Patch)) {
			continue
		}

		statuses = append(statuses, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueNotSupported,
			Message: fmt.Sprintf("patch %q is not valid JSON", patch.Patch),
		})
	}

	return statuses
}
