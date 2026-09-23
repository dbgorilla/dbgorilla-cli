package collector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// The collector's credentials on the gcp target live in Secret Manager and
// ONLY there: the CLI writes them with the operator's own credentials before
// deploying, the template merely grants the collector's service account read
// access by name, and the instance fetches the values at boot. Secret values
// therefore never reach Infrastructure Manager — neither the deployment's
// input values nor its Terraform state, both of which are readable by
// principals holding config.* read roles (Infrastructure Manager has no
// equivalent of CloudFormation's NoEcho).

const secretManagerBase = "https://secretmanager.googleapis.com/v1"

// gcpSecrets is the template's secret naming contract, in order (a change is
// a template-version bump): each secret is "<deployment-name>-<suffix>", the
// boot script fetches every one of them unconditionally, and value reads the
// credential that fills it. This one list drives create, write, and delete,
// so a secret's name and its value can never disagree — a suffix without a
// value, or a value without a suffix, is impossible to express.
var gcpSecrets = []struct {
	suffix string
	value  func(GcpSecretValues) string
}{
	{"server-secret", func(v GcpSecretValues) string { return v.ServerSecret }},
	{"db-password", func(v GcpSecretValues) string { return v.DBPassword }},
	{"instaclustr-api-key", func(v GcpSecretValues) string { return v.InstaclustrKey }},
	{"prometheus-api-key", func(v GcpSecretValues) string { return v.PrometheusKey }},
	{"provisioning-api-key", func(v GcpSecretValues) string { return v.ProvisioningKey }},
}

// gcpSecretPlaceholder stands in for an absent credential (IAM-auth installs
// have no db password; cloud_sql installs no Instaclustr key), so the boot
// script never needs to know which credentials this install carries.
const gcpSecretPlaceholder = "unused"

// GcpSecretValues carries the collector credentials. Empty fields are
// written as the placeholder.
type GcpSecretValues struct {
	ServerSecret   string
	DBPassword     string
	InstaclustrKey string
	// PrometheusKey is the dedicated low-privilege key for the
	// platform-metrics scrape — a separate Instaclustr key kind (the
	// provisioning keys 401 on monitoring surfaces).
	PrometheusKey string
	// ProvisioningKey is the account-wide Instaclustr Provisioning key for
	// fast-fork operations. Opt-in (--fast-fork); empty when absent.
	ProvisioningKey string
}

// GcpSecretIDs lists the Secret Manager secret ids an install writes, in the
// template's naming contract.
func GcpSecretIDs(deploymentName string) []string {
	ids := make([]string, 0, len(gcpSecrets))
	for _, s := range gcpSecrets {
		ids = append(ids, deploymentName+"-"+s.suffix)
	}
	return ids
}

// EnsureGcpSecrets creates the deployment's secrets (tolerating ones that
// already exist) and writes each value as a new version, which becomes
// "latest" atomically — a booting instance never observes a gap. Values
// travel only in request bodies, never in a URL or an error.
func EnsureGcpSecrets(project, deploymentName string, values GcpSecretValues) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	for _, s := range gcpSecrets {
		id := deploymentName + "-" + s.suffix
		value := s.value(values)
		if value == "" {
			value = gcpSecretPlaceholder
		}
		createURL := fmt.Sprintf("%s/projects/%s/secrets?secretId=%s",
			secretManagerBase, url.PathEscape(project), url.QueryEscape(id))
		create := map[string]any{
			"replication": map[string]any{"automatic": map[string]any{}},
			"labels":      map[string]string{"managed-by": "dbgorilla-cli"},
		}
		if err := gcpDo(ctx, cfg, http.MethodPost, createURL, create, nil); err != nil &&
			!errors.Is(err, errGcpConflict) {
			return fmt.Errorf("could not create secret %q: %w", id, err)
		}
		versionURL := fmt.Sprintf("%s/projects/%s/secrets/%s:addVersion",
			secretManagerBase, url.PathEscape(project), url.PathEscape(id))
		payload := map[string]any{
			"payload": map[string]string{"data": base64.StdEncoding.EncodeToString([]byte(value))},
		}
		if err := gcpDo(ctx, cfg, http.MethodPost, versionURL, payload, nil); err != nil {
			return fmt.Errorf("could not write secret %q: %w", id, err)
		}
	}
	return nil
}

// DeleteGcpSecrets removes the deployment's secrets, tolerating ones already
// gone. It is the uninstall/rollback counterpart of EnsureGcpSecrets: the CLI
// owns these secrets, the template only reads them.
func DeleteGcpSecrets(project, deploymentName string) error {
	ctx := context.Background()
	cfg, err := loadGCPConfig(ctx)
	if err != nil {
		return gcpCredsErr(err)
	}
	var errs []error
	for _, id := range GcpSecretIDs(deploymentName) {
		deleteURL := fmt.Sprintf("%s/projects/%s/secrets/%s",
			secretManagerBase, url.PathEscape(project), url.PathEscape(id))
		if err := gcpDo(ctx, cfg, http.MethodDelete, deleteURL, nil, nil); err != nil &&
			!errors.Is(err, errGcpNotFound) {
			errs = append(errs, fmt.Errorf("secret %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
