package collector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The CloudFormation lifecycle is the part of an AWS install that can destroy
// something: a wrong branch here deletes a healthy stack or reports success
// over a failed deploy. None of it was reachable in tests before, because every
// path needed a live account.

// --- fixtures --------------------------------------------------------------

func stacksXML(status string, params ...string) string {
	return `<DescribeStacksResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
  <DescribeStacksResult>
    <Stacks>
      <member>
        <StackName>dbg-collector</StackName>
        <StackStatus>` + status + `</StackStatus>
        <Parameters>` + strings.Join(params, "") + `</Parameters>
      </member>
    </Stacks>
  </DescribeStacksResult>
</DescribeStacksResponse>`
}

func stackParamXML(key, value string) string {
	return `<member><ParameterKey>` + key + `</ParameterKey><ParameterValue>` + value + `</ParameterValue></member>`
}

const noSuchStackXML = `<ErrorResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
  <Error><Type>Sender</Type><Code>ValidationError</Code>
  <Message>Stack with id dbg-collector does not exist</Message></Error>
</ErrorResponse>`

func createStackXML() string {
	return `<CreateStackResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
  <CreateStackResult><StackId>arn:aws:cloudformation:us-east-1:111122223333:stack/dbg-collector/abc</StackId></CreateStackResult>
</CreateStackResponse>`
}

func updateStackXML() string {
	return `<UpdateStackResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
  <UpdateStackResult><StackId>arn:aws:cloudformation:us-east-1:111122223333:stack/dbg-collector/abc</StackId></UpdateStackResult>
</UpdateStackResponse>`
}

func deleteStackXML() string {
	return `<DeleteStackResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/"/>`
}

func stackEventsXML(status, reason, logicalID string) string {
	return `<DescribeStackEventsResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
  <DescribeStackEventsResult>
    <StackEvents>
      <member>
        <ResourceStatus>` + status + `</ResourceStatus>
        <ResourceStatusReason>` + reason + `</ResourceStatusReason>
        <LogicalResourceId>` + logicalID + `</LogicalResourceId>
      </member>
    </StackEvents>
  </DescribeStackEventsResult>
</DescribeStackEventsResponse>`
}

// templateServer stands in for the published S3 template, so a deploy test
// never touches the network.
func templateServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/collector-fargate.yaml"
}

// --- stackState ------------------------------------------------------------

func TestStackState(t *testing.T) {
	ctx := context.Background()

	t.Run("existing stack reports its status", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStacks", stacksXML("CREATE_COMPLETE")))
		exists, status, err := stackState(ctx, cfnClient(t), "dbg-collector")
		if err != nil || !exists || status != "CREATE_COMPLETE" {
			t.Fatalf("got (%v,%q,%v)", exists, status, err)
		}
	})

	// "Does not exist" is a normal answer, not a failure: it is how a first
	// install tells create from update.
	t.Run("missing stack is not an error", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeStacks", http.StatusBadRequest, noSuchStackXML))
		exists, status, err := stackState(ctx, cfnClient(t), "dbg-collector")
		if err != nil {
			t.Fatalf("a missing stack must not error, got %v", err)
		}
		if exists || status != "" {
			t.Errorf("got (%v,%q), want (false,\"\")", exists, status)
		}
	})

	t.Run("a real API error is reported", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeStacks", http.StatusForbidden,
			awsErrorXML("AccessDenied", "not authorized to perform cloudformation:DescribeStacks")))
		if _, _, err := stackState(ctx, cfnClient(t), "dbg-collector"); err == nil {
			t.Fatal("an access-denied must not look like a missing stack")
		}
	})

	t.Run("empty stack list is treated as missing", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStacks",
			`<DescribeStacksResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
			   <DescribeStacksResult><Stacks></Stacks></DescribeStacksResult>
			 </DescribeStacksResponse>`))
		exists, _, err := stackState(ctx, cfnClient(t), "dbg-collector")
		if err != nil || exists {
			t.Fatalf("got (%v,%v), want (false,nil)", exists, err)
		}
	})
}

