// Package postgres_kerberos_ccache verifies that kine authenticates to a
// GSSAPI-configured PostgreSQL from a mounted Kerberos credential cache (a
// pre-minted TGT) rather than a keytab. The PostgreSQL side is identical to the
// keytab suite: the server never sees how the client obtained its ticket, so
// this exercises only kine's credential-cache code path (krb5_ccache /
// KRB5CCNAME resolution).
package postgres_kerberos_ccache

import (
	"context"
	"time"

	"github.com/k3s-io/kine/e2e/config"
	"github.com/k3s-io/kine/e2e/helper/kube"
	"github.com/k3s-io/kine/e2e/helper/setup"
	"github.com/k3s-io/kine/e2e/setup/kerberos"
	"github.com/k3s-io/kine/e2e/setup/kine"
	"github.com/k3s-io/kine/e2e/setup/postgres"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PostgreSQL Kerberos credential-cache authentication", Ordered, func() {
	// k is the cluster client, owned by this Describe block and built from the
	// connection the suite published. ns is created per run with GenerateName in
	// BeforeAll and shared across the ordered specs below.
	var (
		k  *kube.Client
		ns string
	)

	BeforeAll(func(ctx context.Context) {
		var err error
		k, err = kube.New(config.RestConfig)
		Expect(err).NotTo(HaveOccurred())

		ns, err = k.CreateNamespace(ctx, config.NamespacePrefix)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) {
			Expect(k.DeleteNamespace(ctx, ns)).To(Succeed())
		})

		// Shared infrastructure: the KDC, the per-run principals/keytabs, the kine
		// credential cache minted from the kine principal, and the
		// GSSAPI-configured PostgreSQL.
		err = setup.AllFailFast(
			kerberos.Deploy(k, ns),
			kerberos.BootstrapKeytabs(k, ns, postgres.ServiceFQDN(ns), postgres.Role),
			kerberos.BootstrapCCache(k, ns, postgres.Role),
			postgres.Deploy(k, ns),
		)(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	Context("with an explicit krb5_ccache path", Ordered, func() {
		BeforeAll(func(ctx context.Context) {
			err := kine.Install(k, kine.Options{
				Namespace:    ns,
				Image:        config.KineImage,
				CCacheSecret: kerberos.KineCCacheSecret,
				WithService:  true,
			})(ctx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("authenticates to PostgreSQL and serves the etcd API", func(ctx context.Context) {
			// kine only opens its :2379 listener after connecting to the database
			// and creating its schema, so the deployment becoming ready proves the
			// GSSAPI handshake from the credential cache succeeded.
			Eventually(func(g Gomega) {
				ready, err := k.DeploymentReady(ctx, ns, kine.Name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(BeTrue())
			}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		})

		It("authenticated as the kine role and created the kine table", func(ctx context.Context) {
			By("the kine table is owned by the Kerberos-mapped role", func() {
				postgres.AssertKineOwnsTable(ctx, k, ns)
			})

			By("at least one connection authenticated as the kine role", func() {
				postgres.AssertKineRoleHasConnections(ctx, k, ns)
			})
		})
	})

	Context("with the cache located via KRB5CCNAME", Ordered, func() {
		const name = "kine-ccache-env"

		BeforeAll(func(ctx context.Context) {
			// A valueless krb5_ccache parameter plus KRB5CCNAME=FILE:<path>: kine
			// must resolve the cache from the environment rather than an explicit
			// DSN path.
			err := kine.Install(k, kine.Options{
				Namespace:    ns,
				Name:         name,
				Image:        config.KineImage,
				CCacheSecret: kerberos.KineCCacheSecret,
				CCacheViaEnv: true,
			})(ctx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("authenticates to PostgreSQL and serves the etcd API", func(ctx context.Context) {
			Eventually(func(g Gomega) {
				ready, err := k.DeploymentReady(ctx, ns, name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(BeTrue())
			}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		})
	})
})
