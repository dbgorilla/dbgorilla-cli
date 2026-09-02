package collector

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

// captureStdout collects what fn prints, for the commands that write straight
// to stdout (log tailing).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return out
}

// --- CloudWatch log tailing ------------------------------------------------

// logEventsJSON renders a FilterLogEvents page. nextToken empty ends pagination.
func logEventsJSON(nextToken string, events ...string) string {
	tok := ""
	if nextToken != "" {
		tok = `,"nextToken":"` + nextToken + `"`
	}
	return `{"events":[` + strings.Join(events, ",") + `]` + tok + `}`
}

func logEventJSON(id, message string, ts int64) string {
	return `{"eventId":"` + id + `","message":"` + message + `","timestamp":` + itoa64(ts) + `}`
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestTailLogs_PrintsEveryPage(t *testing.T) {
	// A busy collector exceeds one page. Ignoring the pagination token would
	// silently drop everything past the first page.
	stubAWS(t, newAWSFake(t).onSeq("FilterLogEvents",
		logEventsJSON("page2", logEventJSON("e1", "first line", 1000)),
		logEventsJSON("", logEventJSON("e2", "second line", 2000)),
	))

	out := captureStdout(t, func() {
		if err := TailLogs("/dbgorilla/collector/dbg-collector", "us-east-1", false); err != nil {
			t.Fatalf("TailLogs: %v", err)
		}
	})
	for _, want := range []string{"first line", "second line"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q, got: %s", want, out)
		}
	}
}

func TestTailLogs_ErrorNamesTheLogGroup(t *testing.T) {
	stubAWS(t, newAWSFake(t).fail("FilterLogEvents", http.StatusBadRequest,
		`{"__type":"ResourceNotFoundException","message":"The specified log group does not exist"}`))

	err := TailLogs("/dbgorilla/collector/missing", "us-east-1", false)
	if err == nil || !strings.Contains(err.Error(), "/dbgorilla/collector/missing") {
		t.Fatalf("err = %v, want the log group named", err)
	}
}

func TestTailLogs_CredentialFailure(t *testing.T) {
	stubAWSConfigError(t, errors.New("no credentials"))
	if err := TailLogs("group", "", false); err == nil {
		t.Fatal("expected the credential error")
	}
}

// --- identity / availability ----------------------------------------------

func TestAwsAvailable(t *testing.T) {
	t.Run("working credentials", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("GetCallerIdentity", callerIdentityXML))
		if err := AwsAvailable(); err != nil {
			t.Fatalf("AwsAvailable: %v", err)
		}
	})

	// The message has to name the fix, because "credentials aren't working" on
	// its own tells an operator nothing they can act on.
	t.Run("rejected credentials point at the fix", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("GetCallerIdentity", http.StatusForbidden,
			awsErrorXML("ExpiredToken", "the security token included in the request is expired")))
		err := AwsAvailable()
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "aws sso login") || !strings.Contains(err.Error(), "AWS_PROFILE") {
			t.Errorf("error should name the way out, got: %v", err)
		}
	})

	t.Run("config resolution failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no providers"))
		err := AwsAvailable()
		if err == nil || !strings.Contains(err.Error(), "aws configure") {
			t.Fatalf("err = %v, want the configuration hint", err)
		}
	})
}

func TestAwsIdentity(t *testing.T) {
	t.Run("returns the caller ARN", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("GetCallerIdentity", callerIdentityXML))
		arn, err := AwsIdentity()
		if err != nil || arn != "arn:aws:iam::111122223333:user/dev" {
			t.Fatalf("got (%q,%v)", arn, err)
		}
	})

	t.Run("api error", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("GetCallerIdentity", http.StatusForbidden,
			awsErrorXML("AccessDenied", "denied")))
		if _, err := AwsIdentity(); err == nil {
			t.Fatal("expected the API error")
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if _, err := AwsIdentity(); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

func TestAwsAccountID(t *testing.T) {
	t.Run("returns the account", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).on("GetCallerIdentity", callerIdentityXML))
		acct, err := AwsAccountID()
		if err != nil || acct != "111122223333" {
			t.Fatalf("got (%q,%v)", acct, err)
		}
	})

	t.Run("api error", func(t *testing.T) {
		stubAWS(t, newAWSFake(t).fail("GetCallerIdentity", http.StatusForbidden,
			awsErrorXML("AccessDenied", "denied")))
		if _, err := AwsAccountID(); err == nil {
			t.Fatal("expected the API error")
		}
	})

	t.Run("credential failure", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if _, err := AwsAccountID(); err == nil {
			t.Fatal("expected the credential error")
		}
	})
}

func TestAwsRegion(t *testing.T) {
	t.Run("reports the resolved region", func(t *testing.T) {
		stubAWS(t, newAWSFake(t))
		if got := AwsRegion(); got != "us-east-1" {
			t.Errorf("AwsRegion = %q", got)
		}
	})

	// Captured at install time to pin later operations; an unresolvable region
	// must read as "unknown" rather than crash the command.
	t.Run("empty when the config cannot be resolved", func(t *testing.T) {
		stubAWSConfigError(t, errors.New("no credentials"))
		if got := AwsRegion(); got != "" {
			t.Errorf("AwsRegion = %q, want empty", got)
		}
	})
}

// --- small exported helpers -----------------------------------------------

func TestCommandCatalog_IsAStableIndependentCopy(t *testing.T) {
	got := CommandCatalog("postgres")
	if len(got) != 2 || got[0] != CmdExecuteQuery || got[1] != CmdExplain {
		t.Fatalf("catalog = %v", got)
	}
	// The picker mutates what it is given; the catalog must not be aliased to
	// the package's own slice.
	got[0] = "mutated"
	if CommandCatalog("postgres")[0] != CmdExecuteQuery {
		t.Error("CommandCatalog must return a copy")
	}
}

func TestHostedTemplateURL_IsVersionPinned(t *testing.T) {
	url := HostedTemplateURL()
	if !strings.HasSuffix(url, ".yaml") {
		t.Errorf("template URL = %q", url)
	}
	// What a customer reviews must be what their account deploys, so the URL
	// carries the template version rather than a floating "latest".
	if !strings.Contains(url, TemplateVersion) {
		t.Errorf("template URL %q should carry version %q", url, TemplateVersion)
	}
}

func TestTemplateRefSource(t *testing.T) {
	if got := (templateRef{URL: "https://example.com/t.yaml"}).Source(); got != "https://example.com/t.yaml" {
		t.Errorf("Source = %q", got)
	}
}

func TestReachable(t *testing.T) {
	t.Run("an open port is reachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		if err := Reachable(ln.Addr().String()); err != nil {
			t.Errorf("Reachable: %v", err)
		}
	})

	t.Run("a closed port is not", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close() // nothing listening now
		if err := Reachable(addr); err == nil {
			t.Error("a closed port must not report reachable")
		}
	})
}

func TestTargetDial_DefaultsThePort(t *testing.T) {
	if got := TargetDial(AwsTarget{Host: "db.example.com"}); got != "db.example.com:5432" {
		t.Errorf("TargetDial = %q, want the Postgres default port", got)
	}
	if got := TargetDial(AwsTarget{Host: "db.example.com", Port: 6432}); got != "db.example.com:6432" {
		t.Errorf("TargetDial = %q", got)
	}
}