// --- deploy: the branch that can delete things -----------------------------

func TestDeploy_CreatesWhenNoStackExists(t *testing.T) {
	f := newAWSFake(t).
		// First poll: nothing there, so this is a create. Later polls: the
		// create waiter watching the new stack reach CREATE_COMPLETE.
		onSeq("DescribeStacks", noSuchStackXML, stacksXML("CREATE_COMPLETE")).
		on("CreateStack", createStackXML())
	stubAWS(t, f)

	d := FargateDeploy{
		StackName:   "dbg-collector",
		TemplateURL: templateServer(t),
		Params:      map[string]string{"CollectorImage": "img:1"},
	}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.called("CreateStack") != 1 {
		t.Errorf("CreateStack called %d times, want 1", f.called("CreateStack"))
	}
	if f.called("UpdateStack") > 0 {
		t.Error("a stack that does not exist cannot be updated")
	}
	// CAPABILITY_IAM is required or CloudFormation refuses the whole stack,
	// which creates the task role.
	if !strings.Contains(f.sentBody(), "CAPABILITY_IAM") {
		t.Error("create must declare CAPABILITY_IAM")
	}
	// Parameters must actually reach CloudFormation.
	if !strings.Contains(f.sentBody(), "CollectorImage") {
		t.Error("stack parameters should be sent on create")
	}
}

// A create that ends in CREATE_FAILED must report the reason from the stack's
// own events, not just "the waiter failed".
func TestDeploy_CreateFailureCarriesTheReason(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		onSeq("DescribeStacks", noSuchStackXML, stacksXML("CREATE_FAILED")).
		on("CreateStack", createStackXML()).
		on("DescribeStackEvents", stackEventsXML(
			"CREATE_FAILED", "The image could not be pulled", "CollectorTaskDefinition")))

	err := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}.Run()
	if err == nil {
		t.Fatal("a CREATE_FAILED stack must fail the deploy")
	}
	if !strings.Contains(err.Error(), "could not be pulled") {
		t.Errorf("error should carry the stack's own reason, got: %v", err)
	}
}

func TestDeploy_DryRunOnlyValidates(t *testing.T) {
	f := newAWSFake(t).on("ValidateTemplate",
		`<ValidateTemplateResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
		   <ValidateTemplateResult><Description>ok</Description></ValidateTemplateResult>
		 </ValidateTemplateResponse>`)
	stubAWS(t, f)

	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t), DryRun: true}
	if err := d.Run(); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// The point of a dry run: nothing is created, updated or deleted.
	for _, forbidden := range []string{"CreateStack", "UpdateStack", "DeleteStack"} {
		if f.called(forbidden) > 0 {
			t.Errorf("dry run must not call %s", forbidden)
		}
	}
}

func TestDeploy_DryRunSurfacesValidationFailure(t *testing.T) {
	stubAWS(t, newAWSFake(t).fail("ValidateTemplate", http.StatusBadRequest,
		awsErrorXML("ValidationError", "Template format error")))

	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t), DryRun: true}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("err = %v, want a template validation failure", err)
	}
}

// A healthy stack is updated in place. Re-running an unchanged deploy must be a
// silent success — that is what makes the install idempotent.
func TestDeploy_UpdatesHealthyStack(t *testing.T) {
	f := newAWSFake(t).
		on("DescribeStacks", stacksXML("UPDATE_COMPLETE")).
		on("UpdateStack", updateStackXML())
	stubAWS(t, f)

	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.called("UpdateStack") != 1 {
		t.Errorf("UpdateStack called %d times, want 1", f.called("UpdateStack"))
	}
	if f.called("DeleteStack") > 0 {
		t.Error("a healthy stack must never be deleted")
	}
}

