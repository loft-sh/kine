// Package kerberos deploys an MIT Kerberos KDC into the test cluster and, at
// runtime, creates the PostgreSQL service principal and the kine client
// principal, exporting their keytabs into Secrets. Principals depend on the
// namespace Service DNS names, so they are generated per-run rather than
// checked in.
package kerberos

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/k3s-io/kine/e2e/helper/kube"
	"github.com/k3s-io/kine/e2e/helper/setup"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	// Realm is the Kerberos realm used by the test environment.
	Realm = "KINE.TEST"
	// KDCService is the Service (and Deployment) name of the KDC.
	KDCService = "krb5kdc"
	// Image is the maintained MIT krb5 KDC image.
	Image = "gcavalcante8808/krb5-server:latest"
	// MasterPassword seeds the KDC database; value is irrelevant for the test.
	MasterPassword = "e2emasterpassword"

	// Krb5ConfigMap holds the client krb5.conf mounted into postgres and kine.
	Krb5ConfigMap = "krb5-conf"
	// PostgresKeytabSecret / KineKeytabSecret hold the exported keytabs.
	PostgresKeytabSecret = "postgres-keytab"
	KineKeytabSecret     = "kine-keytab"
	// KineCCacheSecret holds the kine principal's credential cache (a pre-minted
	// TGT) used by kine's credential-cache authentication mode.
	KineCCacheSecret = "kine-ccache"

	labelSelector = "app=" + KDCService
)

// KDCHost returns the in-cluster FQDN of the KDC Service.
func KDCHost(namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", KDCService, namespace)
}

// Krb5Conf returns a client krb5.conf for the realm. DNS canonicalization is
// disabled so the SPN equals the Service DNS name the client connects to.
func Krb5Conf(namespace string) string {
	return fmt.Sprintf(`[libdefaults]
 default_realm = %[1]s
 dns_lookup_realm = false
 dns_lookup_kdc = false
 dns_canonicalize_hostname = false
 rdns = false
 forwardable = true
 udp_preference_limit = 1
[realms]
 %[1]s = {
   kdc = %[2]s
   admin_server = %[2]s
 }
`, Realm, KDCHost(namespace))
}

// Deploy installs the krb5.conf ConfigMap and the KDC into the (already
// existing) namespace, then waits for the KDC to become ready.
func Deploy(k *kube.Client, namespace string) setup.Func {
	return setup.Named("kerberos.Deploy", func(ctx context.Context) error {
		if err := k.Apply(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: Krb5ConfigMap, Namespace: namespace},
			Data:       map[string]string{"krb5.conf": Krb5Conf(namespace)},
		}); err != nil {
			return err
		}

		if err := k.Apply(ctx, kdcDeployment(namespace)); err != nil {
			return err
		}
		if err := k.Apply(ctx, kdcService(namespace)); err != nil {
			return err
		}

		return k.WaitDeploymentReady(ctx, namespace, KDCService, 2*time.Minute)
	})
}

// BootstrapKeytabs creates the postgres service principal and the kine client
// principal in the running KDC and stores their keytabs as Secrets.
//
//	pgServiceFQDN: e.g. postgres.<ns>.svc.cluster.local
//	kineUser:      short principal name, e.g. "kine"
func BootstrapKeytabs(k *kube.Client, namespace, pgServiceFQDN, kineUser string) setup.Func {
	return setup.Named("kerberos.BootstrapKeytabs", func(ctx context.Context) error {
		pod, err := k.FirstPodName(ctx, namespace, labelSelector)
		if err != nil {
			return err
		}

		pgSPN := fmt.Sprintf("postgres/%s@%s", pgServiceFQDN, Realm)
		kinePrinc := fmt.Sprintf("%s@%s", kineUser, Realm)

		for _, princ := range []string{pgSPN, kinePrinc} {
			if _, _, err := k.ExecInPod(ctx, namespace, pod,
				[]string{"kadmin.local", "-q", "addprinc -randkey " + princ}); err != nil {
				return fmt.Errorf("addprinc %s: %w", princ, err)
			}
		}

		pgKeytab, err := ktaddAndFetch(ctx, k, namespace, pod, "/tmp/postgres.keytab", pgSPN)
		if err != nil {
			return err
		}
		kineKeytab, err := ktaddAndFetch(ctx, k, namespace, pod, "/tmp/kine.keytab", kinePrinc)
		if err != nil {
			return err
		}

		if err := k.Apply(ctx, keytabSecret(namespace, PostgresKeytabSecret, pgKeytab)); err != nil {
			return err
		}
		return k.Apply(ctx, keytabSecret(namespace, KineKeytabSecret, kineKeytab))
	})
}

