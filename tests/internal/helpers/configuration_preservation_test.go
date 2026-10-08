package helpers

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func preservationFixture() (*unstructured.Unstructured, *apiextensionsv1.CustomResourceDefinition) {
	stringDefault := apiextensionsv1.JSONSchemaProps{Type: "string", Default: &apiextensionsv1.JSON{Raw: []byte(`"30s"`)}}
	integerDefault := apiextensionsv1.JSONSchemaProps{Type: "integer", Default: &apiextensionsv1.JSON{Raw: []byte(`3`)}}
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "example.medik8s.io/v1alpha1", "kind": "Configuration",
		"spec": map[string]interface{}{
			"peerUpdateInterval": "17m", "maxApiErrorThreshold": int64(5), "nullable": nil,
			"template":   map[string]interface{}{"spec": map[string]interface{}{}},
			"parameters": map[string]interface{}{"entry": map[string]interface{}{"value": "kept"}},
			"items":      []interface{}{map[string]interface{}{"name": "first"}, map[string]interface{}{"name": "second"}},
		},
	}}
	object.SetUID("original-uid")
	specSchema := apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
		"peerUpdateInterval": {Type: "string"}, "maxApiErrorThreshold": {Type: "integer"},
		"maxTimeForNoPeersResponse": stringDefault,
		"nullable":                  {Type: "string", Default: &apiextensionsv1.JSON{Raw: []byte(`"auto"`)}},
		"newFlag":                   {Type: "boolean", Default: &apiextensionsv1.JSON{Raw: []byte(`false`)}},
		"noDefault":                 {Type: "string"},
		"template": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"retries": integerDefault}},
		}},
		"newObject": {
			Type: "object", Default: &apiextensionsv1.JSON{Raw: []byte(`{}`)},
			Properties: map[string]apiextensionsv1.JSONSchemaProps{"retries": integerDefault},
		},
		"objectWithoutDefault": {
			Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"retries": integerDefault},
		},
		"items": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"name": {Type: "string"}, "timeout": stringDefault,
			},
		}}},
		"parameters": {Type: "object", AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
			Allows: true, Schema: &apiextensionsv1.JSONSchemaProps{
				Type:       "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{"value": {Type: "string"}, "timeout": stringDefault},
			},
		}},
	}}
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "configurations.example.medik8s.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.medik8s.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Configuration", Plural: "configurations"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": specSchema},
				}},
			}},
		},
	}

	return object, crd
}

func preservationClient(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	_ = apiextensionsv1.AddToScheme(scheme)

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestVerifyConfigurationPreserved(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*unstructured.Unstructured)
		wantErr bool
	}{
		{name: "unchanged"},
		{name: "status only", change: func(o *unstructured.Unstructured) {
			o.Object["status"] = map[string]interface{}{"ready": true}
		}},
		{name: "new SNR default", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "30s", "spec", "maxTimeForNoPeersResponse")
		}},
		{name: "new boolean default", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, false, "spec", "newFlag")
		}},
		{name: "new nested integer default", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, int64(3), "spec", "template", "spec", "retries")
		}},
		{name: "new object with nested defaults", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, map[string]interface{}{"retries": int64(3)}, "spec", "newObject")
		}},
		{name: "new default in existing list item", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []interface{}{
				map[string]interface{}{"name": "first", "timeout": "30s"},
				map[string]interface{}{"name": "second"},
			}, "spec", "items")
		}},
		{name: "new default in existing map entry", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "30s", "spec", "parameters", "entry", "timeout")
		}},
		{name: "wrong default", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "31s", "spec", "maxTimeForNoPeersResponse")
		}},
		{name: "unknown field", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "unexpected", "spec", "unknown")
		}},
		{name: "declared field without default", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "unexpected", "spec", "noDefault")
		}},
		{name: "parent without default", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(
				o.Object, map[string]interface{}{"retries": int64(3)}, "spec", "objectWithoutDefault",
			)
		}},
		{name: "new arbitrary map entry", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, map[string]interface{}{"timeout": "30s"}, "spec", "parameters", "new")
		}},
		{name: "changed setting", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "18m", "spec", "peerUpdateInterval")
		}},
		{name: "removed setting", wantErr: true, change: func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "spec", "maxApiErrorThreshold")
		}},
		{name: "changed null to schema default", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "auto", "spec", "nullable")
		}},
		{name: "changed object to null", wantErr: true, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, nil, "spec", "template")
		}},
		{name: "reordered list", wantErr: true, change: func(o *unstructured.Unstructured) {
			items, _, _ := unstructured.NestedSlice(o.Object, "spec", "items")
			items[0], items[1] = items[1], items[0]
			_ = unstructured.SetNestedSlice(o.Object, items, "spec", "items")
		}},
		{name: "added list item", wantErr: true, change: func(o *unstructured.Unstructured) {
			items, _, _ := unstructured.NestedSlice(o.Object, "spec", "items")
			_ = unstructured.SetNestedSlice(o.Object, append(items, map[string]interface{}{"name": "third"}), "spec", "items")
		}},
		{name: "replaced UID", wantErr: true, change: func(o *unstructured.Unstructured) { o.SetUID("replacement") }},
		{name: "deleting", wantErr: true, change: func(o *unstructured.Unstructured) {
			now := metav1.Now()
			o.SetDeletionTimestamp(&now)
		}},
		{name: "missing spec", wantErr: true, change: func(o *unstructured.Unstructured) { delete(o.Object, "spec") }},
		{name: "invalid spec", wantErr: true, change: func(o *unstructured.Unstructured) { o.Object["spec"] = "invalid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkConfigurationPreservation(t, test.change, test.wantErr)
		})
	}
}

