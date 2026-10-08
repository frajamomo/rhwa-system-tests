package helpers

import (
	"context"
	"fmt"
	"reflect"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VerifyConfigurationPreserved keeps the original identity and every existing setting.
// Additions must match defaults declared in the installed CRD for this API version.
func VerifyConfigurationPreserved(
	ctx context.Context, apiClient client.Client, object *unstructured.Unstructured,
	uid types.UID, spec map[string]interface{},
) error {
	if uid == "" || object.GetUID() != uid || object.GetDeletionTimestamp() != nil {
		return fmt.Errorf("%s configuration was replaced or deleted: expected UID %s, got %s",
			object.GetKind(), uid, object.GetUID())
	}
	current, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s configuration spec is missing", object.GetKind())
	}
	if reflect.DeepEqual(spec, current) {
		return nil
	}
	specSchema, err := configurationSpecSchema(ctx, apiClient, object)
	if err != nil {
		return err
	}

	return verifyPreservedValue("spec", spec, current, specSchema)
}

func configurationSpecSchema(
	ctx context.Context, apiClient client.Client, object *unstructured.Unstructured,
) (*apiextensionsv1.JSONSchemaProps, error) {
	kind := object.GroupVersionKind()
	if apiClient == nil || kind.Group == "" || kind.Version == "" || kind.Kind == "" {
		return nil, fmt.Errorf("cannot validate configuration additions without a client and API identity")
	}
	crds := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := apiClient.List(ctx, crds); err != nil {
		return nil, fmt.Errorf("read configuration CRDs: %w", err)
	}

	for _, crd := range crds.Items {
		if crd.Spec.Group != kind.Group || crd.Spec.Names.Kind != kind.Kind {
			continue
		}

		for _, version := range crd.Spec.Versions {
			if version.Name != kind.Version || !version.Served ||
				version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			if specSchema, found := version.Schema.OpenAPIV3Schema.Properties["spec"]; found {
				return &specSchema, nil
			}
		}
	}

	return nil, fmt.Errorf("no served configuration spec schema found for %s", kind)
}

func configurationPropertySchema(schema *apiextensionsv1.JSONSchemaProps, key string) *apiextensionsv1.JSONSchemaProps {
	if schema == nil {
		return nil
	}
	if property, found := schema.Properties[key]; found {
		return &property
	}
	if schema.AdditionalProperties != nil {
		return schema.AdditionalProperties.Schema
	}

	return nil
}

func verifyPreservedValue(path string, before, after interface{}, schema *apiextensionsv1.JSONSchemaProps) error {
	if reflect.DeepEqual(before, after) {
		return nil
	}

	switch original := before.(type) {
	case map[string]interface{}:
		current, ok := after.(map[string]interface{})
		if !ok {
			break
		}

		return verifyPreservedMap(path, original, current, schema)
	case []interface{}:
		current, ok := after.([]interface{})
		if !ok || len(current) != len(original) {
			break
		}
		var itemSchema *apiextensionsv1.JSONSchemaProps
		if schema != nil && schema.Items != nil {
			itemSchema = schema.Items.Schema
		}

		for index, value := range original {
			if err := verifyPreservedValue(fmt.Sprintf("%s[%d]", path, index), value, current[index], itemSchema); err != nil {
				return err
			}
		}

		return nil
	}

	return fmt.Errorf("configuration field %s changed: expected %v, got %v", path, before, after)
}

func verifyPreservedMap(
	path string, before, after map[string]interface{}, schema *apiextensionsv1.JSONSchemaProps,
) error {
	for key, value := range before {
		actual, found := after[key]
		if !found {
			return fmt.Errorf("configuration field %s.%s was removed", path, key)
		}
		if err := verifyPreservedValue(path+"."+key, value, actual, configurationPropertySchema(schema, key)); err != nil {
			return err
		}
	}

	for key, value := range after {
		if _, existed := before[key]; existed {
			continue
		}
		if schema == nil {
			return fmt.Errorf("configuration field %s.%s was added without a CRD default", path, key)
		}
		property, declared := schema.Properties[key]
		if !declared || property.Default == nil {
			return fmt.Errorf("configuration field %s.%s was added without a CRD default", path, key)
		}
		var expected interface{}
		if err := json.Unmarshal(property.Default.Raw, &expected); err != nil {
			return fmt.Errorf("decode CRD default for %s.%s: %w", path, key, err)
		}
		if err := verifyPreservedValue(path+"."+key, expected, value, &property); err != nil {
			return err
		}
	}

	return nil
}
