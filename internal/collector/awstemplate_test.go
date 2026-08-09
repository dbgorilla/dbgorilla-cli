package collector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// There is no local template: a deploy either uses the published copy or fails
// telling the operator to update. It must never quietly deploy something else.
func TestResolveTemplate_NoLocalFallback(t *testing.T) {
	restore := hostedTemplateURL
	t.Cleanup(func() { hostedTemplateURL = restore })

	t.Run("published template is deployed by URL", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		defer srv.Close()
		hostedTemplateURL = srv.URL

		ref, err := resolveTemplate(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if ref.URL != srv.URL {
			t.Errorf("want the published URL, got %q", ref.URL)
		}
	})

	t.Run("unpublished version fails telling the operator to update", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		hostedTemplateURL = srv.URL

		_, err := resolveTemplate(context.Background(), "")
		if err == nil {
			t.Fatal("a missing published template must fail, not fall back to a local copy")
		}
		// The convention is that errors say what to do next.
		if !strings.Contains(err.Error(), "dbg upgrade") {
			t.Errorf("error should tell the operator to update, got: %v", err)
		}
		if !strings.Contains(err.Error(), "--template-url") {
			t.Errorf("error should offer the self-hosted escape hatch, got: %v", err)
		}
	})

	t.Run("unreachable host fails too", func(t *testing.T) {
		hostedTemplateURL = "https://127.0.0.1:1/nope.yaml"
		if _, err := resolveTemplate(context.Background(), ""); err == nil {
			t.Fatal("blocked egress must fail rather than deploy a local copy")
		}
	})
}

// A collector this CLI cannot configure correctly must be refused at discovery
// rather than deployed with engine = "postgres" stamped on it.
func TestCheckEngine(t *testing.T) {
	for _, ok := range []string{"postgres", "postgres14", "aurora-postgresql"} {
		if err := checkEngine(ok, "db"); err != nil {
			t.Errorf("%s should be supported: %v", ok, err)
		}
	}
	for _, bad := range []string{"mysql", "aurora-mysql", "mariadb", "sqlserver-ex", "oracle-se2"} {
		err := checkEngine(bad, "db")
		if err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
		if !errors.Is(err, ErrUnsupportedEngine) {
			t.Errorf("%s should report ErrUnsupportedEngine, got %v", bad, err)
		}
		// The convention is that errors say what to do next.
		if !strings.Contains(err.Error(), "--config") {
			t.Errorf("%s: error should offer a way forward, got %q", bad, err)
		}
	}
}

// CloudFormation rejects an empty CommaDelimitedList, so an all-password
// deployment still has to send something for RdsConnectResources.
func TestRdsConnectParam_NeverEmpty(t *testing.T) {
	got := rdsConnectParam([]AwsTarget{{
		InstanceID: "db", DbiResourceID: "db-RES", User: "u",
		Databases: []string{"app"}, AuthMethod: "password",
	}}, "us-east-2", "111122223333")
	if len(got) != 1 {
		t.Fatalf("want exactly one placeholder ARN, got %v", got)
	}
	if !strings.HasPrefix(got[0], "arn:aws:rds-db:") || !strings.Contains(got[0], "dbuser:none/none") {
		t.Errorf("want a match-nothing ARN, got %q", got[0])
	}
	// It must not widen to the template's wildcard default.
	if strings.Contains(got[0], "*") {
		t.Errorf("the fallback must not be a wildcard: %q", got[0])
	}
}

// The aws update path re-renders the stored config, so a key it cannot model
// would be silently dropped from a running collector.
func TestStrictParseConfig(t *testing.T) {
	valid := `
[dbgorilla]
agent_id = "a"
tenant_id = "t"
secret = "${DBG_SERVER_SECRET}"
`
	if _, err := StrictParseConfig(valid); err != nil {
		t.Fatalf("a config this build fully models should parse: %v", err)
	}

	future := valid + `
[engine.postgres]
statement_timeout_ms = 5000
`
	_, err := StrictParseConfig(future)
	if err == nil {
		t.Fatal("an unmodelled key must be refused, not dropped")
	}
	if !strings.Contains(err.Error(), "upgrade") && !strings.Contains(err.Error(), "Upgrade") {
		t.Errorf("error should tell the user to upgrade, got %q", err)
	}
	// ParseConfig stays permissive: encode-config must not require modelling
	// every collector option.
	if _, err := ParseConfig(future); err != nil {
		t.Errorf("ParseConfig should remain permissive: %v", err)
	}
}

// The template's version lives in two places that must agree: the Go constant
// the CLI pins its URL to, and the Metadata block CI publishes under. A bump to
// one without the other would have the CLI request a key CI never wrote.
func TestTemplateVersionMatches(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("cloudformation", "collector-fargate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "TemplateVersion: '" + TemplateVersion + "'"
	if !strings.Contains(string(raw), want) {
		t.Errorf("template does not declare %s — expected a line %q", TemplateVersion, want)
	}
	// And the URL the CLI deploys must be the versioned key, not a moving one.
	if !strings.HasSuffix(hostedTemplateURL, "/"+TemplateVersion+".yaml") {
		t.Errorf("hosted URL should pin the template version, got %q", hostedTemplateURL)
	}
}
