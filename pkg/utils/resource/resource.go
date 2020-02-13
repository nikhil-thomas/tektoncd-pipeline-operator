package resource

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Mod struct {
	KeyPath []string
	Value   interface{}
}

func Clone(u *unstructured.Unstructured, mods []Mod) (*unstructured.Unstructured, error) {
	clone := u.DeepCopy()
	for _, item := range mods {

		err := unstructured.SetNestedField(clone.Object, item.Value, item.KeyPath...)
		if err != nil {
			return nil, err
		}
	}
	return clone, nil
}

func VersiondNamedResource(u *unstructured.Unstructured, suffix string) (*unstructured.Unstructured, error) {
	name := u.GetName()
	newName := name + suffix
	mods := []Mod{
		{
			KeyPath: []string{"metadata", "name"},
			Value:   newName,
		},
	}
	return Clone(u, mods)
}

func AddVersionNamedResources(ustrs *[]unstructured.Unstructured, version string) error {
	suffix := "-" + strings.ReplaceAll(version, ".", "-")
	clones := []unstructured.Unstructured{}
	for _, item := range *ustrs {
		clone, err := VersiondNamedResource(&item, suffix)
		if err != nil {
			return err
		}
		clones = append(clones, *clone)
	}
	*ustrs = append(*ustrs, clones...)
	return nil
}
