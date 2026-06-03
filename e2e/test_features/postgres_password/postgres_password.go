// Package postgres_password verifies that kine authenticates to PostgreSQL with
// a plain user/password DSN (scram-sha-256), with no Kerberos involved.
package postgres_password

import (
	"context"
	"time"

	"github.com/k3s-io/kine/e2e/config"
	"github.com/k3s-io/kine/e2e/helper/kube"
	"github.com/k3s-io/kine/e2e/setup/kine"
	"github.com/k3s-io/kine/e2e/setup/postgres"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PostgreSQL password authentication", Ordered, func() {
	// k is the cluster client, owned by this Describe block and built from the
	// connection the suite published. ns is created per run with GenerateName
	// in BeforeAll and shared across the ordered specs below.
	var (
		k  *kube.Client
		ns string
	)

	const appName = "kine-password"

	BeforeAll(func(ctx context.Context) {
		var err error
		k, err = kube.New(config.RestConfig)
		Expect(err).NotTo(HaveOccurred())

		ns, err = k.CreateNamespace(ctx, config.NamespacePrefix)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) {
			Expect(k.DeleteNamespace(ctx, ns)).To(Succeed())
		})

		// A PostgreSQL configured for scram-sha-256 on TCP; no Kerberos.
		Expect(postgres.DeployPassword(k, ns)(ctx)).To(Succeed())
	})

	Context("with the correct user and password", Ordered, func() {
		BeforeAll(func(ctx context.Context) {
			err := kine.Install(k, kine.Options{
				Namespace:   ns,
				Image:       config.KineImage,
				Endpoint:    postgres.PasswordEndpoint(ns, appName),
				WithService: true,
			})(ctx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("authenticates to PostgreSQL and serves the etcd API", func(ctx context.Context) {
			// kine only opens its :2379 listener after connecting to the
			// database and creating its schema, so the deployment becoming
			// ready proves the password authentication succeeded.
			Eventually(func(g Gomega) {
				ready, err := k.DeploymentReady(ctx, ns, kine.Name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(BeTrue())
			}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		})

		It("authenticated as the kine role and created the kine table", func(ctx context.Context) {
			By("the kine table is owned by the kine role", func() {
				postgres.AssertKineOwnsTable(ctx, k, ns)
			})

			By("at least one connection authenticated as the kine role", func() {
				postgres.AssertKineRoleHasConnections(ctx, k, ns)
			})
		})
	})
})
