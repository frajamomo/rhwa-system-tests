package sbrutils

import (
	"context"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestSafeSpecWithoutStorage(t *testing.T) {
	spec := SafeSpec("run-token", "")
	if _, found := spec["sharedStorageClass"]; found {
		t.Fatal("no-storage probe must omit sharedStorageClass")
	}

	selector, ok := spec["nodeSelector"].(map[string]interface{})
	if !ok || len(selector) != 1 || selector[RunLabel] != "run-token" {
		t.Fatalf("unsafe node selector: %v", spec["nodeSelector"])
	}
}

func TestVerifyConfiguration(t *testing.T) {
	spec := SafeSpec("run-token", "")
	object := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	object.SetUID("original-uid")

	for _, test := range []struct {
		name    string
		change  func(*unstructured.Unstructured)
		uid     types.UID
		wantErr bool
	}{
		{name: "unchanged", uid: object.GetUID()},
		{name: "status only", uid: object.GetUID(), change: func(changed *unstructured.Unstructured) {
			changed.Object["status"] = map[string]interface{}{"ready": false}
		}},
		{name: "recreated", uid: object.GetUID(), wantErr: true, change: func(changed *unstructured.Unstructured) {
			changed.SetUID("replacement-uid")
		}},
		{name: "missing baseline identity", wantErr: true},
		{name: "changed field", uid: object.GetUID(), wantErr: true, change: func(changed *unstructured.Unstructured) {
			changed.Object["spec"] = SafeSpec("different-token", "")
		}},
		{name: "added field", uid: object.GetUID(), wantErr: true, change: func(changed *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(changed.Object, "unexpected-storage", "spec", "sharedStorageClass")
		}},
		{name: "missing spec", uid: object.GetUID(), wantErr: true, change: func(changed *unstructured.Unstructured) {
			delete(changed.Object, "spec")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := object.DeepCopy()
			if test.change != nil {
				test.change(changed)
			}

			err := helpers.VerifyConfigurationPreserved(context.Background(), nil, changed, test.uid, spec)
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected preservation result: %v", err)
			}
		})
	}
}
