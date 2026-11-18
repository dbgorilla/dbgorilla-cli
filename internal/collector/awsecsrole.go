package collector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// The ECS service-linked role.
//
// ECS needs the AWSServiceRoleForECS role in the account before it can create
// a cluster. ECS creates that role itself the first time it is used, but not
// reliably in time for the CreateCluster call that triggered it: in an account
// that has never used ECS, the stack's Cluster fails with "Unable to assume the
// service linked role", the rest of the stack is cancelled, and a plain re-run
// succeeds. Creating the role up front, before anything is minted or deployed,
// removes that first-install failure.

const (
	// ECSServiceLinkedRoleName is the fixed name AWS gives ECS's service-linked role.
	ECSServiceLinkedRoleName = "AWSServiceRoleForECS"
	ecsServicePrincipal      = "ecs.amazonaws.com"
)

// ECSRoleCreateCommand is what an administrator runs when the installing
// identity may not create the role itself.
const ECSRoleCreateCommand = "aws iam create-service-linked-role --aws-service-name " + ecsServicePrincipal

// ECSRoleStatus is the outcome of EnsureECSServiceLinkedRole.
type ECSRoleStatus int

const (
	// ECSRolePresent: the role already exists; nothing was changed.
	ECSRolePresent ECSRoleStatus = iota
	// ECSRoleCreated: the role was missing and this call created it.
	ECSRoleCreated
	// ECSRoleMissing: the role is missing and dryRun prevented creating it.
	ECSRoleMissing
	// ECSRoleUnknown: the role could not be looked up (typically no
	// iam:GetRole). The returned error says why; the caller should warn and
	// carry on rather than block an install whose account likely has the role.
	ECSRoleUnknown
)

// ecsRoleSettle is how long to wait after creating the role before anything
// uses it. IAM is eventually consistent, and the failure this guards against is
// exactly a role that exists but is not yet assumable. Paid once per account,
// on its first install. A variable so tests don't sleep.
var ecsRoleSettle = 10 * time.Second

// EnsureECSServiceLinkedRole makes sure ECS's service-linked role exists,
// creating it when missing unless dryRun is set.
//
// A non-nil error with ECSRoleUnknown is advisory (the lookup failed). A
// non-nil error with ECSRoleMissing means the role is absent and could not be
// created; the deploy would fail, so the caller should stop.
func EnsureECSServiceLinkedRole(dryRun bool) (ECSRoleStatus, error) {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return ECSRoleUnknown, err
	}
	client := iam.NewFromConfig(cfg)

	_, err = client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(ECSServiceLinkedRoleName)})
	switch {
	case err == nil:
		return ECSRolePresent, nil
	case !awsErrorCodeIs(err, "NoSuchEntity"):
		return ECSRoleUnknown, fmt.Errorf("could not check for the %s role: %w", ECSServiceLinkedRoleName, err)
	case dryRun:
		return ECSRoleMissing, nil
	}

	_, err = client.CreateServiceLinkedRole(ctx, &iam.CreateServiceLinkedRoleInput{
		AWSServiceName: aws.String(ecsServicePrincipal),
	})
	if err != nil {
		// Created concurrently (by ECS itself, or another install) between the
		// lookup and here: it exists, which is all that matters.
		if awsErrorCodeIs(err, "InvalidInput") && strings.Contains(err.Error(), "has been taken") {
			return ECSRolePresent, nil
		}
		return ECSRoleMissing, fmt.Errorf(
			"this AWS account has never used ECS, and its %s role could not be created: %w\n"+
				"   Ask an account administrator to run, once:\n     %s\n   then re-run this install",
			ECSServiceLinkedRoleName, err, ECSRoleCreateCommand)
	}
	if ecsRoleSettle > 0 {
		time.Sleep(ecsRoleSettle)
	}
	return ECSRoleCreated, nil
}
