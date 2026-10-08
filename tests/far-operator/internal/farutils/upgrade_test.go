package farutils

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func upgradeTemplate() *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "fence-agents-remediation.medik8s.io/v1alpha1", "kind": "FenceAgentsRemediationTemplate",
		"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
			"agent": "fence_aws", "retrycount": int64(7), "retryinterval": "7s",
			"timeout": "45s", "remediationStrategy": "ResourceDeletion", "sharedSecretName": "credentials-reference",
			"sharedparameters": map[string]interface{}{"--region": "us-east-1", "--action": "reboot"},
		}}},
	}}
	object.SetName("upgrade-template")
	object.SetNamespace("test-namespace")
	object.SetUID("original-uid")
	object.SetGeneration(2)
	object.SetLabels(map[string]string{helpers.FBCRunLabel: "run-token"})

	return object
}

func TestVerifyConfiguration(t *testing.T) {
	object := upgradeTemplate()
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	for _, test := range []struct {
		name    string
		change  func(*unstructured.Unstructured)
		wantErr bool
	}{
		{name: "unchanged"},
		{name: "status only", change: func(object *unstructured.Unstructured) {
			object.Object["status"] = map[string]interface{}{"ready": true}
		}},
		{name: "recreated", wantErr: true, change: func(object *unstructured.Unstructured) {
			object.SetUID("new-uid")
		}},
		{name: "changed nested config", wantErr: true, change: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, int64(5), "spec", "template", "spec", "retrycount")
		}},
		{name: "additional config", wantErr: true, change: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, int64(1), "spec", "statusValidationSample")
		}},
		{name: "missing config", wantErr: true, change: func(object *unstructured.Unstructured) {
			unstructured.RemoveNestedField(object.Object, "spec", "template", "spec", "timeout")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := object.DeepCopy()
			if test.change != nil {
				test.change(current)
			}
			err := helpers.VerifyConfigurationPreserved(context.Background(), nil, current, object.GetUID(), spec)
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected preservation result: %v", err)
			}
		})
	}
	if err := helpers.VerifyConfigurationPreserved(context.Background(), nil, object, "", spec); err == nil {
		t.Fatal("accepted a missing original UID")
	}
}

func TestVerifyTemplateValidation(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		generation int64
		enabled    bool
		wantErr    bool
	}{
		{name: "fresh success", status: "True", generation: 2, enabled: true},
		{name: "stale success", status: "True", generation: 1, enabled: true, wantErr: true},
		{name: "failed real validation", status: "False", generation: 2, enabled: true, wantErr: true},
		{name: "pending validation", status: "Unknown", generation: 2, wantErr: true},
		{name: "expected safe failure", status: "False", generation: 2},
		{name: "stale safe failure", status: "False", generation: 1, wantErr: true},
		{name: "unexpected safe success", status: "True", generation: 2, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := upgradeTemplate()
			reason := "ValidationSucceeded"
			if test.status == "False" {
				reason = "ValidationFailed"
			}
			_ = unstructured.SetNestedSlice(object.Object, []interface{}{map[string]interface{}{
				"type": TemplateValidationCondition, "status": test.status, "reason": reason,
				"observedGeneration": test.generation,
			}}, "status", "conditions")
			if err := VerifyTemplateValidation(object, test.enabled); (err != nil) != test.wantErr {
				t.Fatalf("unexpected reconciliation result: %v", err)
			}
		})
	}
	if err := VerifyTemplateValidation(upgradeTemplate(), false); err == nil {
		t.Fatal("accepted a template with no validation result")
	}
}

func TestSafeUpgradeMode(t *testing.T) {
	for _, value := range []string{"", "false", "0"} {
		enabled, err := FBCRemediationEnabled(value)
		if err != nil || enabled {
			t.Fatalf("real fencing was enabled by %q: %v", value, err)
		}
	}
	enabled, err := FBCRemediationEnabled("true")
	if err != nil || !enabled {
		t.Fatalf("explicit fencing opt-in failed: %v", err)
	}
	if _, err := FBCRemediationEnabled("maybe"); err == nil {
		t.Fatal("accepted an invalid fencing opt-in")
	}
	object := SafeUpgradeTemplate("template", "namespace", "run-token")
	address, _, _ := unstructured.NestedString(object.Object, "spec", "template", "spec",
		"sharedparameters", "--ip")
	_, hasSecret, _ := unstructured.NestedFieldNoCopy(object.Object, "spec", "template", "spec", "sharedSecretName")
	_, validationEnabled, _ := unstructured.NestedFieldNoCopy(object.Object, "spec", "statusValidationSample")
	if object.GetKind() != "FenceAgentsRemediationTemplate" || address != "192.0.2.1" ||
		hasSecret || validationEnabled {
		t.Fatal("safe baseline must be a template with no real credentials or enabled status probe")
	}
}

func TestRemediationFromTemplate(t *testing.T) {
	template := upgradeTemplate()
	object, err := RemediationFromTemplate(template, "worker-1", "run-token")
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	actual, _, _ := unstructured.NestedMap(object.Object, "spec")
	if !reflect.DeepEqual(expected, actual) || object.GetUID() != "" ||
		object.GetKind() != "FenceAgentsRemediation" || object.GetName() != "worker-1" ||
		object.GetNamespace() != template.GetNamespace() {
		t.Fatal("remediation did not copy the persisted template into a fresh object")
	}
	_ = unstructured.SetNestedField(object.Object, int64(9), "spec", "retrycount")
	unchanged, _, _ := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	if !reflect.DeepEqual(expected, unchanged) {
		t.Fatal("remediation mutated the persisted template")
	}
}

func TestOwnedObjectAndEviction(t *testing.T) {
	object := upgradeTemplate()
	if err := VerifyOwnedObject(object, object.GetUID(), "run-token"); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []types.UID{"", "replacement-uid"} {
		if err := VerifyOwnedObject(object, uid, "run-token"); err == nil {
			t.Fatal("accepted an unowned/replaced object")
		}
	}
	if err := VerifyOwnedObject(object, object.GetUID(), "other-run"); err == nil {
		t.Fatal("accepted another run's object")
	}
	if WorkloadEvicted("original", "original", false, nil) ||
		WorkloadEvicted("original", "", false, fmt.Errorf("connection refused")) ||
		WorkloadEvicted("original", "", false, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"},
			"workload", fmt.Errorf("denied"))) {
		t.Fatal("mistook a live pod or API failure for eviction")
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "workload")
	if !WorkloadEvicted("original", "", false, notFound) ||
		!WorkloadEvicted("original", "original", true, nil) {
		t.Fatal("failed to recognize deletion")
	}
}
