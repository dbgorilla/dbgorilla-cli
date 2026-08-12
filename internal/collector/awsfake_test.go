package collector

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
)

// A fake AWS transport, so every SDK call path in this package is reachable
// without an account. The alternative is leaving discovery, deploy and stack
// lifecycle at 0% and finding out they are broken during a customer install.
//
// Responses are matched on the request body: the query-protocol services (RDS,
// CloudFormation, STS) put the operation name in an `Action=` form field, so
// one handler per operation is enough. Anything unmatched fails the test loudly
// rather than returning a plausible-looking empty result.

// maxFakeCalls bounds how many times one operation may be called before the
// fake declares a stuck waiter. Well above any real poll count in these tests.
const maxFakeCalls = 40

type awsFake struct {
	t *testing.T
	// responses maps an Action name to the body (or successive bodies) to
	// return. The last entry repeats once exhausted, so a waiter that polls
	// does not need a body per poll.
	responses map[string][]string
	// status maps an Action name to a non-200 status (default 200).
	status map[string]int
	// seen counts calls per Action, to walk a sequenced response list.
	seen map[string]int
	// calls records the Action of every request, in order.
	calls []string
	// bodies records the raw request body of every request, in order, so a
	// test can assert on what was actually sent to AWS.
	bodies []string
}

func newAWSFake(t *testing.T) *awsFake {
	t.Helper()
	return &awsFake{
		t:         t,
		responses: map[string][]string{},
		status:    map[string]int{},
		seen:      map[string]int{},
	}
}

// on registers the body returned for an operation.
func (f *awsFake) on(action, body string) *awsFake {
	f.responses[action] = []string{body}
	return f
}

// onSeq registers successive bodies for repeated calls to one operation. The
// final body repeats forever, which is what a polling waiter needs.
func (f *awsFake) onSeq(action string, bodies ...string) *awsFake {
	f.responses[action] = bodies
	return f
}

// fail registers an HTTP status + error body for an operation.
func (f *awsFake) fail(action string, code int, body string) *awsFake {
	f.status[action] = code
	f.responses[action] = []string{body}
	return f
}

// sentBody returns the concatenated request bodies for assertions.
func (f *awsFake) sentBody() string { return strings.Join(f.bodies, "\n") }

// called reports how many times an operation was invoked.
func (f *awsFake) called(action string) int {
	n := 0
	for _, c := range f.calls {
		if c == action {
			n++
		}
	}
	return n
}

func (f *awsFake) RoundTrip(req *http.Request) (*http.Response, error) {
	action := "unknown"
	var raw []byte
	if req.Body != nil {
		raw, _ = io.ReadAll(req.Body)
		for _, kv := range strings.Split(string(raw), "&") {
			if rest, ok := strings.CutPrefix(kv, "Action="); ok {
				action = rest
				break
			}
		}
	}
	// The JSON-protocol services (ECS, CloudWatch Logs) name the operation in a
	// header instead of the body.
	if action == "unknown" {
		if t := req.Header.Get("X-Amz-Target"); t != "" {
			action = t[strings.LastIndex(t, ".")+1:]
		}
	}
	f.calls = append(f.calls, action)
	f.bodies = append(f.bodies, string(raw))

	// A CloudFormation waiter polls until the stack reaches a terminal state or
	// the 30-minute budget expires. A fixture that never reaches one would hang
	// the whole test binary, so cut it short with a message that says which
	// operation is spinning and why.
	if f.seen[action] >= maxFakeCalls {
		f.t.Fatalf("%s called %d times — the stubbed responses never reach a terminal state, "+
			"so a waiter is polling forever. Give it a sequence ending in the state the "+
			"waiter is waiting for (onSeq).", action, f.seen[action])
	}

	bodies, ok := f.responses[action]
	if !ok || len(bodies) == 0 {
		f.t.Errorf("unstubbed AWS call: %s (stub it with .on(%q, ...))", action, action)
		return awsResponse(http.StatusInternalServerError,
			`<ErrorResponse><Error><Code>NotStubbed</Code></Error></ErrorResponse>`), nil
	}
	// Walk the sequence, then hold on the last entry.
	i := min(f.seen[action], len(bodies)-1)
	f.seen[action]++

	body := bodies[i]
	// An explicit status wins; otherwise the body decides, so a sequenced list
	// can mix successes and errors (a delete waiter, for instance, ends on a
	// "does not exist" error while earlier polls succeed).
	code := http.StatusOK
	if c, ok := f.status[action]; ok {
		code = c
	} else if isAWSErrorBody(body) {
		code = http.StatusBadRequest
	}
	return awsResponse(code, body), nil
}

// isAWSErrorBody reports whether a fixture is an error rather than a result.
func isAWSErrorBody(body string) bool {
	return strings.Contains(body, "<ErrorResponse") || strings.Contains(body, `"__type"`)
}