func TestDeploy_NoUpdatesIsSuccess(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		on("DescribeStacks", stacksXML("CREATE_COMPLETE")).
		fail("UpdateStack", http.StatusBadRequest,
			awsErrorXML("ValidationError", "No updates are to be performed.")))

	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}
	if err := d.Run(); err != nil {
		t.Fatalf("an unchanged re-run must succeed, got %v", err)
	}
}

// A stack stuck in ROLLBACK_COMPLETE cannot be updated; the only recovery is
// delete-then-create. This is the one path that legitimately deletes.
func TestDeploy_RecreatesUnusableStack(t *testing.T) {
	f := newAWSFake(t).
		onSeq("DescribeStacks",
			stacksXML("ROLLBACK_COMPLETE"), // initial state check
			noSuchStackXML,                 // delete waiter: gone
			stacksXML("CREATE_COMPLETE"),   // create waiter: the fresh stack
		).
		on("DeleteStack", deleteStackXML()).
		on("CreateStack", createStackXML())
	stubAWS(t, f)

	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}
	err := d.Run()
	if f.called("DeleteStack") != 1 {
		t.Errorf("a ROLLBACK_COMPLETE stack must be deleted first (got %d calls), err=%v",
			f.called("DeleteStack"), err)
	}
	if f.called("CreateStack") != 1 {
		t.Errorf("the delete must be followed by a fresh create (got %d), err=%v",
			f.called("CreateStack"), err)
	}
	if f.called("UpdateStack") > 0 {
		t.Error("an unusable stack cannot be updated in place")
	}
}

// If the cleanup delete fails, the deploy must stop and say the stack was
// stuck — not push on and fail confusingly at CreateStack.
func TestDeploy_RecreateStopsWhenCleanupFails(t *testing.T) {
	f := newAWSFake(t).
		on("DescribeStacks", stacksXML("ROLLBACK_COMPLETE")).
		fail("DeleteStack", http.StatusForbidden, awsErrorXML("AccessDenied", "denied"))
	stubAWS(t, f)

	err := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}.Run()
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK_COMPLETE") {
		t.Fatalf("err = %v, want the stuck state named", err)
	}
	if f.called("CreateStack") > 0 {
		t.Error("must not create over a stack that could not be deleted")
	}
}

func TestStackNeedsRecreate_OnlyUnusableStates(t *testing.T) {
	for _, s := range []string{"ROLLBACK_COMPLETE", "ROLLBACK_FAILED", "CREATE_FAILED", "REVIEW_IN_PROGRESS", "DELETE_FAILED"} {
		if !stackNeedsRecreate(s) {
			t.Errorf("%s cannot be updated in place; it must be recreated", s)
		}
	}
	// A healthy stack being classed as unusable would delete a running collector.
	for _, s := range []string{"CREATE_COMPLETE", "UPDATE_COMPLETE", "UPDATE_ROLLBACK_COMPLETE", "UPDATE_IN_PROGRESS"} {
		if stackNeedsRecreate(s) {
			t.Errorf("%s must NOT trigger a delete-then-create", s)
		}
	}
}

func TestStackInProgress(t *testing.T) {
	for _, s := range []string{"CREATE_IN_PROGRESS", "UPDATE_IN_PROGRESS", "DELETE_IN_PROGRESS", "ROLLBACK_IN_PROGRESS"} {
		if !stackInProgress(s) {
			t.Errorf("%s is an operation in flight", s)
		}
	}
	// REVIEW_IN_PROGRESS is a changeset-only stack: nothing is running, and
	// treating it as busy would wedge the install permanently.
	if stackInProgress("REVIEW_IN_PROGRESS") {
		t.Error("REVIEW_IN_PROGRESS is not an operation in flight")
	}
	if stackInProgress("CREATE_COMPLETE") {
		t.Error("CREATE_COMPLETE is not in progress")
	}
}

