package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectSolo(t *testing.T) {
	rds := func(id string) TargetChoice { return TargetChoice{ID: id, ProviderType: "aws_rds"} }
	aurora := func(id string) TargetChoice { return TargetChoice{ID: id, ProviderType: "aws_aurora"} }
	tests := []struct {
		name             string
		choices          []TargetChoice
		wantID, wantKind string
		wantErr          bool
	}{
		{"one instance", []TargetChoice{rds("prod-pg")}, "prod-pg", "aws_rds", false},
		{"one cluster", []TargetChoice{aurora("prod-aurora")}, "prod-aurora", "aws_aurora", false},
		{"none", nil, "", "", true},
		{"two instances", []TargetChoice{rds("a"), rds("b")}, "", "", true},
		{"instance + cluster is ambiguous", []TargetChoice{rds("a"), aurora("b")}, "", "", true},
	}
	none := errors.New("none found; pass --db-instance-id")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectSolo(tt.choices, none)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got.ID != tt.wantID || got.ProviderType != tt.wantKind {
				t.Errorf("got (%q, %q), want (%q, %q)", got.ID, got.ProviderType, tt.wantID, tt.wantKind)
			}
		})
	}
	// The zero case is the caller's own error, verbatim: it names what was
	// searched, which the shared selector cannot know.
	if _, err := selectSolo(nil, none); !errors.Is(err, none) {
		t.Errorf("no candidates should return the caller's error, got %v", err)
	}
}

func TestGrantStatements(t *testing.T) {
	got := GrantStatements("dbgorilla", []string{"app", "billing"})
	want := []string{
		`CREATE USER "dbgorilla" WITH LOGIN;`,
		`GRANT rds_iam TO "dbgorilla";`,
		`GRANT pg_monitor TO "dbgorilla";`,
		`GRANT CONNECT ON DATABASE "app" TO "dbgorilla";`,
		`GRANT CONNECT ON DATABASE "billing" TO "dbgorilla";`,
		`GRANT pg_read_all_data TO "dbgorilla";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GrantStatements:\n got  %q\n want %q", got, want)
	}
	// No databases -> auth + stats grants only, no CONNECT lines.
	if n := len(GrantStatements("u", nil)); n != 4 {
		t.Errorf("no-databases grant should be 3 statements, got %d", n)
	}
	// Identifiers are quoted (defends against odd names).
	if s := GrantStatements(`we"ird`, nil)[0]; !strings.Contains(s, `"we""ird"`) {
		t.Errorf("identifier not escaped: %s", s)
	}
}

func TestSelectSolo_AmbiguousCarriesCandidates(t *testing.T) {
	want := []TargetChoice{
		{ID: "pg-a", ProviderType: "aws_rds"},
		{ID: "pg-b", ProviderType: "aws_rds"},
		{ID: "aur-c", ProviderType: "aws_aurora"}, // Aurora after RDS
	}
	_, err := selectSolo(want, errors.New("none"))
	var amb *AmbiguousTargetError
	if !errors.As(err, &amb) {
		t.Fatalf("ambiguous selection should return *AmbiguousTargetError, got %T", err)
	}
	if got := amb.Candidates(); !reflect.DeepEqual(got, want) {
		t.Errorf("Candidates()\n got  %+v\n want %+v", got, want)
	}
	// The zero case stays a plain error, not the ambiguous type.
	if _, e := selectSolo(nil, errors.New("none")); errors.As(e, &amb) {
		t.Error("no-databases case should not be an AmbiguousTargetError")
	}
}

// sampleCluster mirrors a described Aurora cluster (post-SDK mapping).
func sampleCluster() *rdsCluster {
	return &rdsCluster{
		ID:             "prod-aurora",
		Engine:         "aurora-postgresql",
		Host:           "prod-aurora.cluster-abc.us-east-2.rds.amazonaws.com",
		Port:           5432,
		ResourceID:     "cluster-XYZ789",
		DatabaseName:   "app",
		IAMAuthEnabled: true,
		SubnetGroup:    "default-vpc-123",
		SecurityGroups: []sgMembership{{ID: "sg-aurora", Status: "active"}},
	}
}

