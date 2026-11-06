// Package postgres deploys a PostgreSQL instance for the e2e suite in one of two
// auth modes:
//
//   - Deploy: GSSAPI (Kerberos). It mounts the service keytab, sets
//     krb_server_keyfile, and rewrites pg_hba.conf so TCP clients authenticate
//     via gss while local/loopback stay trusted for bootstrap.
//   - DeployPassword: plain user/password. It creates the kine role with a
//     password and rewrites pg_hba.conf so TCP clients authenticate via
//     scram-sha-256, no Kerberos involved.
//
// Both modes keep local/loopback on trust so the superuser psql checks work.
package postgres

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/k3s-io/kine/e2e/helper/kube"
	"github.com/k3s-io/kine/e2e/helper/setup"
	"github.com/k3s-io/kine/e2e/setup/kerberos"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	// Service is the Service (and Deployment) name.
	Service = "postgres"
	// Image is the official PostgreSQL image (built --with-gssapi).
	Image = "postgres:17"
	// Database is the database kine connects to; Role is the login role that
	// the kine principal maps to after include_realm=0 strips the realm, and
	// also the role used for plain password auth.
	Database = "kubernetes"
	Role     = "kine"
	// Password is the kine role's password in the plain user/password mode.
	// Test-only; the database is recreated per run.
	Password = "kine-e2e-password"

	initConfigMap         = "postgres-init"
	passwordInitConfigMap = "postgres-init-password"
	keytabMountPath       = "/etc/postgres-keytab"
)

// ServiceFQDN returns the in-cluster FQDN of the PostgreSQL Service.
func ServiceFQDN(namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", Service, namespace)
}

// PasswordEndpoint returns the plain user/password DSN kine uses to reach the
// password-mode PostgreSQL. When appName is non-empty it is forwarded as the
// PostgreSQL application_name so server-side assertions can be scoped to this
// deployment.
func PasswordEndpoint(namespace, appName string) string {
	q := url.Values{"sslmode": {"disable"}}
	if appName != "" {
		q.Set("application_name", appName)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(Role, Password),
		Host:     fmt.Sprintf("%s:5432", ServiceFQDN(namespace)),
		Path:     "/" + Database,
		RawQuery: q.Encode(),
	}
	return u.String()
}

