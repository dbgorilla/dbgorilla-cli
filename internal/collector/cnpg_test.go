package collector

import (
	"errors"
	"strings"
	"testing"
)

func validCNPGTarget() CNPGTarget {
	return CNPGTarget{
		Namespace:   "prod-db",
		Cluster:     "app-db",
		User:        "dbg_readonly",
		K8sMode:     K8sModeAuto,
		MetricsPort: DefaultMetricsPort,
	}
}

// --- identity -------------------------------------------------------------

// The whole point of the key: a failover must not change it. CNPG promotes a
// standby automatically with nothing wrong, and an instance in the key would
// re-key the component and detach every row of its history.
func TestStableKey_ExcludesInstance(t *testing.T) {
	got := validCNPGTarget().StableKey()
	if got != "cnpg:prod-db/app-db" {
		t.Fatalf("stable key = %q, want cnpg:prod-db/app-db", got)
	}
	for _, banned := range []string{"-1", "-2", "app-db-1", "10.", "pod"} {
		if strings.Contains(got, banned) {
			t.Errorf("stable key %q contains instance-shaped %q", got, banned)
		}
	}
}

// SQL dials the role service, never a pod: CNPG's one shared server certificate
// covers only the three role service names, so a pod address cannot pass the
// verify-full check this CLI defaults to.
func TestRoleServiceHost_IsNamespaceQualifiedRW(t *testing.T) {
	if got := validCNPGTarget().RoleServiceHost(); got != "app-db-rw.prod-db" {
		t.Fatalf("role service host = %q, want app-db-rw.prod-db", got)
	}
}

// --- validation -----------------------------------------------------------

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*CNPGTarget)
		want   string
	}{
		"ok":             {func(*CNPGTarget) {}, ""},
		"no namespace":   {func(c *CNPGTarget) { c.Namespace = " " }, "--namespace is required"},
		"no cluster":     {func(c *CNPGTarget) { c.Cluster = "" }, "--cluster is required"},
		"bad k8s mode":   {func(c *CNPGTarget) { c.K8sMode = "sometimes" }, "unknown --k8s-mode"},
		"port zero":      {func(c *CNPGTarget) { c.MetricsPort = 0 }, "not a valid port"},
		"port too big":   {func(c *CNPGTarget) { c.MetricsPort = 70000 }, "not a valid port"},
		"tls without ca": {func(c *CNPGTarget) { c.MetricsTLS = true }, "needs --metrics-ca"},
		"no db user":     {func(c *CNPGTarget) { c.User = "" }, "--db-user is required"},
		"tls with ca":    {func(c *CNPGTarget) { c.MetricsTLS = true; c.MetricsCA = "/ca.crt" }, ""},
		"mode disabled":  {func(c *CNPGTarget) { c.K8sMode = K8sModeDisabled }, ""},
		"mode enabled":   {func(c *CNPGTarget) { c.K8sMode = K8sModeEnabled }, ""},
	} {
		tgt := validCNPGTarget()
		tc.mutate(&tgt)
		err := tgt.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.want != "" && err == nil:
			t.Errorf("%s: expected an error containing %q, got nil", name, tc.want)
		case tc.want != "" && !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: error %q does not contain %q", name, err, tc.want)
		}
	}
}

// --cluster is required rather than defaulted, and the error has to say why --
// a bare "required" invites someone to add namespace-wide discovery as a
// one-liner, which the identity model cannot support.
func TestValidate_ClusterErrorExplainsItself(t *testing.T) {
	tgt := validCNPGTarget()
	tgt.Cluster = ""
	err := tgt.Validate()
	if !errors.Is(err, ErrClusterRequired) {
		t.Fatalf("want ErrClusterRequired, got %v", err)
	}
	if !strings.Contains(err.Error(), "before discovery runs") {
		t.Errorf("cluster error should explain the identity constraint, got %q", err)
	}
}

// --- rendered config ------------------------------------------------------

