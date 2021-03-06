// +build tools

package tools

import (
	_ "github.com/knative/pkg/apis/istio/v1alpha3"
	_ "k8s.io/apimachinery/pkg/util/sets/types"
	_ "k8s.io/code-generator/cmd/client-gen"
	_ "k8s.io/code-generator/cmd/deepcopy-gen"
	_ "k8s.io/code-generator/cmd/defaulter-gen"
	_ "k8s.io/code-generator/cmd/informer-gen"
	_ "k8s.io/code-generator/cmd/lister-gen"
)
