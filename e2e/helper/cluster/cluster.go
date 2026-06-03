// Package cluster manages the kind cluster the e2e suite runs against. It
// drives the `kind` binary directly (the same exec pattern the suite uses for
// `docker build`), writing an isolated kubeconfig so it never touches the
// developer's default one.
package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/onsi/ginkgo/v2"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Cluster is a kind cluster created by the suite.
type Cluster struct {
	Name       string
	kubeconfig string
	RestConfig *rest.Config
}

// Create provisions a kind cluster named name and returns a rest.Config built
// from its isolated kubeconfig. The caller is responsible for Delete.
func Create(ctx context.Context, name string) (*Cluster, error) {
	kubeconfig, err := os.CreateTemp("", "kine-e2e-kubeconfig-*")
	if err != nil {
		return nil, fmt.Errorf("create temp kubeconfig: %w", err)
	}
	if err := kubeconfig.Close(); err != nil {
		return nil, fmt.Errorf("close temp kubeconfig: %w", err)
	}

	c := &Cluster{Name: name, kubeconfig: kubeconfig.Name()}

	if err := c.run(ctx, "create", "cluster",
		"--name", name, "--kubeconfig", c.kubeconfig, "--wait", "120s"); err != nil {
		_ = os.Remove(c.kubeconfig)
		return nil, fmt.Errorf("kind create cluster: %w", err)
	}

	c.RestConfig, err = clientcmd.BuildConfigFromFlags("", c.kubeconfig)
	if err != nil {
		_ = c.Delete(ctx)
		return nil, fmt.Errorf("build rest config: %w", err)
	}
	return c, nil
}

// LoadImage loads a locally-built image into the cluster's nodes.
func (c *Cluster) LoadImage(ctx context.Context, image string) error {
	if err := c.run(ctx, "load", "docker-image", image, "--name", c.Name); err != nil {
		return fmt.Errorf("kind load docker-image %s: %w", image, err)
	}
	return nil
}

// Delete tears the cluster down and removes the temp kubeconfig.
func (c *Cluster) Delete(ctx context.Context) error {
	err := c.run(ctx, "delete", "cluster", "--name", c.Name)
	if c.kubeconfig != "" {
		_ = os.Remove(c.kubeconfig)
	}
	if err != nil {
		return fmt.Errorf("kind delete cluster: %w", err)
	}
	return nil
}

// run invokes the kind binary, streaming output to the Ginkgo writer.
func (c *Cluster) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "kind", args...)
	cmd.Stdout = ginkgo.GinkgoWriter
	cmd.Stderr = ginkgo.GinkgoWriter
	return cmd.Run()
}
