package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/k3s-io/kine/e2e/config"
	"github.com/k3s-io/kine/e2e/helper/cluster"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/rand"

	// Import test features so their specs register.
	_ "github.com/k3s-io/kine/e2e/test_features/postgres_kerberos"
	_ "github.com/k3s-io/kine/e2e/test_features/postgres_kerberos_ccache"
	_ "github.com/k3s-io/kine/e2e/test_features/postgres_password"
)

// clusterName is generated per run so concurrent or repeated runs do not
// collide on a shared kind cluster.
var clusterName = "kine-e2e-" + rand.String(5)

func TestRunKineE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Kine E2E Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	// Allow overriding the image under test via env (the --kine-image flag is
	// registered in the config package and parsed by Ginkgo).
	if v := os.Getenv("KINE_IMAGE"); v != "" {
		config.KineImage = v
	}

	By("Building the kine image "+config.KineImage, func() {
		buildKineImage(config.KineImage)
	})

	var c *cluster.Cluster
	var err error
	By("Creating kind cluster "+clusterName, func() {
		c, err = cluster.Create(ctx, clusterName)
		Expect(err).NotTo(HaveOccurred())
	})
	// Tear the cluster (and everything in it) down at the end of the suite.
	DeferCleanup(c.Delete)

	// Publish the connection so specs can build their own clients.
	config.RestConfig = c.RestConfig

	By("Loading "+config.KineImage+" into kind", func() {
		Expect(c.LoadImage(ctx, config.KineImage)).To(Succeed())
	})
})

// buildKineImage builds the kine image under test from local source using the
// e2e Dockerfile, with the repository root as the build context.
func buildKineImage(image string) {
	root, err := filepath.Abs("..")
	Expect(err).NotTo(HaveOccurred())
	dockerfile, err := filepath.Abs("Dockerfile")
	Expect(err).NotTo(HaveOccurred())

	cmd := exec.Command("docker", "build", "-t", image, "-f", dockerfile, root)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed(), "docker build of kine image failed")
}