func TestDeploy_RefusesWhileAnotherOperationRuns(t *testing.T) {
	stubAWS(t, newAWSFake(t).on("DescribeStacks", stacksXML("UPDATE_IN_PROGRESS")))

	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}
	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("err = %v, want a refusal naming the in-flight operation", err)
	}
}

func TestDeploy_TemplateUnreachableStopsBeforeAnyAWSCall(t *testing.T) {
	f := newAWSFake(t)
	stubAWS(t, f)

	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: "http://127.0.0.1:1/nope.yaml"}
	if err := d.Run(); err == nil {
		t.Fatal("an unreachable template must fail the deploy")
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing should reach AWS before the template resolves, got %v", f.calls)
	}
}

func TestDeploy_CredentialFailure(t *testing.T) {
	stubAWSConfigError(t, errors.New("no credentials"))
	d := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}
	if err := d.Run(); err == nil {
		t.Fatal("expected the credential error")
	}
}

func TestRunQuiet_ReturnsNoOutput(t *testing.T) {
	stubAWS(t, newAWSFake(t).
		// State check, then the update waiter watching it land.
		onSeq("DescribeStacks", stacksXML("CREATE_COMPLETE"), stacksXML("UPDATE_COMPLETE")).
		on("UpdateStack", updateStackXML()))

	out, err := FargateDeploy{StackName: "dbg-collector", TemplateURL: templateServer(t)}.RunQuiet()
	if err != nil {
		t.Fatalf("RunQuiet: %v", err)
	}
	if out != "" {
		t.Errorf("RunQuiet should return no captured output, got %q", out)
	}
}

// --- waiter classification -------------------------------------------------

// Giving up waiting is NOT a failed deploy. Callers roll back on failure, and a
// rollback deprovisions the identity and deletes the stack — doing that to a
// still-converging deploy destroys a healthy install.
func TestWaitErr_TimeoutIsTaggedDistinctly(t *testing.T) {
	timeout := waitErr(errors.New("exceeded max wait time for StackCreateComplete waiter"))
	if !errors.Is(timeout, ErrDeployTimeout) {
		t.Fatalf("a waiter timeout must be ErrDeployTimeout, got %v", timeout)
	}
	// The underlying message survives for the log.
	if !strings.Contains(timeout.Error(), "exceeded max wait time") {
		t.Errorf("original error should be wrapped, got %v", timeout)
	}

	terminal := waitErr(errors.New("waiter state transitioned to Failure"))
	if errors.Is(terminal, ErrDeployTimeout) {
		t.Error("a terminal CREATE_FAILED must not be reported as a timeout")
	}
	if waitErr(nil) != nil {
		t.Error("waitErr(nil) should be nil")
	}
}

func TestDeployTimeout_IsExposedForMessaging(t *testing.T) {
	if DeployTimeout() <= 0 {
		t.Error("the command layer prints this budget; it must be a real duration")
	}
}

// --- stackFailureReason ----------------------------------------------------

func TestStackFailureReason(t *testing.T) {
	ctx := context.Background()

	t.Run("names the failed resource and why", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStackEvents",
			stackEventsXML("CREATE_FAILED", "Resource handler returned message: insufficient capacity", "CollectorService")))
		got := stackFailureReason(ctx, cfnClient(t), "dbg-collector")
		if !strings.Contains(got, "insufficient capacity") || !strings.Contains(got, "CollectorService") {
			t.Errorf("reason = %q, want the message and the logical id", got)
		}
	})

	t.Run("no failed event yields nothing rather than noise", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStackEvents",
			stackEventsXML("CREATE_COMPLETE", "all good", "CollectorService")))
		if got := stackFailureReason(ctx, cfnClient(t), "dbg-collector"); got != "" {
			t.Errorf("reason = %q, want empty", got)
		}
	})

	// Best-effort: this runs while already reporting a failure, so it must never
	// replace the real error with one of its own.
	t.Run("an API error is swallowed", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeStackEvents", http.StatusForbidden,
			awsErrorXML("AccessDenied", "denied")))
		if got := stackFailureReason(ctx, cfnClient(t), "dbg-collector"); got != "" {
			t.Errorf("reason = %q, want empty on error", got)
		}
	})
}

