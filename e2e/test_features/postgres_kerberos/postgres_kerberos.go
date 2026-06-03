// Package postgres_kerberos verifies that kine authenticates to a
// GSSAPI-configured PostgreSQL using a mounted Kerberos keytab.
package postgres_kerberos

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

var _ = Describe("PostgreSQL Kerberos authentication", Ordered, func() {
	// k is the cluster client, owned by this Describe block and built from the
	// connection the suite published. ns is created per run with GenerateName
	// in BeforeAll and shared across the ordered specs below.
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

		// Shared infrastructure: the KDC, the per-run principals/keytabs, and
		// the GSSAPI-configured PostgreSQL.
		err = setup.AllFailFast(
			kerberos.Deploy(k, ns),
			kerberos.BootstrapKeytabs(k, ns, postgres.ServiceFQDN(ns), postgres.Role),
			postgres.Deploy(k, ns),
		)(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	Context("with a keytab matching the database principal", Ordered, func() {
		BeforeAll(func(ctx context.Context) {
			err := kine.Install(k, kine.Options{Namespace: ns, Image: config.KineImage, WithService: true})(ctx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("authenticates to PostgreSQL and serves the etcd API", func(ctx context.Context) {
			// kine only opens its :2379 listener after connecting to the
			// database and creating its schema, so the deployment becoming
			// ready proves the GSSAPI handshake succeeded.
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

	Context("with a principal absent from the keytab", Ordered, func() {
		const badName = "kine-bad-principal"

		BeforeAll(func(ctx context.Context) {
			err := kine.Install(k, kine.Options{
				Namespace: ns,
				Name:      badName,
				Image:     config.KineImage,
				Principal: "nosuchuser@" + kerberos.Realm,
			})(ctx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("never becomes ready", func(ctx context.Context) {
			// kine opens its :2379 listener (and so reports ready) only after a
			// successful database connection. The positive context uses an
			// identical setup differing only in the principal, so a deployment
			// that never becomes ready here is the externally observable proof
			// that the GSSAPI handshake failed. Give it time to attempt and
			// retry, then assert it stays not-ready.
			Consistently(func(g Gomega) {
				ready, err := k.DeploymentReady(ctx, ns, badName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(BeFalse())
			}).WithTimeout(90 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})
	})

	// This Context mutates shared KDC state (realm-wide max ticket life, then the
	// kine principal's key), so it runs last. Earlier specs hold their own
	// already-authenticated connections, which PostgreSQL never re-checks, so they
	// are unaffected. All kine deployments map to the same database role, so each
	// is tagged with a distinct application_name and every server-side query and
	// pg_terminate_backend is scoped to it.
	Context("when Kerberos tickets are short-lived", Ordered, Label("ticket-expiry"), func() {
		const (
			ticketLife = 60 * time.Second
			appA       = "kine-exp-a"
			appB       = "kine-exp-b"
			appC       = "kine-exp-c"
		)
		var installed time.Time

		BeforeAll(func(ctx context.Context) {
			// Cap the TGT (krbtgt) and the kine principal so freshly minted tickets
			// die in ticketLife instead of the realm default (~24h).
			Expect(kerberos.SetMaxTicketLife(k, ns, ticketLife,
				"krbtgt/"+kerberos.Realm+"@"+kerberos.Realm, kine.DefaultPrincipal)(ctx)).To(Succeed())

			// A recycles connections aggressively, so it re-authenticates from the
			// keytab over time; C keeps its startup connection; B is the rotation
			// victim, reused by the recovery spec.
			err := setup.AllFailFast(
				kine.Install(k, kine.Options{
					Namespace: ns, Name: appA, Image: config.KineImage, AppName: appA,
					ConnMaxLifetime: 20 * time.Second, MaxIdleConns: 1,
				}),
				kine.Install(k, kine.Options{Namespace: ns, Name: appB, Image: config.KineImage, AppName: appB}),
				kine.Install(k, kine.Options{Namespace: ns, Name: appC, Image: config.KineImage, AppName: appC}),
			)(ctx)
			Expect(err).NotTo(HaveOccurred())
			installed = time.Now()

			// Every deployment must authenticate once before we exercise expiry.
			for _, name := range []string{appA, appB, appC} {
				Eventually(func(g Gomega) {
					ready, err := k.DeploymentReady(ctx, ns, name)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(ready).To(BeTrue())
				}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
			}
		})

		It("keeps serving on a connection established before the ticket expired", func(ctx context.Context) {
			// C uses the default (unlimited) connection lifetime, so its startup
			// connection is never recycled. Capture its backends, wait past the
			// ticket lifetime, and confirm one is still there: an established session
			// is never re-authenticated, so expiry does not drop it.
			before, err := postgres.SessionPIDs(ctx, k, ns, appC)
			Expect(err).NotTo(HaveOccurred())
			Expect(before).NotTo(BeEmpty())

			waitPast(installed, ticketLife+15*time.Second)

			after, err := postgres.SessionPIDs(ctx, k, ns, appC)
			Expect(err).NotTo(HaveOccurred())
			Expect(intersects(before, after)).To(BeTrue(),
				"a connection established before expiry should still be alive")

			ready, err := k.DeploymentReady(ctx, ns, appC)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeTrue())
		})

		It("re-authenticates on new connections after the ticket expired", func(ctx context.Context) {
			// By now the ticket that first authenticated A is long dead. Drop A's
			// connections and prove it dials a brand-new connection authenticated
			// from the keytab, i.e. a session whose backend_start is after tRef.
			waitPast(installed, ticketLife+15*time.Second)

			tRef, err := postgres.ServerNow(ctx, k, ns)
			Expect(err).NotTo(HaveOccurred())
			Expect(postgres.TerminateBackends(ctx, k, ns, appA)).To(Succeed())

			Eventually(func(g Gomega) {
				n, err := postgres.CountSessionsSince(ctx, k, ns, appA, tRef)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(n).To(BeNumerically(">=", 1))
			}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
		})

		It("fails to open new connections after the principal key is rotated", func(ctx context.Context) {
			// Rotate the kine key at the KDC but leave the mounted keytab stale, so
			// new GSSAPI logins must fail. A failed handshake never reaches
			// pg_stat_activity, so any fresh session here would be a real auth.
			Expect(kerberos.RotatePrincipalKey(k, ns, kine.DefaultPrincipal)(ctx)).To(Succeed())

			tRef, err := postgres.ServerNow(ctx, k, ns)
			Expect(err).NotTo(HaveOccurred())
			Expect(postgres.TerminateBackends(ctx, k, ns, appB)).To(Succeed())

			Consistently(func(g Gomega) {
				n, err := postgres.CountSessionsSince(ctx, k, ns, appB, tRef)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(n).To(Equal(0))
			}).WithTimeout(60 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})

		It("recovers once the keytab is refreshed with the rotated key", func(ctx context.Context) {
			// Re-export the rotated key and overwrite the mounted keytab Secret. kine
			// reloads the keytab from disk on every connection, so once the kubelet
			// propagates the new Secret to the volume, B's next dial succeeds with no
			// restart. The generous timeout absorbs that propagation.
			Expect(kerberos.RefreshKeytab(k, ns, kine.DefaultPrincipal, kerberos.KineKeytabSecret)(ctx)).To(Succeed())

			tRef, err := postgres.ServerNow(ctx, k, ns)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				n, err := postgres.CountSessionsSince(ctx, k, ns, appB, tRef)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(n).To(BeNumerically(">=", 1))
			}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		})
	})
})

// waitPast sleeps until at least d has elapsed since start, letting the expiry
// specs share the single ticket-lifetime clock that began in BeforeAll instead
// of each sleeping a full lifetime.
func waitPast(start time.Time, d time.Duration) {
	if remaining := d - time.Since(start); remaining > 0 {
		time.Sleep(remaining)
	}
}

// intersects reports whether a and b share at least one element. Used to assert
// that a pre-expiry connection is still among the live ones.
func intersects(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, x := range a {
		seen[x] = struct{}{}
	}
	for _, y := range b {
		if _, ok := seen[y]; ok {
			return true
		}
	}
	return false
}
