package collector

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// UpdateComponents changes which databases a running collector monitors. It
// re-renders the config stored on the stack, so a mistake here silently drops
// settings from a collector that is currently running in a customer's account.

const callerIdentityXML = `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <GetCallerIdentityResult>
    <Arn>arn:aws:iam::111122223333:user/dev</Arn>
    <Account>111122223333</Account>
    <UserId>AIDATEST</UserId>
  </GetCallerIdentityResult>
</GetCallerIdentityResponse>`

// storedConfig builds the base64 config blob an installed stack carries.
func storedConfig(t *testing.T, targets []AwsTarget) string {
	t.Helper()
	rendered, err := awsConfigTOML("agent-1", "tenant-1", "us-east-1", targets, Endpoints{}, true)
	if err != nil {
		t.Fatalf("awsConfigTOML: %v", err)
	}
	encoded, err := EncodeConfig(rendered)
	if err != nil {
		t.Fatalf("EncodeConfig: %v", err)
	}
	return encoded
}

func updateTarget(name, instance string) AwsTarget {
	return AwsTarget{
		Name: name, InstanceID: instance, DbiResourceID: "db-RES-" + instance,
		Host: instance + ".rds.amazonaws.com", Port: 5432, User: "readonly",
		Databases: []string{"appdb"}, SSLMode: "require", ProviderType: "aws_rds",
		AuthMethod: "iam",
	}
}

func TestUpdateComponents_ReplacesTheMonitoredSet(t *testing.T) {
	f := newAWSFake(t).
		onSeq("DescribeStacks",
			stacksXML("CREATE_COMPLETE", stackParamXML(configParamKey, storedConfig(t, []AwsTarget{updateTarget("old", "db-old")}))),
			stacksXML("CREATE_COMPLETE", declaredParamsXML()...), // the declared-key read
			stacksXML("UPDATE_COMPLETE"), // the update waiter
		).
		on("GetCallerIdentity", callerIdentityXML).
		on("UpdateStack", updateStackXML())
	stubAWS(t, f)

	err := UpdateComponents("dbg-collector", "us-east-1",
		[]AwsTarget{updateTarget("new", "db-new")}, "", false)
	if err != nil {
		t.Fatalf("UpdateComponents: %v", err)
	}

	body := f.sentBody()
	// The identity minted at install must be preserved, not re-minted: every
	// parameter except the two component-bearing ones rides UsePreviousValue.
	if !strings.Contains(body, "UsePreviousValue=true") {
		t.Error("unrelated parameters must keep their previous values")
	}
	if !strings.Contains(body, "UsePreviousTemplate=true") {
		t.Error("a component update must not swap the stack's template")
	}
}

// Without a password on the update, the stored one must be kept — overwriting
// it with an empty string leaves the collector unable to authenticate.
func TestUpdateComponents_KeepsStoredPasswordWhenNoneGiven(t *testing.T) {
	f := newAWSFake(t).
		onSeq("DescribeStacks",
			stacksXML("CREATE_COMPLETE", stackParamXML(configParamKey, storedConfig(t, []AwsTarget{updateTarget("old", "db-old")}))),
			stacksXML("CREATE_COMPLETE", declaredParamsXML()...), // the declared-key read
			stacksXML("UPDATE_COMPLETE"), // the update waiter
		).
		on("GetCallerIdentity", callerIdentityXML).
		on("UpdateStack", updateStackXML())
	stubAWS(t, f)

	if err := UpdateComponents("dbg-collector", "us-east-1",
		[]AwsTarget{updateTarget("new", "db-new")}, "", false); err != nil {
		t.Fatalf("UpdateComponents: %v", err)
	}
	// DbPassword must be sent as "use previous", never as an empty value.
	if strings.Contains(f.sentBody(), "ParameterKey=DbPassword&ParameterValue=&") {
		t.Error("an empty password must not overwrite the stored one")
	}
}

// A rotated password, or a newly added password-auth database, has to reach the
// stack or the collector is left with an unresolved reference.
func TestUpdateComponents_CarriesANewPassword(t *testing.T) {
	f := newAWSFake(t).
		onSeq("DescribeStacks",
			stacksXML("CREATE_COMPLETE", stackParamXML(configParamKey, storedConfig(t, []AwsTarget{updateTarget("old", "db-old")}))),
			stacksXML("CREATE_COMPLETE", declaredParamsXML()...), // the declared-key read
			stacksXML("UPDATE_COMPLETE"), // the update waiter
		).
		on("GetCallerIdentity", callerIdentityXML).
		on("UpdateStack", updateStackXML())
	stubAWS(t, f)

	if err := UpdateComponents("dbg-collector", "us-east-1",
		[]AwsTarget{updateTarget("new", "db-new")}, "rotated-secret", false); err != nil {
		t.Fatalf("UpdateComponents: %v", err)
	}
	if !strings.Contains(f.sentBody(), "rotated-secret") {
		t.Error("a supplied password must reach the DbPassword parameter")
	}
}

func TestUpdateComponents_NoChangesIsSuccess(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeStacks", stacksXML("CREATE_COMPLETE",
			stackParamXML(configParamKey, storedConfig(t, []AwsTarget{updateTarget("same", "db-same")})))).
		on("GetCallerIdentity", callerIdentityXML).
		fail("UpdateStack", http.StatusBadRequest,
			awsErrorXML("ValidationError", "No updates are to be performed.")))

	if err := UpdateComponents("dbg-collector", "us-east-1",
		[]AwsTarget{updateTarget("same", "db-same")}, "", false); err != nil {
		t.Fatalf("a no-op update must succeed, got %v", err)
	}
}