func TestMergeCluster_Discovery(t *testing.T) {
	got := mergeCluster(AwsTarget{}, sampleCluster(), []string{"subnet-a", "subnet-b"})
	want := AwsTarget{
		Name:             "prod-aurora",
		InstanceID:       "prod-aurora",
		DbiResourceID:    "cluster-XYZ789",
		Host:             "prod-aurora.cluster-abc.us-east-2.rds.amazonaws.com",
		Port:             5432,
		User:             DefaultDBUser,
		Databases:        []string{"app"},
		SSLMode:          "verify-full",
		ProviderType:     "aws_aurora", // forced, even from a blank target
		Subnets:          []string{"subnet-a", "subnet-b"},
		SecurityGroup:    "sg-aurora",
		DBSecurityGroups: []string{"sg-aurora"},
		IAMAuthOn:        true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeCluster (discovery):\n got  %+v\n want %+v", got, want)
	}
}

// sampleInstance mirrors a described RDS instance (post-SDK mapping), including a
// non-active security group that discovery must skip.
func sampleInstance() *rdsInstance {
	return &rdsInstance{
		ID:             "prod-pg",
		Engine:         "postgres",
		DbiResourceID:  "db-ABC123",
		DBName:         "app",
		IAMAuthEnabled: true,
		Host:           "prod-pg.abc.us-east-2.rds.amazonaws.com",
		Port:           5432,
		Subnets:        []string{"subnet-1", "subnet-2"},
		SecurityGroups: []sgMembership{{ID: "sg-inactive", Status: "adding"}, {ID: "sg-active", Status: "active"}},
	}
}

func TestMergeInstance_Discovery(t *testing.T) {
	got := mergeInstance(AwsTarget{}, sampleInstance())
	want := AwsTarget{
		Name:             "prod-pg",
		InstanceID:       "prod-pg",
		DbiResourceID:    "db-ABC123",
		Host:             "prod-pg.abc.us-east-2.rds.amazonaws.com",
		Port:             5432,
		User:             DefaultDBUser,
		Databases:        []string{"app"},
		SSLMode:          "verify-full",
		ProviderType:     "aws_rds",
		Subnets:          []string{"subnet-1", "subnet-2"},
		SecurityGroup:    "sg-active",           // skips the non-active group
		DBSecurityGroups: []string{"sg-active"}, // active groups only
		IAMAuthOn:        true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeInstance (discovery):\n got  %+v\n want %+v", got, want)
	}
}

func TestMergeInstance_ExplicitWins(t *testing.T) {
	into := AwsTarget{
		Name:          "my-name",
		User:          "my_user",
		Host:          "explicit.host",
		Port:          6543,
		Databases:     []string{"custom"},
		Subnets:       []string{"subnet-x"},
		SecurityGroup: "sg-explicit",
		SSLMode:       "require",
		ProviderType:  "aws_aurora",
	}
	got := mergeInstance(into, sampleInstance())

	// Every explicitly-set field must survive discovery.
	if got.Name != "my-name" || got.User != "my_user" || got.Host != "explicit.host" ||
		got.Port != 6543 || got.SSLMode != "require" || got.ProviderType != "aws_aurora" ||
		got.SecurityGroup != "sg-explicit" ||
		!reflect.DeepEqual(got.Databases, []string{"custom"}) ||
		!reflect.DeepEqual(got.Subnets, []string{"subnet-x"}) {
		t.Errorf("explicit fields were overwritten by discovery: %+v", got)
	}
	// A field left unset is still filled from the instance.
	if got.InstanceID != "prod-pg" || got.DbiResourceID != "db-ABC123" {
		t.Errorf("unset fields not filled from instance: %+v", got)
	}
}

func TestAwsTargetComplete(t *testing.T) {
	full := AwsTarget{
		InstanceID: "i", DbiResourceID: "r", Host: "h",
		Databases: []string{"d"}, Subnets: []string{"s"}, SecurityGroup: "sg",
	}
	if !full.Complete() {
		t.Fatal("fully-specified target should be Complete")
	}
	for _, drop := range []func(*AwsTarget){
		func(a *AwsTarget) { a.DbiResourceID = "" },
		func(a *AwsTarget) { a.Host = "" },
		func(a *AwsTarget) { a.Subnets = nil },
		func(a *AwsTarget) { a.SecurityGroup = "" },
	} {
		partial := full
		drop(&partial)
		if partial.Complete() {
			t.Errorf("target missing a required field should not be Complete: %+v", partial)
		}
	}
}

