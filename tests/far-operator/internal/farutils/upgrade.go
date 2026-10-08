package farutils

import (
	"fmt"
	"strconv"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// TemplateValidationCondition identifies the controller's fence-agent status probe.
const TemplateValidationCondition = "FenceAgentStatusValidationSucceeded"

// FBCRemediationEnabled keeps destructive validation explicitly opt-in.
func FBCRemediationEnabled(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("FAR_FBC_REMEDIATION must be a boolean: %w", err)
	}

	return enabled, nil
}

// SafeUpgradeTemplate uses an unreachable documentation-only address and dummy parameters.
// Validation performs a read-only status command; no remediation CR is created.
func SafeUpgradeTemplate(name, namespace, token string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "fence-agents-remediation.medik8s.io/v1alpha1", "kind": "FenceAgentsRemediationTemplate",
		"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
			"agent": "fence_ipmilan", "retrycount": int64(7), "retryinterval": "7s", "timeout": "45s",
			"remediationStrategy": "ResourceDeletion",
			"sharedparameters": map[string]interface{}{
				"--action": "reboot", "--ip": "192.0.2.1", "--username": "fbc-test",
				"--password": "unused-not-a-real-secret", "--lanplus": "",
			},
			"nodeparameters": map[string]interface{}{
				"--ipport": map[string]interface{}{"far-fbc-placeholder-" + token: "6233"},
			},
		}}},
	}}
	object.SetName(name)
	object.SetNamespace(namespace)

	return object
}

// VerifyTemplateValidation rejects stale or missing probe results.
// The safe probe must report an expected failure; real AWS validation must succeed.
func VerifyTemplateValidation(object *unstructured.Unstructured, remediationEnabled bool) error {
	expectedStatus, expectedReason := "False", "ValidationFailed"
	if remediationEnabled {
		expectedStatus, expectedReason = "True", "ValidationSucceeded"
	}
	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		return err
	}
	for _, value := range conditions {
		condition, ok := value.(map[string]interface{})
		if !ok || condition["type"] != TemplateValidationCondition {
			continue
		}
		if condition["status"] == expectedStatus && condition["reason"] == expectedReason &&
			condition["observedGeneration"] == object.GetGeneration() {
			return nil
		}
	}

	return fmt.Errorf("FAR template has no %s validation for generation %d", expectedReason, object.GetGeneration())
}

// RemediationFromTemplate copies the actual persisted settings, not a second test-built spec.
func RemediationFromTemplate(
	template *unstructured.Unstructured, node, token string,
) (*unstructured.Unstructured, error) {
	spec, found, err := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	if err != nil {
		return nil, fmt.Errorf("read FAR template.spec: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("FAR template must contain template.spec")
	}
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": template.GetAPIVersion(), "kind": "FenceAgentsRemediation", "spec": spec,
	}}
	object.SetName(node)
	object.SetNamespace(template.GetNamespace())
	object.SetLabels(map[string]string{helpers.FBCRunLabel: token})

	return object, nil
}

// VerifyOwnedObject protects against deleting or trusting another run's replacement object.
func VerifyOwnedObject(object *unstructured.Unstructured, uid types.UID, token string) error {
	if uid == "" || object.GetUID() != uid || object.GetLabels()[helpers.FBCRunLabel] != token {
		return fmt.Errorf("refusing replaced/unowned %s %s", object.GetKind(), object.GetName())
	}

	return nil
}

// WorkloadEvicted requires disappearance or deletion of the original pod, not an arbitrary API error.
func WorkloadEvicted(uid, currentUID types.UID, deleting bool, err error) bool {
	return apierrors.IsNotFound(err) || (err == nil && (currentUID != uid || deleting))
}
