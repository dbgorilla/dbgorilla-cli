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

	// Extra are operator-supplied chart values, passed through to both output
	// forms. They exist for the case the defaults cannot express: installing a
	// collector build that is not the published one, which is exactly what
	// validating an unreleased change requires.
	Extra []ExtraValue

	// MountClusterCA wires the CNPG cluster CA into the pod. Set whenever the
	// SQL connection verifies the server, which is the default.
	MountClusterCA bool
}

// clusterCAValues renders the volume and mount as `--set` arguments.
//
// Emitted from here rather than left to the operator because the volume name
// and the mount name have to match, and the mount path has to equal the one the
// provider reads. Three values that must agree, in two places, is not something
// to hand over as prose.
func (h HelmInstall) clusterCAValues() []string {
	if !h.MountClusterCA {
		return nil
	}
	return []string{
		"extraVolumes[0].name=" + ClusterCAVolumeName,
		"extraVolumes[0].secret.secretName=" + ClusterCASecretName,
		// Projecting the single key, rather than the whole Secret, keeps the
		// mount correct even if someone later copies more into that Secret.
		"extraVolumes[0].secret.items[0].key=" + ClusterCAFileName,
		"extraVolumes[0].secret.items[0].path=" + ClusterCAFileName,
		"extraVolumeMounts[0].name=" + ClusterCAVolumeName,
		"extraVolumeMounts[0].mountPath=" + ClusterCAMountPath,
		"extraVolumeMounts[0].readOnly=true",
	}
}

// ClusterCAYAML is the same wiring in values.yaml form. The two are generated
// from the same constants so they cannot drift apart.
func (h HelmInstall) ClusterCAYAML() string {
	if !h.MountClusterCA {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "extraVolumes:\n  - name: %s\n    secret:\n      secretName: %s\n      items:\n        - key: %s\n          path: %s\n",
		ClusterCAVolumeName, ClusterCASecretName, ClusterCAFileName, ClusterCAFileName)
	fmt.Fprintf(&b, "extraVolumeMounts:\n  - name: %s\n    mountPath: %s\n    readOnly: true\n",
		ClusterCAVolumeName, ClusterCAMountPath)
	return b.String()
}

// ClusterCACopyCommand renders the one command that copies the CA certificate
// into the collector's namespace.
//
// It extracts ca.crt specifically. The Secret CNPG publishes also contains the
// CA private key, and `kubectl get secret -o yaml | kubectl apply` -- the
// obvious way to copy a Secret between namespaces -- takes both. Anyone holding
// that key can mint a certificate every client in the cluster will trust, which
// is a materially larger grant than the read-only Role this whole design is
// careful about.
func ClusterCACopyCommand(cluster, dbNamespace, releaseNamespace string) string {
	return fmt.Sprintf(
		"kubectl create secret generic %s \\\n"+
			"  --namespace %s \\\n"+
			"  --from-literal=%s=\"$(kubectl get secret %s -n %s \\\n"+
			"      -o jsonpath='{.data.ca\\.crt}' | base64 -d)\"",
		ClusterCASecretName, releaseNamespace, ClusterCAFileName,
		ClusterCASourceSecret(cluster), dbNamespace)
}

// ExtraValue is one operator-supplied chart value, as a dotted key and a value.
type ExtraValue struct {
	Key   string
	Value string
}

// reservedValueKeys are the top-level chart keys this command derives itself.
// Accepting an override for one of them would put a single logical setting in
// two places in the same output, and the two would eventually disagree -- the
// reader has no way to tell which one the chart used.
var reservedValueKeys = map[string]string{
	"config":  "--set-file config.inline carries the collector.toml this command renders",
	"secrets": "--secret-name names the Secret this command emits",
	"rbac":    "the read-only grant is derived from --k8s-mode",
}