// --- top-level stack operations --------------------------------------------

func TestDeleteStack(t *testing.T) {
	t.Run("deletes the named stack in the pinned region", func(t *testing.T) {
		f := newAWSFake(t).on("DeleteStack", deleteStackXML())
		stubAWS(t, f)
		if err := DeleteStack("dbg-collector", "eu-west-1"); err != nil {
			t.Fatalf("DeleteStack: %v", err)
		}
		if !strings.Contains(f.sentBody(), "dbg-collector") {
			t.Error("the request should name the stack")
		}
	})

	t.Run("api error is wrapped with the stack name", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DeleteStack", http.StatusForbidden,
			awsErrorXML("AccessDenied", "denied")))
		err := DeleteStack("dbg-collector", "eu-west-1")
		if err == nil || !strings.Contains(err.Error(), "dbg-collector") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if err := DeleteStack("dbg-collector", ""); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

func TestStackStatus(t *testing.T) {
	t.Run("returns the status", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStacks", stacksXML("UPDATE_COMPLETE")))
		got, err := StackStatus("dbg-collector", "us-east-1")
		if err != nil || got != "UPDATE_COMPLETE" {
			t.Fatalf("got (%q,%v)", got, err)
		}
	})

	// A missing stack is how `collector status` says "not installed here".
	t.Run("missing stack is empty status and no error", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeStacks", http.StatusBadRequest, noSuchStackXML))
		got, err := StackStatus("dbg-collector", "us-east-1")
		if err != nil || got != "" {
			t.Fatalf("got (%q,%v), want empty status and nil error", got, err)
		}
	})

	t.Run("empty stack list is also empty status", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStacks",
			`<DescribeStacksResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
			   <DescribeStacksResult><Stacks></Stacks></DescribeStacksResult>
			 </DescribeStacksResponse>`))
		got, err := StackStatus("dbg-collector", "us-east-1")
		if err != nil || got != "" {
			t.Fatalf("got (%q,%v)", got, err)
		}
	})

	t.Run("real error surfaces", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeStacks", http.StatusForbidden,
			awsErrorXML("AccessDenied", "denied")))
		if _, err := StackStatus("dbg-collector", "us-east-1"); err == nil {
			t.Fatal("an access-denied must not read as 'not installed'")
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if _, err := StackStatus("dbg-collector", ""); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

// --- stackParam ------------------------------------------------------------

func TestStackParam(t *testing.T) {
	ctx := context.Background()

	t.Run("reads the value", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStacks",
			stacksXML("CREATE_COMPLETE", stackParamXML(configParamKey, "YmFzZTY0"))))
		got, err := stackParam(ctx, cfnClient(t), "dbg-collector", configParamKey)
		if err != nil || got != "YmFzZTY0" {
			t.Fatalf("got (%q,%v)", got, err)
		}
	})

	t.Run("missing stack points at install", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStacks",
			`<DescribeStacksResponse xmlns="http://cloudformation.amazonaws.com/doc/2010-05-15/">
			   <DescribeStacksResult><Stacks></Stacks></DescribeStacksResult>
			 </DescribeStacksResponse>`))
		_, err := stackParam(ctx, cfnClient(t), "dbg-collector", configParamKey)
		// The command must be the one that exists: `install` is registered on
		// `collector`, so a bare `dbg install` fails with "unknown command".
		if err == nil || !strings.Contains(err.Error(), "dbg collector install --target aws") {
			t.Fatalf("err = %v, want a pointer to the real install command", err)
		}
	})

	// A stack from an older CLI has no config parameter. Saying so beats a
	// confusing decode failure downstream.
	t.Run("missing parameter names the version problem", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("DescribeStacks",
			stacksXML("CREATE_COMPLETE", stackParamXML("SomethingElse", "x"))))
		_, err := stackParam(ctx, cfnClient(t), "dbg-collector", configParamKey)
		if err == nil || !strings.Contains(err.Error(), "predates this CLI version") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("api error is wrapped", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("DescribeStacks", http.StatusForbidden,
			awsErrorXML("AccessDenied", "denied")))
		if _, err := stackParam(ctx, cfnClient(t), "dbg-collector", configParamKey); err == nil {
			t.Fatal("expected the API error")
		}
	})
}