func TestCfnParamsSorted(t *testing.T) {
	got := cfnParams(map[string]string{"B": "2", "A": "1", "C": "3"})
	var pairs []string
	for _, prm := range got {
		pairs = append(pairs, *prm.ParameterKey+"="+*prm.ParameterValue)
	}
	want := []string{"A=1", "B=2", "C=3"}
	if !reflect.DeepEqual(pairs, want) {
		t.Errorf("cfnParams got %v, want %v", pairs, want)
	}
}

func TestStateIsAWS(t *testing.T) {
	cases := map[string]bool{"aws": true, "": false, "docker": false}
	for target, want := range cases {
		if got := (&State{Target: target}).IsAWS(); got != want {
			t.Errorf("IsAWS(target=%q) = %v, want %v", target, got, want)
		}
	}
}

var multiTargets = []AwsTarget{
	{
		Name: "prod pg", InstanceID: "prod-pg", DbiResourceID: "db-ABC",
		Host: "prod-pg.abc.us-east-2.rds.amazonaws.com", Port: 5432,
		User: "dbgorilla", Databases: []string{"app"}, SSLMode: "verify-full",
		ProviderType: "aws_rds",
	},
	{
		Name: "analytics", InstanceID: "analytics-aurora", DbiResourceID: "cluster-XYZ",
		Host: "analytics.cluster-x.us-east-2.rds.amazonaws.com", Port: 6543,
		User: "readonly", Databases: []string{"warehouse", "events"}, SSLMode: "require",
		ProviderType: "aws_aurora",
	},
}

// awsConfigFor renders the config for targets and parses it back, so the
// assertions below are about the collector's actual config tree rather than
// about generated text.
func awsConfigFor(t *testing.T, targets []AwsTarget) Config {
	t.Helper()
	rendered, err := awsConfigTOML("agent-1", "tenant-1", "us-east-2", targets, Endpoints{}, true)
	if err != nil {
		t.Fatalf("awsConfigTOML: %v", err)
	}
	got, err := ParseConfig(rendered)
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", rendered, err)
	}
	return got
}

func TestAwsConfigTOML_RdsVsAurora(t *testing.T) {
	got := awsConfigFor(t, multiTargets)
	if len(got.Component) != 2 {
		t.Fatalf("want 2 components, got %d", len(got.Component))
	}

	// aws_rds addresses the instance; aws_aurora addresses the cluster.
	rds := got.Component[0]
	if rds.Provider.InstanceID != "prod-pg" || rds.Provider.ClusterID != "" {
		t.Errorf("aws_rds should set instance_id only: %+v", rds.Provider)
	}
	if rds.Provider.Region != "us-east-2" {
		t.Errorf("provider region should be the stack's region, got %q", rds.Provider.Region)
	}
	if rds.Name != "prod pg" || rds.Engine != "postgres" {
		t.Errorf("unexpected component identity: %+v", rds)
	}
	if rds.Connect.Port != 5432 || rds.Connect.SSLMode != "verify-full" {
		t.Errorf("unexpected connect block: %+v", rds.Connect)
	}

	aurora := got.Component[1]
	if aurora.Provider.ClusterID != "analytics-aurora" || aurora.Provider.InstanceID != "" {
		t.Errorf("aws_aurora should set cluster_id only: %+v", aurora.Provider)
	}
	if !reflect.DeepEqual(aurora.Connect.Databases, []string{"warehouse", "events"}) {
		t.Errorf("both databases should carry over: %v", aurora.Connect.Databases)
	}
}