// ParseExtraValues validates `key=value` pairs before anything is minted, so a
// typo costs a re-run rather than an orphaned identity.
func ParseExtraValues(raw []string) ([]ExtraValue, error) {
	out := make([]ExtraValue, 0, len(raw))
	for _, s := range raw {
		key, value, found := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" || strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || strings.Contains(key, "..") {
			return nil, fmt.Errorf("--set %q is not key=value. Example: --set image.tag=v1.2.3", s)
		}
		top := key
		if i := strings.Index(key, "."); i >= 0 {
			top = key[:i]
		}
		if why, reserved := reservedValueKeys[top]; reserved {
			return nil, fmt.Errorf("--set %s is not allowed: %s. Setting it in two places is how the two come to disagree", key, why)
		}
		out = append(out, ExtraValue{Key: key, Value: value})
	}
	// Render the YAML and throw it away. The tree is the only thing that can see
	// a key used as both a value and a section, and that check has to run HERE:
	// the values fragment is printed after the identity is minted, so failing at
	// render time would leave a collector provisioned server-side that the
	// operator never learns about and cannot clean up.
	if _, err := extraValuesYAML(out); err != nil {
		return nil, err
	}
	return out, nil
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
	for _, v := range h.clusterCAValues() {
		fmt.Fprintf(&b, " \\\n  --set %s", v)
	}
	for _, e := range h.Extra {
		fmt.Fprintf(&b, " \\\n  --set %s=%s", e.Key, e.Value)
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
func HelmValuesFragment(renderedTOML, secretRef string, rbac RBACValues, extra []ExtraValue, caYAML string) (string, error) {
	var b strings.Builder
	b.WriteString("# values.yaml for the dbg-collector chart.\n")
	b.WriteString("# The chart renders no structure of its own: config.inline is passed through verbatim.\n")
	// Emitted first, and from the same input as the `helm install` above: the two
	// forms are alternatives, so a value present in one and missing from the
	// other would install two different collectors from one command's output.
	b.WriteString(caYAML)
	if len(extra) > 0 {
		y, err := extraValuesYAML(extra)
		if err != nil {
			return "", err
		}
		b.WriteString(y)
	}
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
	return b.String(), nil
}

// valueNode is one level of the tree the dotted --set keys describe. Insertion
// order is kept so the emitted YAML matches the order the operator typed, which
// is the order they will proofread it in.
type valueNode struct {
	order    []string
	children map[string]*valueNode
	leaf     string
	isLeaf   bool
}

func (n *valueNode) child(name string) *valueNode {
	if n.children == nil {
		n.children = map[string]*valueNode{}
	}
	if c, ok := n.children[name]; ok {
		return c
	}
	c := &valueNode{}
	n.children[name] = c
	n.order = append(n.order, name)
	return c
}

// extraValuesYAML turns dotted keys into nested YAML.
//
// It rejects a key that is used as both a value and a parent (image=x plus
// image.tag=y). Helm resolves that by silently discarding one; emitting YAML
// with the same ambiguity would hand the operator a file whose meaning depends
// on which tool reads it.
func extraValuesYAML(extra []ExtraValue) (string, error) {
	root := &valueNode{}
	for _, e := range extra {
		parts := strings.Split(e.Key, ".")
		n := root
		for i, p := range parts {
			n = n.child(p)
			last := i == len(parts)-1
			if n.isLeaf && !last {
				return "", fmt.Errorf("--set %s conflicts with an earlier --set of %s: one cannot be both a value and a section",
					e.Key, strings.Join(parts[:i+1], "."))
			}
			if last {
				if len(n.order) > 0 {
					return "", fmt.Errorf("--set %s conflicts with an earlier --set of %s.*: one cannot be both a value and a section",
						e.Key, e.Key)
				}
				n.isLeaf, n.leaf = true, e.Value
			}
		}
	}
	var b strings.Builder
	writeValueNode(&b, root, 0)
	return b.String(), nil
}

func writeValueNode(b *strings.Builder, n *valueNode, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, name := range n.order {
		c := n.children[name]
		if c.isLeaf {
			fmt.Fprintf(b, "%s%s: %s\n", indent, name, yamlScalar(c.leaf))
			continue
		}
		fmt.Fprintf(b, "%s%s:\n", indent, name)
		writeValueNode(b, c, depth+1)
	}
}

// yamlScalar quotes anything that is not unambiguously a number or a boolean.
//
// Deliberately the same inference `helm --set` performs, including its known
// trap that a version-like `1.0` is read as a number. Matching it means the two
// forms this command prints install the same thing; diverging to be helpful
// would mean the values.yaml and the helm command disagree, which is a worse
// failure than a documented shared quirk.
func yamlScalar(v string) string {
	switch v {
	case "true", "false", "null", "":
		if v == "" {
			return `""`
		}
		return v
	}
	if isNumeric(v) {
		return v
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
}

func isNumeric(v string) bool {
	s := strings.TrimPrefix(v, "-")
	if s == "" {
		return false
	}
	dots := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return s != "." && !strings.HasSuffix(s, ".")
}