// --- UpgradeImage ----------------------------------------------------------

func TestUpgradeImage(t *testing.T) {
	t.Run("sends the new image and holds everything else", func(t *testing.T) {
		f := newAWSFake(t).
			on("UpdateStack", updateStackXML()).
			on("DescribeStacks", stacksXML("UPDATE_COMPLETE"))
		stubAWS(t, f)

		if err := UpgradeImage("dbg-collector", "us-east-1", "ghcr.io/dbgorilla/collector%3Av2"); err != nil {
			t.Fatalf("UpgradeImage: %v", err)
		}
		body := f.sentBody()
		// Every other parameter must ride on UsePreviousValue, or an upgrade
		// silently drops the monitored databases.
		if !strings.Contains(body, "UsePreviousValue=true") {
			t.Error("non-image parameters must use their previous values")
		}
		if !strings.Contains(body, "UsePreviousTemplate=true") {
			t.Error("an image upgrade must not swap the template")
		}
	})

	// "Nothing to do" is a legitimate outcome and must read as one.
	t.Run("already on that image is a clear message", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("UpdateStack", http.StatusBadRequest,
			awsErrorXML("ValidationError", "No updates are to be performed.")))
		err := UpgradeImage("dbg-collector", "us-east-1", "img:v2")
		if err == nil || !strings.Contains(err.Error(), "nothing to upgrade") {
			t.Fatalf("err = %v, want the already-current message", err)
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if err := UpgradeImage("dbg-collector", "", "img:v2"); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

// --- ECS service control ---------------------------------------------------

func TestScaleService(t *testing.T) {
	t.Run("sets the desired count", func(t *testing.T) {
		f := newAWSFake(t).on("UpdateService", `{"service":{"serviceName":"dbg-collector"}}`)
		stubAWS(t, f)
		if err := ScaleService("dbg-collector", "us-east-1", 0); err != nil {
			t.Fatalf("ScaleService: %v", err)
		}
		if !strings.Contains(f.sentBody(), `"desiredCount":0`) {
			t.Errorf("request should carry the desired count, got %s", f.sentBody())
		}
	})

	t.Run("error names the service and count", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("UpdateService", http.StatusBadRequest,
			`{"__type":"ServiceNotFoundException","message":"service not found"}`))
		err := ScaleService("dbg-collector", "us-east-1", 1)
		if err == nil || !strings.Contains(err.Error(), "dbg-collector") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if err := ScaleService("dbg-collector", "", 1); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

func TestRestartService(t *testing.T) {
	t.Run("forces a new deployment without changing config", func(t *testing.T) {
		f := newAWSFake(t).on("UpdateService", `{"service":{"serviceName":"dbg-collector"}}`)
		stubAWS(t, f)
		if err := RestartService("dbg-collector", "us-east-1"); err != nil {
			t.Fatalf("RestartService: %v", err)
		}
		body := f.sentBody()
		if !strings.Contains(body, `"forceNewDeployment":true`) {
			t.Errorf("request should force a new deployment, got %s", body)
		}
		if strings.Contains(body, `"desiredCount"`) {
			t.Error("a restart must not change the desired count")
		}
	})

	t.Run("error is wrapped", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("UpdateService", http.StatusBadRequest,
			`{"__type":"ServiceNotFoundException","message":"service not found"}`))
		if err := RestartService("dbg-collector", "us-east-1"); err == nil {
			t.Fatal("expected the API error")
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if err := RestartService("dbg-collector", ""); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}