func TestBuildCNPG_ShapeAndSecrets(t *testing.T) {
	cfg := BuildCNPG("agent-1", "tenant-1", validCNPGTarget(), Endpoints{})
	if len(cfg.Component) != 1 {
		t.Fatalf("want exactly one component, got %d", len(cfg.Component))
	}
	c := cfg.Component[0]
	if c.Provider.Type != CNPGProviderType {
		t.Errorf("provider type = %q, want %q", c.Provider.Type, CNPGProviderType)
	}
	if c.Provider.Namespace != "prod-db" || c.Provider.Cluster != "app-db" {
		t.Errorf("provider identity = %q/%q, want prod-db/app-db", c.Provider.Namespace, c.Provider.Cluster)
	}
	if c.Provider.Kubernetes == nil || c.Provider.Kubernetes.Mode != K8sModeAuto {
		t.Errorf("kubernetes mode not carried: %+v", c.Provider.Kubernetes)
	}
	if c.Provider.Metrics == nil || c.Provider.Metrics.Port != DefaultMetricsPort {
		t.Errorf("metrics port not carried: %+v", c.Provider.Metrics)
	}
	if c.Connect.Host != "app-db-rw.prod-db" || c.Connect.Port != 5432 {
		t.Errorf("connect = %s:%d, want app-db-rw.prod-db:5432", c.Connect.Host, c.Connect.Port)
	}
	if c.Name != "app-db" {
		t.Errorf("component name should default to the cluster, got %q", c.Name)
	}
	// The rendered TOML is printed to a terminal and pasted into chart values, so
	// no credential may ever appear as a literal.
	rendered, err := cfg.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rendered, "${"+SecretEnv+"}") {
		t.Error("server secret is not an ${ENV} reference")
	}
	if !strings.Contains(rendered, "${"+DBPasswordEnv+"}") {
		t.Error("db password is not an ${ENV} reference")
	}
}

// Plain HTTP is the CNPG default on 1.29/1.30, so no TLS keys should be emitted
// unless the operator says the Cluster enables them.
func TestBuildCNPG_NoTLSKeysByDefault(t *testing.T) {
	cfg := BuildCNPG("a", "t", validCNPGTarget(), Endpoints{})
	m := cfg.Component[0].Provider.Metrics
	if m.Scheme != "" || m.CACert != "" || m.ServerName != "" {
		t.Errorf("expected no TLS keys by default, got %+v", m)
	}
}

// With TLS the scrape dials a pod IP but must verify the role-service name,
// because that is what CNPG's shared certificate actually covers.
func TestBuildCNPG_TLSVerifiesRoleServiceName(t *testing.T) {
	tgt := validCNPGTarget()
	tgt.MetricsTLS, tgt.MetricsCA = true, "/etc/ca/ca.crt"
	m := BuildCNPG("a", "t", tgt, Endpoints{}).Component[0].Provider.Metrics
	if m.Scheme != "https" {
		t.Errorf("scheme = %q, want https", m.Scheme)
	}
	if m.ServerName != "app-db-rw" {
		t.Errorf("server_name = %q, want app-db-rw (the certificate covers role services only)", m.ServerName)
	}
	if m.CACert != "/etc/ca/ca.crt" {
		t.Errorf("ca_cert = %q, want the supplied bundle", m.CACert)
	}
}

// A config this CLI renders must survive the strict round-trip the AWS update
// path uses -- otherwise a later `dbg` build silently drops the cnpg keys.
func TestBuildCNPG_SurvivesStrictParse(t *testing.T) {
	tgt := validCNPGTarget()
	tgt.MetricsTLS, tgt.MetricsCA = true, "/ca.crt"
	rendered, err := BuildCNPG("a", "t", tgt, Endpoints{}).Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	back, err := StrictParseConfig(rendered)
	if err != nil {
		t.Fatalf("strict parse rejected our own output: %v", err)
	}
	if back.Component[0].Provider.Metrics.ServerName != "app-db-rw" {
		t.Error("server_name did not survive the round trip")
	}
}

func TestMetricsOnly(t *testing.T) {
	// Only `disabled` is knowable here. `auto` degrades at runtime depending on
	// whether the grant exists, which this CLI cannot see.
	for mode, want := range map[string]bool{
		K8sModeDisabled: true,
		K8sModeAuto:     false,
		K8sModeEnabled:  false,
	} {
		tgt := validCNPGTarget()
		tgt.K8sMode = mode
		if got := tgt.MetricsOnly(); got != want {
			t.Errorf("MetricsOnly(%s) = %v, want %v", mode, got, want)
		}
	}
}

// The warning names capabilities, not settings -- "you will not get backup
// state" is actionable where "kubernetes.mode is disabled" is not.
func TestDegradedCapabilities_NamesBackupAndWAL(t *testing.T) {
	joined := strings.ToLower(strings.Join(DegradedCapabilities(), " "))
	for _, want := range []string{"backup", "wal", "failover"} {
		if !strings.Contains(joined, want) {
			t.Errorf("degraded capabilities should mention %q, got %q", want, joined)
		}
	}
}
