package mdrutils

import (
	"context"
	"reflect"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/mdr-operator/internal/mdrparams"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func upgradeTemplate() *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": mdrparams.CRDGroup + "/" + mdrparams.CRDVersion,
		"kind":       "MachineDeletionRemediationTemplate",
		"spec":       map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{}}},
	}}
	object.SetNamespace("test-namespace")
	object.SetName("upgrade-template")
	object.SetUID("original-uid")

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
		{name: "unchanged empty template"},
		{name: "status only", change: func(current *unstructured.Unstructured) {
			current.Object["status"] = map[string]interface{}{}
		}},
		{name: "recreated", wantErr: true, change: func(current *unstructured.Unstructured) {
			current.SetUID("replacement-uid")
		}},
		{name: "spec missing", wantErr: true, change: func(current *unstructured.Unstructured) {
			delete(current.Object, "spec")
		}},
		{name: "nested spec missing", wantErr: true, change: func(current *unstructured.Unstructured) {
			unstructured.RemoveNestedField(current.Object, "spec", "template", "spec")
		}},
		{name: "extra field", wantErr: true, change: func(current *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(current.Object, "unexpected", "spec", "template", "spec", "newField")
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

func safeResponse(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	object, err := SafeProbe(upgradeTemplate(), "nonexistent-probe", "run-token")
	if err != nil {
		t.Fatal(err)
	}
	object.SetUID("fresh-probe-uid")
	conditions := []interface{}{}
	for _, conditionType := range []string{mdrparams.ProcessingConditionType, mdrparams.SucceededConditionType} {
		conditions = append(conditions, map[string]interface{}{
			"type": conditionType, "status": "False", "reason": mdrparams.ConditionReasonStoppedByNHC,
		})
	}
	if err := unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}

	return object
}

func TestSafeProbe(t *testing.T) {
	template := upgradeTemplate()
	object, err := SafeProbe(template, "nonexistent-probe", "run-token")
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	actual, _, _ := unstructured.NestedMap(object.Object, "spec")
	if !reflect.DeepEqual(actual, expected) || object.GetUID() != "" ||
		object.GetKind() != "MachineDeletionRemediation" || object.GetName() != "nonexistent-probe" ||
		object.GetNamespace() != template.GetNamespace() || object.GetLabels()[helpers.FBCRunLabel] != "run-token" ||
		object.GetAnnotations()[mdrparams.NHCTimedOutAnnotationKey] != mdrparams.NHCTimedOutAnnotationValue {
		t.Fatal("probe must copy the persisted template into a fresh, owned, deletion-blocked request")
	}
	_ = unstructured.SetNestedField(object.Object, "probe-only", "spec", "test")
	unchanged, _, _ := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	if !reflect.DeepEqual(expected, unchanged) {
		t.Fatal("probe mutated persisted configuration")
	}
	unstructured.RemoveNestedField(template.Object, "spec", "template", "spec")
	if _, err := SafeProbe(template, "nonexistent-probe", "run-token"); err == nil {
		t.Fatal("accepted a template with no remediation spec")
	}
}

func TestVerifySafeProbe(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*unstructured.Unstructured)
		wantErr bool
	}{
		{name: "fresh stopped response"},
		{name: "stale GA UID", wantErr: true, change: func(object *unstructured.Unstructured) {
			object.SetUID("ga-probe-uid")
		}},
		{name: "missing conditions", wantErr: true, change: func(object *unstructured.Unstructured) {
			delete(object.Object, "status")
		}},
		{name: "wrong owner", wantErr: true, change: func(object *unstructured.Unstructured) {
			object.SetLabels(map[string]string{helpers.FBCRunLabel: "other-run"})
		}},
		{name: "missing safety gate", wantErr: true, change: func(object *unstructured.Unstructured) {
			object.SetAnnotations(nil)
		}},
		{name: "terminating request", wantErr: true, change: func(object *unstructured.Unstructured) {
			timestamp := metav1.Now()
			object.SetDeletionTimestamp(&timestamp)
		}},
		{name: "wrong stopped reason", wantErr: true, change: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(object.Object, []interface{}{
				map[string]interface{}{"type": "Processing", "status": "False", "reason": "NodeNotFound"},
				map[string]interface{}{"type": "Succeeded", "status": "False", "reason": "NodeNotFound"},
			}, "status", "conditions")
		}},
		{name: "only processing stopped", wantErr: true, change: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(object.Object, []interface{}{map[string]interface{}{
				"type": "Processing", "status": "False", "reason": mdrparams.ConditionReasonStoppedByNHC,
			}}, "status", "conditions")
		}},
		{name: "unexpected success", wantErr: true, change: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(object.Object, []interface{}{map[string]interface{}{
				"type": "Succeeded", "status": "True", "reason": "RemediationFinished",
			}}, "status", "conditions")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := safeResponse(t)
			if test.change != nil {
				test.change(object)
			}
			if err := VerifySafeProbe(object, "fresh-probe-uid", "run-token"); (err != nil) != test.wantErr {
				t.Fatalf("unexpected reconciliation result: %v", err)
			}
		})
	}
	if err := VerifySafeProbe(safeResponse(t), "", "run-token"); err == nil {
		t.Fatal("accepted a missing probe UID")
	}
}
