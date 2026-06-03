// Package kine deploys the kine image under test. By default it authenticates
// to PostgreSQL via Kerberos using a mounted keytab; callers that set
// Options.CCacheSecret authenticate from a mounted Kerberos credential cache
// instead, and callers that set Options.Endpoint supply a verbatim DSN (e.g.
// plain user/password) for which no Kerberos artifacts are mounted. kine only
// opens its etcd listener on :2379 after a successful database connection and
// schema setup, so a TCP readiness probe on :2379 is a precise "datastore auth
// succeeded" signal regardless of the auth method.
package kine

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/k3s-io/kine/e2e/helper/kube"
	"github.com/k3s-io/kine/e2e/helper/setup"
	"github.com/k3s-io/kine/e2e/setup/kerberos"
	"github.com/k3s-io/kine/e2e/setup/postgres"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	// Name is the default Deployment/Service name for the kine under test.
	Name = "kine"
	// DefaultPrincipal is the correct client principal whose key is in the
	// mounted keytab.
	DefaultPrincipal = "kine@" + kerberos.Realm
	keytabMountPath  = "/etc/kine-keytab"
	ccacheMountPath  = "/etc/kine-ccache"
	ccacheFile       = "krb5cc"
)

// Options configures a kine deployment.
type Options struct {
	Namespace string // target namespace (required)
	Name      string // Deployment/Service name (defaults to Name)
	Image     string // kine image under test (required)

	// Endpoint, when set, is used verbatim as the --endpoint DSN (e.g. a plain
	// user/password postgres DSN). The keytab and krb5.conf are not mounted in
	// this mode. When empty, a Kerberos GSSAPI endpoint is built from Principal
	// and the keytab Secret is mounted.
	Endpoint string

	Principal    string // client principal carried in the DSN user-part (defaults to DefaultPrincipal); Kerberos mode only
	KeytabSecret string // keytab Secret name (defaults to kerberos.KineKeytabSecret); keytab mode only
	WithService  bool   // also create a Service exposing :2379

	// CCacheSecret, when set (and Endpoint is empty), selects credential-cache
	// mode: kine authenticates from the Kerberos credential cache in the named
	// Secret (key "krb5cc") instead of a keytab. The krb5.conf ConfigMap is
	// mounted and KRB5_CONFIG is set, as in keytab mode.
	CCacheSecret string
	// CCacheViaEnv, in credential-cache mode, passes the cache location to kine
	// via the KRB5CCNAME environment variable (FILE:<path>) paired with a
	// valueless krb5_ccache DSN parameter, instead of an explicit
	// krb5_ccache=<path> parameter. It exercises kine's KRB5CCNAME resolution.
	CCacheViaEnv bool

	// AppName, when set, is sent as the PostgreSQL application_name so that
	// server-side assertions (and pg_terminate_backend) can be scoped to this
	// deployment. Multiple kine deployments share the same Kerberos role, so the
	// application_name is the only thing that distinguishes their sessions.
	AppName string
	// ConnMaxLifetime, when > 0, sets --datastore-connection-max-lifetime so the
	// pool recycles connections (each recycle re-runs the GSSAPI handshake).
	ConnMaxLifetime time.Duration
	// MaxIdleConns, when > 0, sets --datastore-max-idle-connections.
	MaxIdleConns int
}

// Endpoint returns the passwordless GSSAPI DSN kine uses to reach PostgreSQL.
// The client principal ("user@REALM") is carried in the DSN user-part with the
// "@" percent-encoded; kine splits it back out to drive the GSSAPI handshake.
// The keytab path enabling GSSAPI is carried in the krb5_keytab query parameter.
// When appName is non-empty it is forwarded as the PostgreSQL application_name
// (an unknown-to-kine param that prepareDSN passes through to pgx untouched).
func Endpoint(namespace, principal, appName string) string {
	q := url.Values{
		"sslmode":     {"disable"},
		"krbsrvname":  {"postgres"},
		"krb5_keytab": {keytabMountPath + "/krb5.keytab"},
	}
	if appName != "" {
		q.Set("application_name", appName)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.User(principal),
		Host:     fmt.Sprintf("%s:5432", postgres.ServiceFQDN(namespace)),
		Path:     "/" + postgres.Database,
		RawQuery: q.Encode(),
	}
	return u.String()
}

// CCacheEndpoint returns the GSSAPI DSN for credential-cache mode. As in
// Endpoint the client principal is carried in the user-part; kine reduces it to
// the bare PostgreSQL role and takes the client identity from the cached ticket.
// The cache is selected by an explicit krb5_ccache=<ccachePath> parameter, or
// (when viaEnv) by a valueless krb5_ccache parameter that makes kine resolve the
// cache from the KRB5CCNAME environment variable instead.
func CCacheEndpoint(namespace, principal, appName, ccachePath string, viaEnv bool) string {
	q := url.Values{
		"sslmode":    {"disable"},
		"krbsrvname": {"postgres"},
	}
	if viaEnv {
		q["krb5_ccache"] = []string{""}
	} else {
		q.Set("krb5_ccache", ccachePath)
	}
	if appName != "" {
		q.Set("application_name", appName)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.User(principal),
		Host:     fmt.Sprintf("%s:5432", postgres.ServiceFQDN(namespace)),
		Path:     "/" + postgres.Database,
		RawQuery: q.Encode(),
	}
	return u.String()
}