// psql runs a single statement as the postgres superuser over the local trust
// socket and returns trimmed stdout. The expiry specs use it to observe and
// manipulate kine's sessions from the server side.
func psql(ctx context.Context, k *kube.Client, namespace, sql string) (string, error) {
	pod, err := k.FirstPodName(ctx, namespace, "app="+Service)
	if err != nil {
		return "", err
	}
	out, _, err := k.ExecInPod(ctx, namespace, pod, []string{
		"psql", "-U", "postgres", "-d", Database, "-tAc", sql,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ServerNow returns the PostgreSQL server clock as a timestamp string suitable
// as a reference for CountSessionsSince. Reading the server clock (not the test
// runner's, which is off-cluster) keeps the comparison free of skew.
func ServerNow(ctx context.Context, k *kube.Client, namespace string) (string, error) {
	return psql(ctx, k, namespace, "SELECT now()")
}

// SessionPIDs returns the backend PIDs of appName's current connections. A
// connection that survives ticket expiry keeps its PID.
func SessionPIDs(ctx context.Context, k *kube.Client, namespace, appName string) ([]string, error) {
	out, err := psql(ctx, k, namespace,
		fmt.Sprintf("SELECT pid FROM pg_stat_activity WHERE application_name = '%s'", appName))
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// CountSessionsSince counts appName's connections established after tRef (from
// ServerNow), i.e. the freshly authenticated ones. A failed GSSAPI handshake
// never reaches pg_stat_activity (a backend is registered only after auth
// succeeds), so this counts only genuinely authenticated sessions.
func CountSessionsSince(ctx context.Context, k *kube.Client, namespace, appName, tRef string) (int, error) {
	out, err := psql(ctx, k, namespace, fmt.Sprintf(
		"SELECT count(*) FROM pg_stat_activity WHERE application_name = '%s' AND backend_start > '%s'",
		appName, tRef))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// TerminateBackends drops all of appName's connections, forcing the owning kine
// to dial fresh ones (which re-run the GSSAPI handshake). Scoping by
// application_name keeps it from touching other kine deployments, which share
// the same database role.
func TerminateBackends(ctx context.Context, k *kube.Client, namespace, appName string) error {
	_, err := psql(ctx, k, namespace, fmt.Sprintf(
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
			"WHERE application_name = '%s' AND pid <> pg_backend_pid()", appName))
	return err
}

// AssertKineOwnsTable asserts that the kine table exists and is owned by the
// kine role. kine creates its schema as whatever role it authenticated as, so
// the owner being Role is server-side proof the connection logged in as kine.
// The Kerberos and password specs verify this identically and differ only in
// how that login happens, so they share this check. Must run inside a spec.
func AssertKineOwnsTable(ctx context.Context, k *kube.Client, namespace string) {
	GinkgoHelper()
	owner, err := psql(ctx, k, namespace, "SELECT tableowner FROM pg_tables WHERE tablename='kine'")
	Expect(err).NotTo(HaveOccurred())
	Expect(owner).To(Equal(Role))
}

// AssertKineRoleHasConnections asserts that at least one connection in
// pg_stat_activity is authenticated as the kine role. A backend is registered
// only after authentication succeeds, so a non-zero count is server-side proof
// kine logged in as Role. Shared by the Kerberos and password specs. Must run
// inside a spec.
func AssertKineRoleHasConnections(ctx context.Context, k *kube.Client, namespace string) {
	GinkgoHelper()
	count, err := psql(ctx, k, namespace,
		fmt.Sprintf("SELECT count(*) FROM pg_stat_activity WHERE usename = '%s'", Role))
	Expect(err).NotTo(HaveOccurred())
	Expect(count).NotTo(Equal("0"))
}

// initScript creates the kine role + database, rewrites pg_hba.conf so GSSAPI
// is the matched rule for TCP clients, and points PostgreSQL at the keytab.
func initScript() string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
  CREATE ROLE %[1]s LOGIN;
  CREATE DATABASE %[2]s OWNER %[1]s;
EOSQL

cat > "$PGDATA/pg_hba.conf" <<-EOF
  local   all all                 trust
  host    all all 127.0.0.1/32    trust
  host    all all ::1/128         trust
  host    all all 0.0.0.0/0       gss include_realm=0 krb_realm=%[3]s
EOF

echo "krb_server_keyfile = '%[4]s/krb5.keytab'" >> "$PGDATA/postgresql.conf"
`, Role, Database, kerberos.Realm, keytabMountPath)
}

// passwordInitScript creates the kine role with a password + its database, and
// rewrites pg_hba.conf so TCP clients must authenticate via scram-sha-256 while
// local/loopback stay trusted for the superuser psql checks. PostgreSQL 17
// defaults password_encryption to scram-sha-256, so the role's password is
// stored as a scram secret and is genuinely verified on connect.
func passwordInitScript() string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
  CREATE ROLE %[1]s LOGIN PASSWORD '%[3]s';
  CREATE DATABASE %[2]s OWNER %[1]s;
EOSQL

cat > "$PGDATA/pg_hba.conf" <<-EOF
  local   all all                 trust
  host    all all 127.0.0.1/32    trust
  host    all all ::1/128         trust
  host    all all 0.0.0.0/0       scram-sha-256
EOF
`, Role, Database, Password)
}

// Deploy installs the GSSAPI init ConfigMap, the Deployment, and the Service,
// then waits for PostgreSQL to become ready. It requires the krb5.conf ConfigMap
// and the postgres keytab Secret to already exist (see the kerberos package).
func Deploy(k *kube.Client, namespace string) setup.Func {
	return setup.Named("postgres.Deploy", func(ctx context.Context) error {
		if err := k.Apply(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: initConfigMap, Namespace: namespace},
			Data:       map[string]string{"10-kerberos.sh": initScript()},
		}); err != nil {
			return err
		}
		// GSSAPI mode mounts the service keytab and the client krb5.conf.
		mounts := []corev1.VolumeMount{
			{Name: "keytab", MountPath: keytabMountPath, ReadOnly: true},
			{Name: "krb5", MountPath: "/etc/krb5.conf", SubPath: "krb5.conf", ReadOnly: true},
		}
		volumes := []corev1.Volume{
			{Name: "keytab", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: kerberos.PostgresKeytabSecret},
			}},
			{Name: "krb5", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: kerberos.Krb5ConfigMap},
				},
			}},
		}
		if err := k.Apply(ctx, deployment(namespace, initConfigMap, mounts, volumes)); err != nil {
			return err
		}
		if err := k.Apply(ctx, service(namespace)); err != nil {
			return err
		}
		return k.WaitDeploymentReady(ctx, namespace, Service, 3*time.Minute)
	})
}

// DeployPassword installs the password-mode init ConfigMap, the Deployment, and
// the Service, then waits for PostgreSQL to become ready. Unlike Deploy it needs
// no Kerberos artifacts: the pod mounts only the init script and authenticates
// TCP clients via scram-sha-256.
func DeployPassword(k *kube.Client, namespace string) setup.Func {
	return setup.Named("postgres.DeployPassword", func(ctx context.Context) error {
		if err := k.Apply(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: passwordInitConfigMap, Namespace: namespace},
			Data:       map[string]string{"10-password.sh": passwordInitScript()},
		}); err != nil {
			return err
		}
		if err := k.Apply(ctx, deployment(namespace, passwordInitConfigMap, nil, nil)); err != nil {
			return err
		}
		if err := k.Apply(ctx, service(namespace)); err != nil {
			return err
		}
		return k.WaitDeploymentReady(ctx, namespace, Service, 3*time.Minute)
	})
}

func service(namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: Service, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": Service},
			Ports:    []corev1.ServicePort{{Port: 5432, TargetPort: intstr.FromInt(5432)}},
		},
	}
}

// deployment builds the shared PostgreSQL pod skeleton (image, port 5432,
// pg_isready probe, superuser password) with the init script from initCM mounted
// at the entrypoint dir. extraMounts/extraVolumes carry the keytab + krb5.conf in
// GSSAPI mode and are nil for password mode.
func deployment(namespace, initCM string, extraMounts []corev1.VolumeMount, extraVolumes []corev1.Volume) *appsv1.Deployment {
	labels := map[string]string{"app": Service}
	mounts := append([]corev1.VolumeMount{
		{Name: "init", MountPath: "/docker-entrypoint-initdb.d"},
	}, extraMounts...)
	volumes := append([]corev1.Volume{
		{Name: "init", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: initCM},
			},
		}},
	}, extraVolumes...)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: Service, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         Service,
						Image:        Image,
						Env:          []corev1.EnvVar{{Name: "POSTGRES_PASSWORD", Value: "postgres"}},
						Ports:        []corev1.ContainerPort{{ContainerPort: 5432}},
						VolumeMounts: mounts,
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "pg_isready -U postgres"}},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       3,
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}