// awsResponse builds a response whose content type matches the body, so the
// XML (query-protocol) and JSON services can share one transport.
func awsResponse(code int, body string) *http.Response {
	ct := "application/x-amz-json-1.1"
	if strings.HasPrefix(strings.TrimSpace(body), "<") {
		ct = "text/xml"
	}
	return &http.Response{
		StatusCode: code,
		Status:     http.StatusText(code),
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// stubAWS points every SDK client this package builds at the fake, with static
// credentials and a fixed region so nothing touches the environment.
func stubAWS(t *testing.T, f *awsFake) {
	t.Helper()
	orig := loadAWSConfig
	loadAWSConfig = func(_ context.Context, region string) (aws.Config, error) {
		cfg := aws.Config{
			Region:      "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider("AKIATEST", "secret", ""),
			HTTPClient:  &http.Client{Transport: f},
			// No retries: a test that provokes an error should fail fast rather
			// than spend the SDK's default backoff schedule.
			RetryMaxAttempts: 1,
		}
		if region != "" {
			cfg.Region = region
		}
		return cfg, nil
	}
	t.Cleanup(func() { loadAWSConfig = orig })
}

// cfnClient builds a CloudFormation client on the stubbed config, for the
// unexported helpers that take one.
func cfnClient(t *testing.T) *cloudformation.Client {
	t.Helper()
	cfg, err := loadAWSConfig(context.Background(), "")
	if err != nil {
		t.Fatalf("loadAWSConfig: %v", err)
	}
	return cloudformation.NewFromConfig(cfg)
}

// stubAWSConfigError makes credential resolution itself fail, which is the
// "no AWS_PROFILE / expired SSO" path every AWS command has to survive.
func stubAWSConfigError(t *testing.T, err error) {
	t.Helper()
	orig := loadAWSConfig
	loadAWSConfig = func(context.Context, string) (aws.Config, error) {
		return aws.Config{}, err
	}
	t.Cleanup(func() { loadAWSConfig = orig })
}

// --- XML fixtures ---------------------------------------------------------

// describeInstancesXML renders a DescribeDBInstances response. Fields are the
// ones the mappers read; anything else the SDK ignores.
func describeInstancesXML(instances ...string) string {
	return `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
  <DescribeDBInstancesResult>
    <DBInstances>` + strings.Join(instances, "") + `</DBInstances>
  </DescribeDBInstancesResult>
</DescribeDBInstancesResponse>`
}

func instanceXML(id, engine string) string {
	return `<DBInstance>
      <DBInstanceIdentifier>` + id + `</DBInstanceIdentifier>
      <Engine>` + engine + `</Engine>
      <DbiResourceId>db-RES-` + id + `</DbiResourceId>
      <DBName>appdb</DBName>
      <IAMDatabaseAuthenticationEnabled>true</IAMDatabaseAuthenticationEnabled>
      <Endpoint>
        <Address>` + id + `.abc.us-east-1.rds.amazonaws.com</Address>
        <Port>5432</Port>
      </Endpoint>
      <DBSubnetGroup>
        <Subnets>
          <Subnet><SubnetIdentifier>subnet-a</SubnetIdentifier></Subnet>
          <Subnet><SubnetIdentifier>subnet-b</SubnetIdentifier></Subnet>
        </Subnets>
      </DBSubnetGroup>
      <VpcSecurityGroups>
        <VpcSecurityGroupMembership>
          <VpcSecurityGroupId>sg-live</VpcSecurityGroupId>
          <Status>active</Status>
        </VpcSecurityGroupMembership>
      </VpcSecurityGroups>
    </DBInstance>`
}

func describeClustersXML(clusters ...string) string {
	return `<DescribeDBClustersResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
  <DescribeDBClustersResult>
    <DBClusters>` + strings.Join(clusters, "") + `</DBClusters>
  </DescribeDBClustersResult>
</DescribeDBClustersResponse>`
}

func clusterXML(id, engine string) string {
	return `<DBCluster>
      <DBClusterIdentifier>` + id + `</DBClusterIdentifier>
      <Engine>` + engine + `</Engine>
      <Endpoint>` + id + `.cluster-abc.us-east-1.rds.amazonaws.com</Endpoint>
      <Port>5432</Port>
      <DbClusterResourceId>cluster-RES-` + id + `</DbClusterResourceId>
      <DatabaseName>appdb</DatabaseName>
      <IAMDatabaseAuthenticationEnabled>true</IAMDatabaseAuthenticationEnabled>
      <DBSubnetGroup>` + id + `-subnets</DBSubnetGroup>
      <VpcSecurityGroups>
        <VpcSecurityGroupMembership>
          <VpcSecurityGroupId>sg-live</VpcSecurityGroupId>
          <Status>active</Status>
        </VpcSecurityGroupMembership>
      </VpcSecurityGroups>
    </DBCluster>`
}

func subnetGroupsXML(subnets ...string) string {
	var sb strings.Builder
	for _, s := range subnets {
		sb.WriteString(`<Subnet><SubnetIdentifier>` + s + `</SubnetIdentifier></Subnet>`)
	}
	return `<DescribeDBSubnetGroupsResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
  <DescribeDBSubnetGroupsResult>
    <DBSubnetGroups>
      <DBSubnetGroup>
        <Subnets>` + sb.String() + `</Subnets>
      </DBSubnetGroup>
    </DBSubnetGroups>
  </DescribeDBSubnetGroupsResult>
</DescribeDBSubnetGroupsResponse>`
}

func awsErrorXML(code, message string) string {
	return `<ErrorResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
  <Error><Type>Sender</Type><Code>` + code + `</Code><Message>` + message + `</Message></Error>
</ErrorResponse>`
}
