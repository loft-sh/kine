// Package config holds e2e settings shared between the suite bootstrap and the
// test feature packages (so they agree on the image name, namespace, and the
// cluster they run against). The suite populates RestConfig in BeforeSuite;
// specs build their own kube.Client from it and hold the client in their
// Describe block.
package config

import (
	"flag"

	"k8s.io/client-go/rest"
)

// NamespacePrefix is the GenerateName prefix for the per-run test namespace.
const NamespacePrefix = "kine-e2e-"

// KineImage is the kine image under test. The suite builds it from local
// source under this tag, and the specs deploy it.
var KineImage string

// RestConfig is the connection to the kind cluster created by the suite. It is
// set once in BeforeSuite and read by specs to build their Kubernetes clients.
var RestConfig *rest.Config

func init() {
	flag.StringVar(&KineImage, "kine-image", "kine:e2e", "kine image under test (built from local source by the suite)")
}
