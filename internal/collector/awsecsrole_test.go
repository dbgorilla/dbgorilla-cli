package collector

import (
	"net/http"
	"strings"
	"testing"
)

const getECSRoleXML = `<GetRoleResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">
  <GetRoleResult><Role>
    <RoleName>AWSServiceRoleForECS</RoleName>
    <Path>/aws-service-role/ecs.amazonaws.com/</Path>
    <Arn>arn:aws:iam::111122223333:role/aws-service-role/ecs.amazonaws.com/AWSServiceRoleForECS</Arn>
  </Role></GetRoleResult>
</GetRoleResponse>`

const createECSRoleXML = `<CreateServiceLinkedRoleResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">
  <CreateServiceLinkedRoleResult><Role>
    <RoleName>AWSServiceRoleForECS</RoleName>
  </Role></CreateServiceLinkedRoleResult>
</CreateServiceLinkedRoleResponse>`

func noSettle(t *testing.T) {
	t.Helper()
	orig := ecsRoleSettle
	ecsRoleSettle = 0
	t.Cleanup(func() { ecsRoleSettle = orig })
}

func TestEnsureECSServiceLinkedRole(t *testing.T) {
	noSettle(t)
	noRole := awsErrorXML("NoSuchEntity", "The role with name AWSServiceRoleForECS cannot be found.")

	t.Run("an existing role is left alone", func(t *testing.T) {
		f := newAWSFake(t).on("GetRole", getECSRoleXML)
		stubAWS(t, f)
		got, err := EnsureECSServiceLinkedRole(false)
		if err != nil || got != ECSRolePresent {
			t.Fatalf("got %v, %v; want present", got, err)
		}
		if f.called("CreateServiceLinkedRole") != 0 {
			t.Error("must not create a role that exists")
		}
	})

	t.Run("a missing role is created for the ECS service", func(t *testing.T) {
		f := newAWSFake(t).on("GetRole", noRole).on("CreateServiceLinkedRole", createECSRoleXML)
		stubAWS(t, f)
		got, err := EnsureECSServiceLinkedRole(false)
		if err != nil || got != ECSRoleCreated {
			t.Fatalf("got %v, %v; want created", got, err)
		}
		if !strings.Contains(f.sentBody(), "AWSServiceName=ecs.amazonaws.com") {
			t.Errorf("create must name the ECS service principal; sent:\n%s", f.sentBody())
		}
	})

	t.Run("a dry run reports the missing role and creates nothing", func(t *testing.T) {
		f := newAWSFake(t).on("GetRole", noRole)
		stubAWS(t, f)
		got, err := EnsureECSServiceLinkedRole(true)
		if err != nil || got != ECSRoleMissing {
			t.Fatalf("got %v, %v; want missing with no error", got, err)
		}
		if f.called("CreateServiceLinkedRole") != 0 {
			t.Error("a dry run must not create the role")
		}
	})

	// ECS creates the role itself on first use, so it can appear between the
	// lookup and the create. That is success, not an error.
	t.Run("a role created concurrently counts as present", func(t *testing.T) {
		f := newAWSFake(t).on("GetRole", noRole).
			on("CreateServiceLinkedRole", awsErrorXML("InvalidInput",
				"Service role name AWSServiceRoleForECS has been taken in this account, please try a different suffix."))
		stubAWS(t, f)
		got, err := EnsureECSServiceLinkedRole(false)
		if err != nil || got != ECSRolePresent {
			t.Fatalf("got %v, %v; want present", got, err)
		}
	})

	t.Run("a denied create stops with the command an administrator runs", func(t *testing.T) {
		f := newAWSFake(t).on("GetRole", noRole).
			fail("CreateServiceLinkedRole", http.StatusForbidden, awsErrorXML("AccessDenied", "not authorized to perform: iam:CreateServiceLinkedRole"))
		stubAWS(t, f)
		got, err := EnsureECSServiceLinkedRole(false)
		if got != ECSRoleMissing || err == nil {
			t.Fatalf("got %v, %v; want missing with an error", got, err)
		}
		if !strings.Contains(err.Error(), ECSRoleCreateCommand) {
			t.Errorf("error should give the fix, got: %v", err)
		}
	})

	// Without iam:GetRole the CLI can't tell; the role most likely exists, so
	// this is reported as unknown (the caller warns) and nothing is created.
	t.Run("a denied lookup is unknown and creates nothing", func(t *testing.T) {
		f := newAWSFake(t).fail("GetRole", http.StatusForbidden, awsErrorXML("AccessDenied", "not authorized to perform: iam:GetRole"))
		stubAWS(t, f)
		got, err := EnsureECSServiceLinkedRole(false)
		if got != ECSRoleUnknown || err == nil {
			t.Fatalf("got %v, %v; want unknown with an error", got, err)
		}
		if f.called("CreateServiceLinkedRole") != 0 {
			t.Error("must not attempt a create after a failed lookup")
		}
	})
}
