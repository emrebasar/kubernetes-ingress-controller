package util

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// K8sObjectInfo describes a Kubernetes object.
type K8sObjectInfo struct {
	Name             string
	Namespace        string
	Annotations      map[string]string
	GroupVersionKind schema.GroupVersionKind
}

func deepCopy(m map[string]string) map[string]string {
	result := map[string]string{}
	for k, v := range m {
		result[k] = v
	}
	return result
}

type ObjMetaTyper interface {
	metav1.Object

	// Maybe there's a way to use metav1.Type interface in here but to the best of my knowledge
	// all k8s types instead of fulfilling that interface embed metav1.TypeMeta struct which
	// doesn't fulfill metav1.Type.
	// Hence the verbatim definition of interface funcion below.
	GroupVersionKind() schema.GroupVersionKind
}

func FromK8sObject(obj ObjMetaTyper) K8sObjectInfo {
	ret := K8sObjectInfo{
		Name:        obj.GetName(),
		Namespace:   obj.GetNamespace(),
		Annotations: deepCopy(obj.GetAnnotations()),
	}
	if gvk := obj.GroupVersionKind(); gvk.String() != "" {
		ret.GroupVersionKind = gvk
	}
	return ret
}
