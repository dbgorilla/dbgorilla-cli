# The DBGorilla collector on Google Cloud: a single-instance regional managed
# instance group running the collector container on Container-Optimized OS,
# deployed by Infrastructure Manager (or plain Terraform — nothing here is
# IM-specific).
#
# template-version: v1.0
#
# This file is published, never embedded in the CLI: what a customer reviews
# at the published address is exactly what their project deploys. Secrets
# arrive as sensitive input variables and land ONLY in Secret Manager; the
# instance fetches them at boot with its own service account, so they never
# appear in instance metadata (which any project viewer can read).
#
# Naming contract with the CLI (do not change without a version bump): the
# deployment's resources — the service account, the secrets, the MIG — are all
# named by the local part of var.runtime_service_account, which the CLI sets
# to the Infrastructure Manager deployment name.

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
  }
}

locals {
  # The CLI names the runtime service account "<deployment>@<project>.iam…",
  # so its local part IS the deployment name.
  name    = split("@", var.runtime_service_account)[0]
  project = split(".", split("@", var.runtime_service_account)[1])[0]
}

provider "google" {
  project = local.project
  region  = var.region
}

# --- identity ---------------------------------------------------------------

resource "google_service_account" "collector" {
  account_id   = local.name
  display_name = "DBGorilla collector"
}

# Read-only monitoring roles plus the connect roles both database services
# gate their traffic on. Log writing lets the container's output reach Cloud
# Logging for `dbg collector logs`.
resource "google_project_iam_member" "collector" {
  for_each = toset([
    "roles/monitoring.viewer",
    "roles/cloudsql.viewer",
    "roles/cloudsql.client",
    "roles/alloydb.viewer",
    "roles/alloydb.client",
    "roles/logging.logWriter",
  ])
  project = local.project
  role    = each.value
  member  = "serviceAccount:${google_service_account.collector.email}"
}

# --- secrets ----------------------------------------------------------------

resource "google_secret_manager_secret" "server_secret" {
  secret_id = "${local.name}-server-secret"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "server_secret" {
  secret      = google_secret_manager_secret.server_secret.id
  secret_data = var.server_secret
}

resource "google_secret_manager_secret" "db_password" {
  secret_id = "${local.name}-db-password"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "db_password" {
  secret      = google_secret_manager_secret.db_password.id
  secret_data = var.db_password == "" ? "unused" : var.db_password
}

resource "google_secret_manager_secret_iam_member" "server_secret" {
  secret_id = google_secret_manager_secret.server_secret.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.collector.email}"
}

resource "google_secret_manager_secret_iam_member" "db_password" {
  secret_id = google_secret_manager_secret.db_password.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.collector.email}"
}

# --- the instance -----------------------------------------------------------

# Boot flow: fetch both secrets with the VM's own token, then run the
# collector container with them in its environment. The config arrives
# base64-encoded in metadata (it holds no secrets) and is materialized to a
# file the container reads.
locals {
  startup_script = <<-EOT
    #!/bin/bash
    set -euo pipefail
    token=$(curl -s -H "Metadata-Flavor: Google" \
      "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
    fetch_secret() {
      curl -s -H "Authorization: Bearer $token" \
        "https://secretmanager.googleapis.com/v1/projects/${local.project}/secrets/$1/versions/latest:access" \
        | python3 -c 'import json,sys,base64; print(base64.b64decode(json.load(sys.stdin)["payload"]["data"]).decode())'
    }
    DBG_SERVER_SECRET=$(fetch_secret "${local.name}-server-secret")
    DBG_DB_PASSWORD=$(fetch_secret "${local.name}-db-password")
    mkdir -p /var/lib/dbgorilla
    curl -s -H "Metadata-Flavor: Google" \
      "http://metadata.google.internal/computeMetadata/v1/instance/attributes/collector-config" \
      | base64 -d > /var/lib/dbgorilla/collector.toml
    docker run -d --name dbg-collector --restart=always --network=host \
      -v /var/lib/dbgorilla/collector.toml:/etc/dbgorilla/collector.toml:ro \
      -e DBG_SERVER_SECRET="$DBG_SERVER_SECRET" \
      -e DBG_DB_PASSWORD="$DBG_DB_PASSWORD" \
      "${var.collector_image}" --config /etc/dbgorilla/collector.toml
  EOT
}

resource "google_compute_instance_template" "collector" {
  name_prefix  = "${local.name}-"
  machine_type = "e2-small"
  region       = var.region

  disk {
    source_image = "projects/cos-cloud/global/images/family/cos-stable"
    auto_delete  = true
    boot         = true
    disk_size_gb = 20
  }

  network_interface {
    network = var.network
    # Private-IP only: database access is over PSA/PSC, and egress to Google
    # APIs rides Private Google Access. Public IPs are commonly banned by org
    # policy (constraints/compute.vmExternalIpAccess).
  }

  service_account {
    email  = google_service_account.collector.email
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  metadata = {
    startup-script   = local.startup_script
    collector-config = var.collector_config
    # COS: keep the OS current between instance recreations.
    cos-update-strategy = "update_enabled"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_compute_region_instance_group_manager" "collector" {
  # GcpMigFor's naming contract: the MIG carries the deployment name.
  name               = local.name
  region             = var.region
  base_instance_name = local.name
  target_size        = 1

  version {
    instance_template = google_compute_instance_template.collector.id
  }

  update_policy {
    type                    = "PROACTIVE"
    minimal_action          = "REPLACE"
    max_surge_fixed         = 0
    max_unavailable_fixed   = 3
    replacement_method      = "RECREATE"
  }
}

output "instance_group" {
  value = google_compute_region_instance_group_manager.collector.instance_group
}

output "service_account" {
  value = google_service_account.collector.email
}