// BootstrapCCache mints a TGT for the kine client principal into a Kerberos
// credential cache and stores it as a Secret, for kine's credential-cache
// authentication mode. It extracts the principal's current key without rotating
// it (ktadd -norandkey, so any keytab already exported for the principal stays
// valid) and runs kinit from that keytab to populate the cache. It requires the
// kine principal to already exist (see BootstrapKeytabs).
//
//	kineUser: short principal name, e.g. "kine"
func BootstrapCCache(k *kube.Client, namespace, kineUser string) setup.Func {
	return setup.Named("kerberos.BootstrapCCache", func(ctx context.Context) error {
		pod, err := k.FirstPodName(ctx, namespace, labelSelector)
		if err != nil {
			return err
		}

		princ := fmt.Sprintf("%s@%s", kineUser, Realm)
		const (
			ktPath = "/tmp/kine-cc.keytab"
			ccPath = "/tmp/kine.ccache"
		)

		// Extract the current key without rotating it, so the kine keytab Secret
		// exported by BootstrapKeytabs stays usable.
		if _, _, err := k.ExecInPod(ctx, namespace, pod,
			[]string{"kadmin.local", "-q", fmt.Sprintf("ktadd -norandkey -k %s %s", ktPath, princ)}); err != nil {
			return fmt.Errorf("ktadd -norandkey %s: %w", princ, err)
		}

		// Mint a TGT into the credential cache from that keytab. The TGT keeps the
		// realm-default lifetime (~24h), far longer than a test run.
		if _, _, err := k.ExecInPod(ctx, namespace, pod,
			[]string{"kinit", "-k", "-t", ktPath, "-c", "FILE:" + ccPath, princ}); err != nil {
			return fmt.Errorf("kinit %s: %w", princ, err)
		}

		ccache, err := readPodFileB64(ctx, k, namespace, pod, ccPath)
		if err != nil {
			return err
		}
		return k.Apply(ctx, ccacheSecret(namespace, KineCCacheSecret, ccache))
	})
}

// SetMaxTicketLife caps the maximum ticket lifetime of the given principals via
// kadmin.local modprinc, so freshly minted tickets expire in d rather than the
// realm default (~24h). The expiry specs call this for the krbtgt principal (which
// bounds every TGT) and the kine principal to make ticket expiry observable
// within a test run. d is rounded to whole seconds.
func SetMaxTicketLife(k *kube.Client, namespace string, d time.Duration, principals ...string) setup.Func {
	return setup.Named("kerberos.SetMaxTicketLife", func(ctx context.Context) error {
		pod, err := k.FirstPodName(ctx, namespace, labelSelector)
		if err != nil {
			return err
		}
		life := fmt.Sprintf("%d seconds", int(d.Seconds()))
		for _, princ := range principals {
			if _, _, err := k.ExecInPod(ctx, namespace, pod,
				[]string{"kadmin.local", "-q", fmt.Sprintf("modprinc -maxlife %q %s", life, princ)}); err != nil {
				return fmt.Errorf("modprinc -maxlife %s: %w", princ, err)
			}
		}
		return nil
	})
}

// RotatePrincipalKey changes principal's key to a new random value
// (kadmin.local cpw -randkey), invalidating any keytab exported before the
// rotation. The expiry specs use it to simulate a key rotation that the mounted
// keytab has not yet caught up with, so new GSSAPI logins fail.
func RotatePrincipalKey(k *kube.Client, namespace, principal string) setup.Func {
	return setup.Named("kerberos.RotatePrincipalKey", func(ctx context.Context) error {
		pod, err := k.FirstPodName(ctx, namespace, labelSelector)
		if err != nil {
			return err
		}
		if _, _, err := k.ExecInPod(ctx, namespace, pod,
			[]string{"kadmin.local", "-q", "cpw -randkey " + principal}); err != nil {
			return fmt.Errorf("cpw -randkey %s: %w", principal, err)
		}
		return nil
	})
}