// The stored config is re-rendered on every update. A key this build cannot
// model would be silently deleted, so it has to stop instead.
func TestUpdateComponents_RefusesConfigItCannotModel(t *testing.T) {
	unknown, err := EncodeConfig("[unknown_section]\nsome_key = \"value\"\n")
	if err != nil {
		t.Fatal(err)
	}
	stubAWS(t, newAWSFake(t).
		on("DescribeStacks", stacksXML("CREATE_COMPLETE", stackParamXML(configParamKey, unknown))))

	err = UpdateComponents("dbg-collector", "us-east-1", []AwsTarget{updateTarget("new", "db-new")}, "", false)
	if err == nil {
		t.Fatal("an unmodellable config must stop the update, not be silently rewritten")
	}
	if !strings.Contains(err.Error(), "dbg-collector") {
		t.Errorf("error should name the stack, got: %v", err)
	}
}

func TestUpdateComponents_UndecodableConfig(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeStacks", stacksXML("CREATE_COMPLETE", stackParamXML(configParamKey, "!!!not-base64!!!"))))

	if err := UpdateComponents("dbg-collector", "us-east-1", nil, "", false); err == nil {
		t.Fatal("expected a decode failure")
	}
}

func TestUpdateComponents_MissingStackParameter(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeStacks", stacksXML("CREATE_COMPLETE", stackParamXML("Unrelated", "x"))))

	err := UpdateComponents("dbg-collector", "us-east-1", nil, "", false)
	if err == nil || !strings.Contains(err.Error(), "predates this CLI version") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateComponents_AccountLookupFailure(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeStacks", stacksXML("CREATE_COMPLETE",
			stackParamXML(configParamKey, storedConfig(t, []AwsTarget{updateTarget("old", "db-old")})))).
		fail("GetCallerIdentity", http.StatusForbidden, awsErrorXML("AccessDenied", "denied")))

	if err := UpdateComponents("dbg-collector", "us-east-1",
		[]AwsTarget{updateTarget("new", "db-new")}, "", false); err == nil {
		t.Fatal("the grant list needs the account id; a failure must stop the update")
	}
}

func TestUpdateComponents_CredentialFailure(t *testing.T) {
	stubAWSConfigError(t, errors.New("no credentials"))
	if err := UpdateComponents("dbg-collector", "", nil, "", false); err == nil {
		t.Fatal("expected the credential error")
	}
}

// sentConfig decodes the collector config an UpdateStack call carried.
func sentConfig(t *testing.T, f *awsFake) Config {
	t.Helper()
	for _, body := range f.bodies {
		q, err := url.ParseQuery(body)
		if err != nil || q.Get("Action") != "UpdateStack" {
			continue
		}
		for i := 1; q.Has("Parameters.member." + itoa(i) + ".ParameterKey"); i++ {
			if q.Get("Parameters.member."+itoa(i)+".ParameterKey") != configParamKey {
				continue
			}
			toml, err := DecodeConfig(q.Get("Parameters.member." + itoa(i) + ".ParameterValue"))
			if err != nil {
				t.Fatalf("DecodeConfig: %v", err)
			}
			conf, err := StrictParseConfig(toml)
			if err != nil {
				t.Fatalf("StrictParseConfig: %v", err)
			}
			return conf
		}
	}
	t.Fatal("no UpdateStack call carried the config parameter")
	return Config{}
}

// An update with no command flag must not turn commands on or off: each
// database keeps what it runs with now, and a database new to the collector
// gets none, even though the gate is on for the others.
func TestUpdateComponents_KeepCommandsPreservesEachDatabase(t *testing.T) {
	kept := updateTarget("kept", "db-kept")
	kept.Commands = []string{CmdExplain}
	off := updateTarget("off", "db-off")
	f := newAWSFake(t).
		onSeq("DescribeStacks",
			stacksXML("CREATE_COMPLETE", stackParamXML(configParamKey, storedConfig(t, []AwsTarget{kept, off}))),
			stacksXML("CREATE_COMPLETE", declaredParamsXML()...),
			stacksXML("UPDATE_COMPLETE"),
		).
		on("GetCallerIdentity", callerIdentityXML).
		on("UpdateStack", updateStackXML())
	stubAWS(t, f)

	targets := []AwsTarget{updateTarget("kept", "db-kept"), updateTarget("off", "db-off"), updateTarget("new", "db-new")}
	if err := UpdateComponents("dbg-collector", "us-east-1", targets, "", true); err != nil {
		t.Fatalf("UpdateComponents: %v", err)
	}
	conf := sentConfig(t, f)
	got := map[string][]string{}
	for _, c := range conf.Component {
		got[c.Name] = c.Commands
	}
	if !slices.Equal(got["kept"], []string{CmdExplain}) {
		t.Errorf("kept: want [explain], got %v", got["kept"])
	}
	if len(got["off"]) != 0 || len(got["new"]) != 0 {
		t.Errorf("off and new must have no commands, got off=%v new=%v", got["off"], got["new"])
	}
	// With the gate on, a database with no list inherits [commands].allowed;
	// it must be present and empty, or "off" and "new" would get everything.
	if !conf.Commands.Enabled || conf.Commands.Allowed == nil || len(*conf.Commands.Allowed) != 0 {
		t.Errorf("want enabled with allowed = [], got %+v", conf.Commands)
	}
}