// Install creates the kine Deployment (and optionally a Service). It does not
// wait for readiness; specs assert readiness (or the lack of it) via
// gomega.Eventually.
func Install(k *kube.Client, opts Options) setup.Func {
	if opts.Name == "" {
		opts.Name = Name
	}
	// The Kerberos defaults only apply when no verbatim DSN was supplied. The
	// keytab default applies only in keytab mode (no CCacheSecret).
	if opts.Endpoint == "" {
		if opts.Principal == "" {
			opts.Principal = DefaultPrincipal
		}
		if opts.CCacheSecret == "" && opts.KeytabSecret == "" {
			opts.KeytabSecret = kerberos.KineKeytabSecret
		}
	}
	return setup.Named("kine.Install", func(ctx context.Context) error {
		dep := deployment(opts)
		if err := k.Apply(ctx, dep); err != nil {
			return err
		}
		if opts.WithService {
			if err := k.Apply(ctx, service(dep.Namespace, opts.Name)); err != nil {
				return err
			}
		}
		return nil
	})
}

func service(namespace, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports:    []corev1.ServicePort{{Port: 2379, TargetPort: intstr.FromInt(2379)}},
		},
	}
}

func deployment(opts Options) *appsv1.Deployment {
	labels := map[string]string{"app": opts.Name}

	endpoint := opts.Endpoint
	switch {
	case endpoint != "":
		// A verbatim DSN supplied by the caller (e.g. plain user/password).
	case opts.CCacheSecret != "":
		endpoint = CCacheEndpoint(opts.Namespace, opts.Principal, opts.AppName, ccacheMountPath+"/"+ccacheFile, opts.CCacheViaEnv)
	default:
		endpoint = Endpoint(opts.Namespace, opts.Principal, opts.AppName)
	}
	args := []string{"--endpoint=" + endpoint}
	if opts.ConnMaxLifetime > 0 {
		args = append(args, fmt.Sprintf("--datastore-connection-max-lifetime=%s", opts.ConnMaxLifetime))
	}
	if opts.MaxIdleConns > 0 {
		args = append(args, fmt.Sprintf("--datastore-max-idle-connections=%d", opts.MaxIdleConns))
	}

	container := corev1.Container{
		Name:            "kine",
		Image:           opts.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            args,
		Ports:           []corev1.ContainerPort{{ContainerPort: 2379}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(2379)},
			},
			InitialDelaySeconds: 3,
			PeriodSeconds:       3,
		},
	}
	var volumes []corev1.Volume

	// The GSSAPI modes (keytab or credential cache) mount the client krb5.conf
	// and set KRB5_CONFIG; the verbatim-DSN mode (opts.Endpoint set) needs
	// neither. The two GSSAPI modes differ only in whether the kine pod carries a
	// keytab Secret or a credential-cache Secret.
	if opts.Endpoint == "" {
		container.Env = []corev1.EnvVar{{Name: "KRB5_CONFIG", Value: "/etc/krb5.conf"}}
		krb5Mount := corev1.VolumeMount{Name: "krb5", MountPath: "/etc/krb5.conf", SubPath: "krb5.conf", ReadOnly: true}
		krb5Volume := corev1.Volume{Name: "krb5", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: kerberos.Krb5ConfigMap},
			},
		}}

		switch {
		case opts.CCacheSecret != "":
			if opts.CCacheViaEnv {
				container.Env = append(container.Env, corev1.EnvVar{
					Name:  "KRB5CCNAME",
					Value: "FILE:" + ccacheMountPath + "/" + ccacheFile,
				})
			}
			container.VolumeMounts = []corev1.VolumeMount{
				{Name: "ccache", MountPath: ccacheMountPath, ReadOnly: true},
				krb5Mount,
			}
			volumes = []corev1.Volume{
				{Name: "ccache", VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: opts.CCacheSecret},
				}},
				krb5Volume,
			}
		default:
			container.VolumeMounts = []corev1.VolumeMount{
				{Name: "keytab", MountPath: keytabMountPath, ReadOnly: true},
				krb5Mount,
			}
			volumes = []corev1.Volume{
				{Name: "keytab", VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: opts.KeytabSecret},
				}},
				krb5Volume,
			}
		}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: opts.Name, Namespace: opts.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
					Volumes:    volumes,
				},
			},
		},
	}
}
