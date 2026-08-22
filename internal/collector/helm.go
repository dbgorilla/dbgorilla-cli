package collector

import (
	"fmt"
	"strings"
)

// HelmInstall describes the `helm install` this CLI hands the operator. The CLI
// does not run Helm: the customer's platform team owns cluster access, this tool
// does not have it, and asking for it to run one command would be a materially
// larger ask than the read-only RBAC the collector itself needs.
type HelmInstall struct {
	Release    string
	Namespace  string
	ChartRef   string
	ConfigPath string
	// SecretRef names the pre-created Kubernetes Secret carrying the collector's
	// server secret and, when password auth is used, the database password.
	SecretRef string

	// RBAC is the chart's read-only grant, always emitted rather than defaulted.
	// Without it the Kubernetes API returns 403, `mode = auto` degrades to
	// metrics-only as designed, and the install looks successful while backup
	// state reads as absent rather than unavailable.
	RBAC RBACValues
}

// RBACValues mirrors the chart's rbac.* keys.
//
// Namespaces is the DATABASE namespace, not the release namespace: the grant has
// to be readable where the CNPG Cluster lives. Scope stays namespaced -- a
// RoleBinding cannot grant cluster-scoped resources, and the design deliberately
// pays that limitation to keep "this agent can see nothing outside your
// database's namespace" true in the default install.
type RBACValues struct {
	Create     bool
	Scope      string
	Namespaces []string
}

// RBACFor derives the chart grant from the Kubernetes access mode. Metrics-only
// needs no permissions at all, which is the honest thing to emit -- asking for a
// grant the collector will never use is how a read-only ask stops being believed.
func RBACFor(k8sMode, dbNamespace string) RBACValues {
	if k8sMode == K8sModeDisabled {
		return RBACValues{Create: false}
	}
	return RBACValues{Create: true, Scope: "namespaced", Namespaces: []string{dbNamespace}}
}

// DefaultHelmInstall fills in the release name/namespace and chart reference.
func DefaultHelmInstall(configPath, secretRef string, rbac RBACValues) HelmInstall {
	return HelmInstall{
		Release:    DefaultReleaseName,
		Namespace:  DefaultReleaseNamespace,
		ChartRef:   DefaultChartRef,
		ConfigPath: configPath,
		SecretRef:  secretRef,
		RBAC:       rbac,
	}
}

// Command renders the install command as a copy-pastable multi-line string.
//
// `--set-file` is used rather than `--set` deliberately: the config is multi-line
// TOML and `--set` would require escaping every newline and comma, which is the
// kind of quoting a customer gets wrong once and then cannot debug.
func (h HelmInstall) Command() string {
	var b strings.Builder
	fmt.Fprintf(&b, "helm install %s %s \\\n", h.Release, h.ChartRef)
	fmt.Fprintf(&b, "  --namespace %s --create-namespace \\\n", h.Namespace)
	fmt.Fprintf(&b, "  --set-file config.inline=%s", h.ConfigPath)
	if h.SecretRef != "" {
		fmt.Fprintf(&b, " \\\n  --set secrets.existingSecret=%s", h.SecretRef)
	}
	fmt.Fprintf(&b, " \\\n  --set rbac.create=%t", h.RBAC.Create)
	if h.RBAC.Create {
		fmt.Fprintf(&b, " \\\n  --set rbac.scope=%s", h.RBAC.Scope)
		for i, ns := range h.RBAC.Namespaces {
			fmt.Fprintf(&b, " \\\n  --set rbac.namespaces[%d]=%s", i, ns)
		}
	}
	return b.String()
}

// SecretCommand renders the `kubectl create secret` that supplies the two values
// the config references as ${ENV} placeholders.
//
// The secret is created by the operator, not by this CLI, and the values are
// shown as shell variables rather than literals so the real secret never lands
// in a shell history file or a terminal scrollback.
//
// The key names are load-bearing: the chart injects each one as an environment
// variable of the same name, and the config references it as ${NAME}. They have
// to match on both sides or the collector resolves nothing.
func (h HelmInstall) SecretCommand(withDBPassword bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kubectl create secret generic %s \\\n", h.SecretRef)
	fmt.Fprintf(&b, "  --namespace %s \\\n", h.Namespace)
	fmt.Fprintf(&b, "  --from-literal=%s=\"$DBG_SERVER_SECRET\"", SecretEnv)
	if withDBPassword {
		fmt.Fprintf(&b, " \\\n  --from-literal=%s=\"$COLLECTOR_DB_PASSWORD\"", DBPasswordEnv)
	}
	return b.String()
}

// HelmValuesFragment renders the equivalent values.yaml, for operators who
// deploy through Argo CD or Flux and cannot paste a `helm install`.
//
// config.inline is emitted as a YAML block scalar so the TOML passes through
// byte-for-byte; the chart is a passthrough and renders no structure of its own.
func HelmValuesFragment(renderedTOML, secretRef string, rbac RBACValues) string {
	var b strings.Builder
	b.WriteString("# values.yaml for the dbg-collector chart.\n")
	b.WriteString("# The chart renders no structure of its own: config.inline is passed through verbatim.\n")
	if secretRef != "" {
		// secrets.existingSecret, not a top-level key: when it is set the chart
		// creates no Secret of its own, so the pre-created one must carry the
		// server-secret key AND every database-password env var the config
		// references. That is the vault/ExternalSecret route the chart recommends
		// over putting values in this file.
		fmt.Fprintf(&b, "secrets:\n  existingSecret: %s\n", secretRef)
	}
	fmt.Fprintf(&b, "rbac:\n  create: %t\n", rbac.Create)
	if rbac.Create {
		fmt.Fprintf(&b, "  scope: %s\n  namespaces:\n", rbac.Scope)
		for _, ns := range rbac.Namespaces {
			fmt.Fprintf(&b, "    - %s\n", ns)
		}
	}
	b.WriteString("config:\n  inline: |\n")
	for _, line := range strings.Split(strings.TrimRight(renderedTOML, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(&b, "    %s\n", line)
	}
	return b.String()
}
