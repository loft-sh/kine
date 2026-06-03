// Package kube provides a small Kubernetes client wrapper shared by the e2e
// setup steps: applying objects, waiting for readiness, and execing into pods.
// The clients are carried on a Client value owned by the spec (constructed via
// New from the cluster's rest.Config); this package holds no state of its own.
package kube

import (
	"bytes"
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
)

// Client bundles the controller-runtime and client-go clients for a cluster.
type Client struct {
	restCfg    *rest.Config
	crClient   clientpkg.Client
	kubeClient kubernetes.Interface
}

// New builds a Client from cfg.
func New(cfg *rest.Config) (*Client, error) {
	cr, err := clientpkg.New(cfg, clientpkg.Options{})
	if err != nil {
		return nil, fmt.Errorf("create controller-runtime client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}
	return &Client{restCfg: cfg, crClient: cr, kubeClient: cs}, nil
}

// Apply creates obj, treating AlreadyExists as success so steps are re-runnable.
func (c *Client) Apply(ctx context.Context, obj clientpkg.Object) error {
	if err := c.crClient.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// UpdateSecret overwrites the Data of an existing Secret. It reads the Secret
// first so the current resourceVersion is carried into the update; Apply cannot
// do this (it only Creates and swallows AlreadyExists). Used to refresh a
// mounted keytab in place after a key rotation.
func (c *Client) UpdateSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	secret := &corev1.Secret{}
	if err := c.crClient.Get(ctx, clientpkg.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return fmt.Errorf("get secret %s/%s: %w", namespace, name, err)
	}
	secret.Data = data
	if err := c.crClient.Update(ctx, secret); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// DeploymentReady reports whether the named Deployment has at least one ready
// replica. A missing Deployment is reported as not-ready (nil error) so callers
// can poll it cleanly.
func (c *Client) DeploymentReady(ctx context.Context, namespace, name string) (bool, error) {
	dep := &appsv1.Deployment{}
	if err := c.crClient.Get(ctx, clientpkg.ObjectKey{Namespace: namespace, Name: name}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas >= 1, nil
}

// WaitDeploymentReady blocks until the named Deployment reports at least one
// ready replica or the timeout elapses. It is used by the setup steps; specs
// should poll DeploymentReady via gomega.Eventually instead.
func (c *Client) WaitDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := wait.PollUntilContextCancel(pollCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		return c.DeploymentReady(ctx, namespace, name)
	}); err != nil {
		return fmt.Errorf("wait for deployment %s/%s ready: %w", namespace, name, err)
	}
	return nil
}

// FirstPodName returns the name of the first pod matching labelSelector.
func (c *Client) FirstPodName(ctx context.Context, namespace, labelSelector string) (string, error) {
	pods, err := c.kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods in %s matching %q", namespace, labelSelector)
	}
	return pods.Items[0].Name, nil
}

// ExecInPod runs cmd in the first container of pod and returns its stdout/stderr.
func (c *Client) ExecInPod(ctx context.Context, namespace, pod string, cmd []string) (stdout, stderr string, err error) {
	req := c.kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: cmd,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.restCfg, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("create executor: %w", err)
	}
	var outBuf, errBuf bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &outBuf, Stderr: &errBuf}); err != nil {
		return outBuf.String(), errBuf.String(), fmt.Errorf("exec %v in %s/%s: %w (stderr: %s)", cmd, namespace, pod, err, errBuf.String())
	}
	return outBuf.String(), errBuf.String(), nil
}

// CreateNamespace creates a namespace using GenerateName and returns the
// server-assigned name.
func (c *Client) CreateNamespace(ctx context.Context, generateName string) (string, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: generateName}}
	if err := c.crClient.Create(ctx, ns); err != nil {
		return "", fmt.Errorf("create namespace %s*: %w", generateName, err)
	}
	return ns.Name, nil
}

// DeleteNamespace removes the namespace (best effort; NotFound is ignored).
func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	err := c.crClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
