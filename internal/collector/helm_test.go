package collector

import (
	"strings"
	"testing"
)

func TestDefaultHelmInstall_Defaults(t *testing.T) {
	h := DefaultHelmInstall("/tmp/collector.toml", "dbg-secrets", RBACFor(K8sModeAuto, "prod-db"))
	if h.Release != DefaultReleaseName || h.Namespace != DefaultReleaseNamespace {
		t.Errorf("unexpected release coordinates: %+v", h)
	}
	if !strings.HasPrefix(h.ChartRef, "oci://") {
		t.Errorf("chart ref should be an OCI reference, got %q", h.ChartRef)
	}
}

// --set-file, not --set: the config is multi-line TOML, and --set would make the
// customer escape every newline and comma by hand.
func TestHelmInstall_CommandUsesSetFile(t *testing.T) {
	cmd := DefaultHelmInstall("/tmp/collector.toml", "dbg-secrets", RBACFor(K8sModeAuto, "prod-db")).Command()
	if !strings.Contains(cmd, "--set-file config.inline=/tmp/collector.toml") {
		t.Errorf("expected --set-file for the config, got:\n%s", cmd)
	}
	if strings.Contains(cmd, "--set config.inline") {
		t.Errorf("--set would require escaping multi-line TOML:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--create-namespace") {
		t.Errorf("expected --create-namespace, got:\n%s", cmd)
	}
}

func TestHelmInstall_CommandOmitsSecretRefWhenUnset(t *testing.T) {
	h := DefaultHelmInstall("/tmp/c.toml", "", RBACFor(K8sModeAuto, "prod-db"))
	if strings.Contains(h.Command(), "secrets.existingSecret") {
		t.Errorf("no secret name means no existingSecret flag:\n%s", h.Command())
	}
}

// The secret command must reference shell variables, never literals: it is
// printed to a terminal and would otherwise land in shell history.
func TestHelmInstall_SecretCommandUsesShellVars(t *testing.T) {
	h := DefaultHelmInstall("/tmp/c.toml", "dbg-secrets", RBACFor(K8sModeAuto, "prod-db"))
	got := h.SecretCommand(true)
	for _, want := range []string{
		"kubectl create secret generic dbg-secrets",
		"--from-literal=" + SecretEnv + "=\"$DBG_SERVER_SECRET\"",
		"--from-literal=" + DBPasswordEnv,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("secret command missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "--namespace "+DefaultReleaseNamespace) {
		t.Errorf("secret must be created in the release namespace:\n%s", got)
	}
}

func TestHelmInstall_SecretCommandOmitsDBPasswordWhenNotNeeded(t *testing.T) {
	got := DefaultHelmInstall("/tmp/c.toml", "s", RBACFor(K8sModeAuto, "prod-db")).SecretCommand(false)
	if strings.Contains(got, DBPasswordEnv) {
		t.Errorf("db password should be omitted when unused:\n%s", got)
	}
}

// The chart passes config.inline through verbatim, so the TOML has to survive
// the YAML block scalar byte-for-byte -- indentation added, nothing else.
func TestHelmValuesFragment_IndentsTOMLVerbatim(t *testing.T) {
	toml := "[dbgorilla]\nagent_id = \"a\"\n\n[topology]\ninterval = \"60s\"\n"
	got := mustFragment(t, toml, "dbg-secrets", RBACFor(K8sModeAuto, "prod-db"))
	if !strings.Contains(got, "secrets:\n  existingSecret: dbg-secrets") {
		t.Errorf("secret ref missing:\n%s", got)
	}
	if !strings.Contains(got, "config:\n  inline: |\n") {
		t.Errorf("expected a block scalar for config.inline:\n%s", got)
	}
	for _, line := range []string{"    [dbgorilla]", "    agent_id = \"a\"", "    [topology]"} {
		if !strings.Contains(got, line) {
			t.Errorf("expected %q indented into the block:\n%s", line, got)
		}
	}
	// A blank TOML line must stay blank, not become four spaces -- trailing
	// whitespace is what YAML linters reject in a chart values file.
	if strings.Contains(got, "    \n") {
		t.Errorf("blank lines should not be indented into whitespace:\n%q", got)
	}
}

func TestHelmValuesFragment_OmitsSecretKeyWhenUnset(t *testing.T) {
	got := mustFragment(t, "[dbgorilla]\n", "", RBACFor(K8sModeAuto, "prod-db"))
	if strings.Contains(got, "existingSecret") {
		t.Errorf("no secret name means no key:\n%s", got)
	}
}

// End to end on the pure path: a rendered CNPG config must round-trip through
// the values fragment and still parse as the TOML the collector will load.
func TestHelmValuesFragment_RoundTripsARealConfig(t *testing.T) {
	rendered, err := BuildCNPG("a", "t", validCNPGTarget(), Endpoints{}).Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	frag := mustFragment(t, rendered, "s", RBACFor(K8sModeAuto, "prod-db"))

	// Take ONLY the config.inline block scalar. Extracting every four-space line
	// would also swallow the rbac.namespaces entries, which is how this test
	// started failing on a change that was correct.
	_, after, found := strings.Cut(frag, "config:\n  inline: |\n")
	if !found {
		t.Fatalf("no config.inline block in fragment:\n%s", frag)
	}
	var body []string
	for _, line := range strings.Split(after, "\n") {
		switch {
		case line == "":
			body = append(body, "")
		case strings.HasPrefix(line, "    "):
			body = append(body, strings.TrimPrefix(line, "    "))
		default:
			t.Fatalf("line escaped the block scalar: %q", line)
		}
	}
	if _, err := ParseConfig(strings.Join(body, "\n")); err != nil {
		t.Fatalf("config did not survive the values fragment: %v", err)
	}
}

// The grant is emitted rather than left to the chart's defaults, because a
// missing grant is silent: 403 at discovery, `auto` degrades to metrics-only by
// design, and metrics-only then answers backup questions falsely.
func TestRBACFor_GrantsTheDatabaseNamespaceNotTheReleaseNamespace(t *testing.T) {
	got := RBACFor(K8sModeAuto, "prod-db")
	if !got.Create || got.Scope != "namespaced" {
		t.Fatalf("auto should create a namespaced grant, got %+v", got)
	}
	if len(got.Namespaces) != 1 || got.Namespaces[0] != "prod-db" {
		t.Errorf("grant must target the DATABASE namespace, got %v", got.Namespaces)
	}
	if got.Namespaces[0] == DefaultReleaseNamespace {
		t.Error("granting the release namespace would read nothing: the Cluster is elsewhere")
	}
}

// Metrics-only needs no permissions at all. Asking for a grant the collector
// will never use is how a read-only ask stops being believed.
func TestRBACFor_MetricsOnlyAsksForNothing(t *testing.T) {
	got := RBACFor(K8sModeDisabled, "prod-db")
	if got.Create {
		t.Errorf("metrics-only must not request a grant, got %+v", got)
	}
	cmd := DefaultHelmInstall("/tmp/c.toml", "s", got).Command()
	if !strings.Contains(cmd, "--set rbac.create=false") {
		t.Errorf("install must disable rbac explicitly:\n%s", cmd)
	}
	if strings.Contains(cmd, "rbac.scope") || strings.Contains(cmd, "rbac.namespaces") {
		t.Errorf("no scope or namespaces when nothing is granted:\n%s", cmd)
	}
}

func TestHelmInstall_CommandCarriesTheGrant(t *testing.T) {
	cmd := DefaultHelmInstall("/tmp/c.toml", "s", RBACFor(K8sModeEnabled, "prod-db")).Command()
	for _, want := range []string{
		"--set rbac.create=true", "--set rbac.scope=namespaced", "--set rbac.namespaces[0]=prod-db",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("install command missing %q:\n%s", want, cmd)
		}
	}
}

func TestHelmValuesFragment_CarriesTheGrant(t *testing.T) {
	got := mustFragment(t, "[dbgorilla]\n", "s", RBACFor(K8sModeAuto, "prod-db"))
	for _, want := range []string{"rbac:", "  create: true", "  scope: namespaced", "    - prod-db"} {
		if !strings.Contains(got, want) {
			t.Errorf("values fragment missing %q:\n%s", want, got)
		}
	}
}

// The chart nests these under `secrets.*`. A top-level `existingSecret` is
// silently ignored by Helm -- an unknown value is not an error -- so the chart
// would fall through to creating its own empty Secret and the collector would
// start with no credentials. Asserting the exact path is the only thing that
// catches that, because nothing downstream errors.
func TestHelmInstall_SecretKeyIsNestedUnderSecrets(t *testing.T) {
	cmd := DefaultHelmInstall("/tmp/c.toml", "dbg-secrets", RBACFor(K8sModeAuto, "prod-db")).Command()
	if !strings.Contains(cmd, "--set secrets.existingSecret=dbg-secrets") {
		t.Errorf("expected the nested chart key, got:\n%s", cmd)
	}
	if strings.Contains(cmd, "--set existingSecret=") {
		t.Errorf("top-level existingSecret is not a chart value and would be ignored:\n%s", cmd)
	}
}

func TestHelmValuesFragment_SecretKeyIsNestedUnderSecrets(t *testing.T) {
	got := mustFragment(t, "[dbgorilla]\n", "dbg-secrets", RBACFor(K8sModeAuto, "prod-db"))
	if !strings.Contains(got, "secrets:\n  existingSecret: dbg-secrets") {
		t.Errorf("expected secrets.existingSecret, got:\n%s", got)
	}
}

// The Secret's keys are the env var names the config references. If the two ever
// drift the collector resolves nothing -- and the failure is a startup error, not
// a config error, so it is diagnosed far from its cause.
func TestSecretCommand_KeysMatchTheConfigPlaceholders(t *testing.T) {
	rendered, err := BuildCNPG("a", "t", validCNPGTarget(), Endpoints{}).Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := DefaultHelmInstall("/tmp/c.toml", "s", RBACFor(K8sModeAuto, "prod-db")).SecretCommand(true)
	for _, env := range []string{SecretEnv, DBPasswordEnv} {
		if !strings.Contains(rendered, "${"+env+"}") {
			t.Errorf("config does not reference ${%s}", env)
		}
		if !strings.Contains(got, "--from-literal="+env+"=") {
			t.Errorf("secret command does not create key %s:\n%s", env, got)
		}
	}
}

// mustFragment renders the values.yaml for tests that are not about the extra
// chart values, and fails rather than returning an error the caller would have
// to thread through every assertion.
func mustFragment(t *testing.T, toml, secretRef string, rbac RBACValues, extra ...ExtraValue) string {
	t.Helper()
	got, err := HelmValuesFragment(toml, secretRef, rbac, extra)
	if err != nil {
		t.Fatalf("HelmValuesFragment: %v", err)
	}
	return got
}

// --- operator-supplied chart values ----------------------------------------

// The published chart and image are the defaults, and validating an unreleased
// collector means installing one that is not published yet. Without a way to
// say so, the documented install path cannot install the thing under test.
func TestHelmInstall_ExtraValuesReachTheCommand(t *testing.T) {
	h := DefaultHelmInstall("/tmp/c.toml", "sec", RBACFor(K8sModeAuto, "prod-db"))
	h.ChartRef = "./dbg-collector"
	h.Extra = []ExtraValue{{Key: "image.tag", Value: "pr-102"}}
	cmd := h.Command()
	if !strings.Contains(cmd, "helm install dbg-collector ./dbg-collector") {
		t.Errorf("chart reference not used:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--set image.tag=pr-102") {
		t.Errorf("extra value missing from the command:\n%s", cmd)
	}
}

// The command prints two forms of the same install. A value that reaches one
// and not the other means following the second installs a different collector
// than following the first -- from a single command's output.
func TestHelmValuesFragment_CarriesTheSameExtrasAsTheCommand(t *testing.T) {
	extra := []ExtraValue{
		{Key: "image.repository", Value: "reg.example/dbg-collector"},
		{Key: "image.tag", Value: "pr-102"},
		{Key: "replicaCount", Value: "2"},
	}
	got := mustFragment(t, "[dbgorilla]\n", "s", RBACFor(K8sModeAuto, "prod-db"), extra...)
	for _, want := range []string{
		"image:\n",
		"  repository: \"reg.example/dbg-collector\"\n",
		"  tag: \"pr-102\"\n",
		"replicaCount: 2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("values.yaml missing %q\n---\n%s", want, got)
		}
	}
	// Shared keys nest once, rather than repeating the parent -- a duplicate
	// mapping key makes the file ambiguous, and YAML parsers disagree on which
	// one wins.
	if strings.Count(got, "image:") != 1 {
		t.Errorf("image key emitted more than once\n---\n%s", got)
	}
}

// The command derives these three itself. Accepting an override would put one
// setting in two places in the same output, and nothing tells the reader which
// one the chart used.
func TestParseExtraValues_RejectsTheKeysThisCommandOwns(t *testing.T) {
	for _, key := range []string{"config.inline", "config", "secrets.existingSecret", "rbac.create", "rbac"} {
		if _, err := ParseExtraValues([]string{key + "=x"}); err == nil {
			t.Errorf("--set %s should be rejected: this command derives it", key)
		}
	}
	// Anything else is the operator's business.
	if _, err := ParseExtraValues([]string{"image.tag=v1", "resources.limits.cpu=500m"}); err != nil {
		t.Errorf("ordinary chart values should be accepted: %v", err)
	}
}

func TestParseExtraValues_RejectsMalformedInput(t *testing.T) {
	for _, bad := range []string{"noequals", "=novalue", ".leading=x", "trailing.=x", "a..b=x"} {
		if _, err := ParseExtraValues([]string{bad}); err == nil {
			t.Errorf("--set %q should be rejected", bad)
		}
	}
	// An empty value is a real thing to want -- it clears a chart default.
	if _, err := ParseExtraValues([]string{"nodeSelector="}); err != nil {
		t.Errorf("an empty value should be allowed: %v", err)
	}
}

// Helm resolves this by silently dropping one of the two. Emitting YAML with
// the same ambiguity hands over a file whose meaning depends on which parser
// reads it, which is worse than refusing.
func TestHelmValuesFragment_RejectsAKeyUsedAsBothValueAndSection(t *testing.T) {
	for _, pair := range [][]ExtraValue{
		{{Key: "image", Value: "x"}, {Key: "image.tag", Value: "y"}},
		{{Key: "image.tag", Value: "y"}, {Key: "image.tag.sub", Value: "z"}},
	} {
		if _, err := HelmValuesFragment("[dbgorilla]\n", "s", RBACFor(K8sModeAuto, "prod-db"), pair); err == nil {
			t.Errorf("conflicting keys %v should be rejected", pair)
		}
	}
}

// Quoting follows `helm --set`, including its trap of reading a version-like
// value as a number. Matching it keeps the two printed forms equivalent;
// diverging to be more helpful would make them install different things.
func TestYamlScalar_MatchesHelmSetInference(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.3":  `"v1.2.3"`,
		"pr-102":  `"pr-102"`,
		"2":       "2",
		"1.0":     "1.0",
		"-1":      "-1",
		"true":    "true",
		"false":   "false",
		"":        `""`,
		"500m":    `"500m"`,
		`say"hi"`: `"say\"hi\""`,
	} {
		if got := yamlScalar(in); got != want {
			t.Errorf("yamlScalar(%q) = %s, want %s", in, got, want)
		}
	}
}

// The conflict check has to run at parse time, not at render time. The values
// fragment is printed after the identity is minted, so a late failure would
// leave a collector provisioned server-side that the operator never sees and
// cannot remove.
func TestParseExtraValues_RejectsConflictsBeforeAnythingIsMinted(t *testing.T) {
	if _, err := ParseExtraValues([]string{"image=x", "image.tag=y"}); err == nil {
		t.Fatal("a value-and-section conflict must be caught while parsing flags")
	}
}