func checkConfigurationPreservation(t *testing.T, change func(*unstructured.Unstructured), wantErr bool) {
	t.Helper()
	object, crd := preservationFixture()
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	original := object.DeepCopy()
	current := object.DeepCopy()
	if change != nil {
		change(current)
	}
	unchanged := current.DeepCopy()
	err := VerifyConfigurationPreserved(context.Background(), preservationClient(crd), current, object.GetUID(), spec)
	if (err != nil) != wantErr {
		t.Fatalf("unexpected preservation result: %v", err)
	}
	if !reflect.DeepEqual(current, unchanged) || !reflect.DeepEqual(object, original) ||
		!reflect.DeepEqual(spec, original.Object["spec"]) {
		t.Fatal("validation mutated configuration or baseline")
	}
}

func TestConfigurationPreservationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*apiextensionsv1.CustomResourceDefinition)
	}{
		{name: "wrong kind", change: func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Spec.Names.Kind = "Other" }},
		{name: "wrong group", change: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Spec.Group = "other.medik8s.io"
		}},
		{name: "wrong version", change: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Spec.Versions[0].Name = "v2"
		}},
		{name: "unserved version", change: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Spec.Versions[0].Served = false
		}},
		{name: "missing schema", change: func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Spec.Versions[0].Schema = nil
		}},
		{name: "malformed default", change: func(crd *apiextensionsv1.CustomResourceDefinition) {
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			field := spec.Properties["maxTimeForNoPeersResponse"]
			field.Default.Raw = []byte("not-json")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			object, crd := preservationFixture()
			spec, _, _ := unstructured.NestedMap(object.Object, "spec")
			_ = unstructured.SetNestedField(object.Object, "30s", "spec", "maxTimeForNoPeersResponse")
			test.change(crd)
			err := VerifyConfigurationPreserved(context.Background(), preservationClient(crd), object, object.GetUID(), spec)
			if err == nil {
				t.Fatal("accepted addition without a valid matching CRD default")
			}
		})
	}
	object, crd := preservationFixture()
	spec, _, _ := unstructured.NestedMap(object.Object, "spec")
	if err := VerifyConfigurationPreserved(context.Background(), nil, object, "", spec); err == nil {
		t.Fatal("accepted missing baseline UID")
	}
	_ = unstructured.SetNestedField(object.Object, "30s", "spec", "maxTimeForNoPeersResponse")
	err := VerifyConfigurationPreserved(context.Background(), preservationClient(), object, object.GetUID(), spec)
	if err == nil {
		t.Fatal("accepted missing CRD")
	}
	failure := errors.New("CRD access denied")
	api := interceptor.NewClient(preservationClient(crd), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failure },
	})
	err = VerifyConfigurationPreserved(context.Background(), api, object, object.GetUID(), spec)
	if !errors.Is(err, failure) {
		t.Fatalf("hid CRD access failure: %v", err)
	}
}