func TestAwsConfigTOML_IdentityAndSecretsAreReferences(t *testing.T) {
	rendered, err := awsConfigTOML("agent-1", "tenant-1", "us-east-2", multiTargets, Endpoints{}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseConfig(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dbgorilla.AgentID != "agent-1" || got.Dbgorilla.TenantID != "tenant-1" {
		t.Errorf("identity did not carry over: %+v", got.Dbgorilla)
	}
	// The secret is a ${VAR} reference the task resolves from Secrets Manager;
	// a literal here would put the OpAMP secret in a stack parameter.
	if got.Dbgorilla.Secret != "${"+SecretEnv+"}" {
		t.Errorf("secret must stay a reference, got %q", got.Dbgorilla.Secret)
	}
	if got.Commands.Enabled {
		t.Error("commands should be off when not enabled")
	}
}

func TestPasswordAuth_ReferencesSecretAndSkipsIAM(t *testing.T) {
	pw := AwsTarget{
		Name: "legacy", InstanceID: "legacy-pg", DbiResourceID: "db-PW",
		Host: "legacy.rds.amazonaws.com", Port: 5432, User: "readonly",
		Databases: []string{"app"}, SSLMode: "require",
		ProviderType: "aws_rds", AuthMethod: "password",
	}
	got := awsConfigFor(t, []AwsTarget{pw})
	auth := got.Component[0].Auth
	if auth.Method != "password" {
		t.Errorf("want password auth, got %q", auth.Method)
	}
	// The password is a reference, never the literal.
	if auth.Password != "${"+CloudDBPasswordEnv+"}" {

		t.Errorf("password must be a ${VAR} reference, got %q", auth.Password)
	}

	// A password-auth target gets no rds-db:connect grant.
	if got := rdsConnectARNs([]AwsTarget{pw}, "us-east-2", "111122223333"); len(got) != 0 {
		t.Errorf("password auth should produce no rds-db:connect, got %v", got)
	}
	// Mixed: the IAM target still gets its grant; the password one doesn't.
	mixed := rdsConnectARNs([]AwsTarget{multiTargets[0], pw}, "us-east-2", "111122223333")
	if len(mixed) != 1 {
		t.Errorf("mixed IAM+password should yield exactly 1 grant, got %v", mixed)
	}
}

func TestNoCACertIsPinned(t *testing.T) {
	// The collector image trusts the Amazon RDS roots system-wide (0.3.2+), so
	// neither auth method needs a ca_cert of its own — and pinning one would
	// replace the trust store rather than add to it, breaking any endpoint whose
	// certificate chains elsewhere (an RDS Proxy presents a publicly-rooted one).
	// A private-CA database is the operator's to configure explicitly.
	pw := AwsTarget{
		Name: "legacy", InstanceID: "legacy-pg", Host: "legacy.rds.amazonaws.com",
		Port: 5432, User: "readonly", ProviderType: "aws_rds", AuthMethod: "password",
	}
	iam := pw
	iam.AuthMethod = ""
	for _, tc := range []struct {
		name   string
		target AwsTarget
	}{{"password", pw}, {"iam", iam}} {
		if got := awsComponent(tc.target, "us-east-2").Connect.CACert; got != "" {
			t.Errorf("%s: want no ca_cert, got %q", tc.name, got)
		}
	}
}

func TestIamAuthOmitsPassword(t *testing.T) {
	got := awsConfigFor(t, multiTargets[:1])
	if pw := got.Component[0].Auth.Password; pw != "" {
		t.Errorf("IAM auth must not emit a password field, got %q", pw)
	}
}

func TestRdsConnectARNs_Dedup(t *testing.T) {
	got := rdsConnectARNs(multiTargets, "us-east-2", "111122223333")
	want := []string{
		"arn:aws:rds-db:us-east-2:111122223333:dbuser:db-ABC/dbgorilla",
		"arn:aws:rds-db:us-east-2:111122223333:dbuser:cluster-XYZ/readonly",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("want %v, got %v", want, got)
	}

	// Same instance + same user across two component rows -> one grant.
	same := []AwsTarget{
		{DbiResourceID: "db-ABC", User: "dbgorilla"},
		{DbiResourceID: "db-ABC", User: "dbgorilla"},
	}
	if got := rdsConnectARNs(same, "us-east-2", "111122223333"); len(got) != 1 {
		t.Errorf("identical grants should dedup to 1, got %v", got)
	}
}

func TestEncodeConfigRoundTrips(t *testing.T) {
	rendered, err := awsConfigTOML("agent-1", "tenant-1", "us-east-2", multiTargets, Endpoints{}, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeConfig(rendered)
	if err != nil {
		t.Fatal(err)
	}
	// The template's AllowedPattern rejects anything else, so the encoding must
	// stay single-line standard base64.
	if strings.ContainsAny(encoded, "\n\r ") {
		t.Error("encoded config must be a single line with no whitespace")
	}
	back, err := DecodeConfig(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if back != rendered {
		t.Errorf("round trip changed the config:\n%s\n---\n%s", rendered, back)
	}
}

func TestCompactConfig(t *testing.T) {
	in := `# a leading comment
[dbgorilla]

agent_id = "a"   # a trailing comment, which is part of the line
  # an indented comment
tenant_id = "#not-a-comment"
`
	got := CompactConfig(in)
	if strings.Contains(got, "a leading comment") || strings.Contains(got, "an indented comment") {
		t.Errorf("whole-line comments should be dropped:\n%s", got)
	}
	// Trailing comments are left alone — cutting them would mean parsing values.
	if !strings.Contains(got, "# a trailing comment") {
		t.Errorf("trailing comments should be preserved:\n%s", got)
	}
	// A '#' inside a value is not a comment.
	if !strings.Contains(got, `tenant_id = "#not-a-comment"`) {
		t.Errorf("a '#' inside a value must survive:\n%s", got)
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("blank lines should be dropped:\n%s", got)
	}
	// Stripping must not change what the config means.
	before, err := ParseConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("compacting changed the parsed config:\n%+v\n%+v", before, after)
	}
}

func TestCompactConfigLeavesMultilineStringsAlone(t *testing.T) {
	// Inside a multi-line string a leading '#' is content, not a comment, so
	// the whole config is returned untouched rather than risk corrupting it.
	in := "[dbgorilla]\nagent_id = \"\"\"\n# not a comment\n\"\"\"\n"
	if got := CompactConfig(in); got != in {
		t.Errorf("a config with multi-line strings should be unchanged:\n%s", got)
	}
}

func TestEncodeConfigRejectsOversizeConfig(t *testing.T) {
	// CloudFormation caps a parameter at 4096 bytes; say so plainly rather than
	// letting CreateStack fail with a generic validation error.
	_, err := EncodeConfig(strings.Repeat("x", maxConfigParamBytes))
	if err == nil {
		t.Fatal("an oversize config should be rejected")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should explain the limit, got: %v", err)
	}
}

func TestAwsStackParams_CarriesDatabasesAsParameters(t *testing.T) {
	params, secrets, err := AwsStackParams(AwsStackInput{
		AgentID: "agent-1", TenantID: "tenant-1", Image: "img@sha256:abc",
		Region: "us-east-2", AccountID: "111122223333", Targets: multiTargets,
		Subnets: []string{"subnet-1", "subnet-2"}, SecurityGroup: "sg-1",
		AssignPublicIP: "ENABLED", CommandsEnabled: true, ServerSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every template parameter the CLI is responsible for must be present —
	// credentials in the secrets map, everything else in the printable one,
	// never both.
	for _, k := range fargateParamKeys {
		_, inParams := params[k]
		_, inSecrets := secrets[k]
		if inParams == inSecrets {
			t.Errorf("stack parameter %q: in params=%v, in secrets=%v — want exactly one", k, inParams, inSecrets)
		}
	}
	if secrets["ServerSecret"] != "s3cret" {
		t.Error("the server secret must ride the secrets map")
	}
	if params["Subnets"] != "subnet-1,subnet-2" {
		t.Errorf("subnets should be comma-joined, got %q", params["Subnets"])
	}
	if n := strings.Count(params[rdsConnectParamKey], ","); n != 1 {
		t.Errorf("want 2 comma-joined grants, got %q", params[rdsConnectParamKey])
	}
	// The databases live in the config parameter, not in the template body.
	decoded, err := DecodeConfig(params[configParamKey])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decoded, "prod-pg") || !strings.Contains(decoded, "analytics-aurora") {
		t.Errorf("config parameter should carry both databases:\n%s", decoded)
	}
}

func TestAwsStackParams_AllPasswordAuthStillSendsAGrantList(t *testing.T) {
	// CloudFormation rejects an empty CommaDelimitedList, and falling back to
	// the template's wildcard default would silently widen the task role.
	pw := AwsTarget{
		Name: "legacy", InstanceID: "legacy-pg", DbiResourceID: "db-PW",
		Host: "legacy.rds.amazonaws.com", Port: 5432, User: "readonly",
		Databases: []string{"app"}, ProviderType: "aws_rds", AuthMethod: "password",
	}
	params, _, err := AwsStackParams(AwsStackInput{
		AgentID: "a", TenantID: "t", Image: "img", Region: "us-east-2",
		AccountID: "111122223333", Targets: []AwsTarget{pw},
		Subnets: []string{"subnet-1"}, SecurityGroup: "sg-1", DBPassword: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := params[rdsConnectParamKey]
	if got == "" {
		t.Fatal("grant list must not be empty")
	}
	if strings.Contains(got, "*") {
		t.Errorf("must not fall back to a wildcard grant, got %q", got)
	}
}

func TestResolveTemplate_ExplicitOverrideDoesNotFallBack(t *testing.T) {
	// An unreachable override is an error: the caller asked for a specific
	// template (a staging build, say), so silently deploying the embedded one
	// would deploy something they did not ask for.
	_, err := resolveTemplate(context.Background(), "https://127.0.0.1:1/nope.yaml")
	if err == nil {
		t.Fatal("an unreachable explicit template should error")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("error should say the template is unreachable, got: %v", err)
	}
}

func TestLoadAwsConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "dbs.toml")
	os.WriteFile(good, []byte(`
[[database]]
instance-id = "prod-pg"
name = "Production"

[[database]]
instance-id = "analytics-aurora"
provider-type = "aws_aurora"
databases = ["warehouse", "events"]
`), 0644)
	cfg, err := LoadAwsConfig(good)
	if err != nil {
		t.Fatalf("LoadAwsConfig: %v", err)
	}
	if len(cfg.Databases) != 2 {
		t.Fatalf("want 2 databases, got %d", len(cfg.Databases))
	}
	if seed := cfg.Databases[1].Seed(); seed.ProviderType != "aws_aurora" ||
		!reflect.DeepEqual(seed.Databases, []string{"warehouse", "events"}) {
		t.Errorf("Seed did not carry explicit fields: %+v", seed)
	}

	// Missing instance-id is rejected.
	bad := filepath.Join(dir, "bad.toml")
	os.WriteFile(bad, []byte("[[database]]\nname = \"no id\"\n"), 0644)
	if _, err := LoadAwsConfig(bad); err == nil {
		t.Error("entry without instance-id should error")
	}

	// No entries is rejected.
	empty := filepath.Join(dir, "empty.toml")
	os.WriteFile(empty, []byte("# nothing\n"), 0644)
	if _, err := LoadAwsConfig(empty); err == nil {
		t.Error("empty config should error")
	}
}

// TestTemplateIsStatic guards the contract between the CLI and the template it
// publishes: the template must declare exactly the parameters the CLI sets, and
// must carry no injection markers. A static template is what makes publishing
// (and a console quick-create) possible. The file is read from disk rather than
// from the binary — it is deliberately not embedded, so that the published copy
// is the only one that exists.
func TestTemplateIsStatic(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("cloudformation", "collector-fargate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fargateTemplate := string(raw)

	for _, marker := range []string{"__COMPONENTS__", "__RDS_CONNECT__"} {
		if strings.Contains(fargateTemplate, marker) {
			t.Errorf("template still carries the %s injection marker", marker)
		}
	}
	for _, k := range fargateParamKeys {
		if !strings.Contains(fargateTemplate, "\n  "+k+":") {
			t.Errorf("template does not declare the %q parameter the CLI sets", k)
		}
	}
	// The config parameter must not be a plaintext secret carrier.
	if strings.Contains(fargateTemplate, "DBG__COMPONENT__") {
		t.Error("template still configures the collector through DBG__* env vars")
	}
}