// RefreshKeytab re-exports principal's current key and overwrites the named
// keytab Secret with it. A consumer that reloads its keytab per connection (kine
// does) then recovers from a key rotation without a restart, once the kubelet
// propagates the updated Secret to the mounted volume.
func RefreshKeytab(k *kube.Client, namespace, principal, secretName string) setup.Func {
	return setup.Named("kerberos.RefreshKeytab", func(ctx context.Context) error {
		pod, err := k.FirstPodName(ctx, namespace, labelSelector)
		if err != nil {
			return err
		}
		keytab, err := ktaddAndFetch(ctx, k, namespace, pod, "/tmp/refresh.keytab", principal)
		if err != nil {
			return err
		}
		return k.UpdateSecret(ctx, namespace, secretName, map[string][]byte{"krb5.keytab": keytab})
	})
}

// ktaddAndFetch exports principal into ktPath inside the KDC pod and returns
// the keytab bytes.
func ktaddAndFetch(ctx context.Context, k *kube.Client, namespace, pod, ktPath, principal string) ([]byte, error) {
	if _, _, err := k.ExecInPod(ctx, namespace, pod,
		[]string{"kadmin.local", "-q", fmt.Sprintf("ktadd -k %s %s", ktPath, principal)}); err != nil {
		return nil, fmt.Errorf("ktadd %s: %w", principal, err)
	}
	return readPodFileB64(ctx, k, namespace, pod, ktPath)
}

// readPodFileB64 reads a binary file from a pod by base64-encoding it over exec
// (exec stdout is text, so base64 keeps keytabs and credential caches intact)
// and decoding the result.
func readPodFileB64(ctx context.Context, k *kube.Client, namespace, pod, path string) ([]byte, error) {
	stdout, _, err := k.ExecInPod(ctx, namespace, pod, []string{"base64", path})
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", path, err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(stdout), ""))
	if err != nil {
		return nil, fmt.Errorf("decode file %s: %w", path, err)
	}
	return data, nil
}

func keytabSecret(namespace, name string, keytab []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{"krb5.keytab": keytab},
	}
}

// ccacheSecret holds a Kerberos credential cache under the "krb5cc" key, which
// kine's credential-cache mode mounts and reads.
func ccacheSecret(namespace, name string, ccache []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{"krb5cc": ccache},
	}
}

func kdcService(namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: KDCService, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": KDCService},
			Ports: []corev1.ServicePort{
				{Name: "kdc-tcp", Port: 88, TargetPort: intstr.FromInt(88), Protocol: corev1.ProtocolTCP},
				{Name: "kdc-udp", Port: 88, TargetPort: intstr.FromInt(88), Protocol: corev1.ProtocolUDP},
				{Name: "kadmin", Port: 749, TargetPort: intstr.FromInt(749), Protocol: corev1.ProtocolTCP},
				{Name: "kpasswd", Port: 464, TargetPort: intstr.FromInt(464), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func kdcDeployment(namespace string) *appsv1.Deployment {
	labels := map[string]string{"app": KDCService}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: KDCService, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "kdc",
						Image: Image,
						Env: []corev1.EnvVar{
							{Name: "KRB5_REALM", Value: Realm},
							{Name: "KRB5_KDC", Value: KDCHost(namespace)},
							{Name: "KRB5_PASS", Value: MasterPassword},
						},
						Ports: []corev1.ContainerPort{
							{ContainerPort: 88, Protocol: corev1.ProtocolTCP},
							{ContainerPort: 88, Protocol: corev1.ProtocolUDP},
							{ContainerPort: 749, Protocol: corev1.ProtocolTCP},
							{ContainerPort: 464, Protocol: corev1.ProtocolTCP},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(88)},
							},
							InitialDelaySeconds: 3,
							PeriodSeconds:       2,
						},
					}},
				},
			},
		},
	}
}
