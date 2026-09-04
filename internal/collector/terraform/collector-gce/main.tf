# The DBGorilla collector on Google Cloud: a single-instance regional managed
# instance group running the collector container on Container-Optimized OS,
# deployed by Infrastructure Manager (or plain Terraform).
#
# template-version: v1.1
#
# This file is published, never embedded in the CLI. Secrets arrive as
# sensitive input variables and are stored in Secret Manager; the instance
# fetches them at boot with its own service account, so they never appear in
# instance metadata. Infrastructure Manager retains input values on the
# deployment resource and in its Terraform state, readable by principals
# holding config.* read roles.
#
# Naming contract with the CLI (a change is a version bump): every resource is
# named by the local part of var.runtime_service_account, which the CLI sets to
# the deployment name.
#
# The instance has no public IP. Image pulls and the collector's connection to
# DBGorilla need egress from the VPC (Cloud NAT, or an equivalent route).

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
  }
}

locals {
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

# Read-only monitoring roles, the connect and IAM-login roles of both database
# services, and log writing for `dbg collector logs`.
resource "google_project_iam_member" "collector" {
  for_each = toset([
    "roles/monitoring.viewer",
    "roles/cloudsql.viewer",
    "roles/cloudsql.client",
    "roles/cloudsql.instanceUser",
    "roles/alloydb.viewer",
    "roles/alloydb.client",
    "roles/alloydb.databaseUser",
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

# Boot: fetch both secrets with the VM's own token (retrying while IAM
# bindings propagate), materialize the config from metadata, run the
# container. Secrets reach docker by variable name, never on a command line.
locals {
  startup_script = <<-EOT
    #!/bin/bash
    set -euo pipefail
    retry() {
      local attempts=$1
      shift
      for ((i = 1; i <= attempts; i++)); do
        "$@" && return 0
        sleep 10
      done
      return 1
    }
    metadata() {
      curl -sf -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/$1"
    }
    access_token() {
      metadata instance/service-accounts/default/token \
        | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'
    }
    secret() {
      printf 'Authorization: Bearer %s' "$(access_token)" | curl -sf -H @- \
        "https://secretmanager.googleapis.com/v1/projects/${local.project}/secrets/$1/versions/latest:access" \
        | python3 -c 'import json,sys,base64; print(base64.b64decode(json.load(sys.stdin)["payload"]["data"]).decode())'
    }
    DBG_SERVER_SECRET=$(retry 30 secret "${local.name}-server-secret")
    DBG_DB_PASSWORD=$(retry 30 secret "${local.name}-db-password")
    export DBG_SERVER_SECRET DBG_DB_PASSWORD
    mkdir -p /var/lib/dbgorilla
    retry 30 metadata instance/attributes/collector-config | base64 -d > /var/lib/dbgorilla/collector.toml
    docker run -d --name dbg-collector --restart=always --network=host \
      -v /var/lib/dbgorilla/collector.toml:/etc/dbgorilla/collector.toml:ro \
      -e DBG_SERVER_SECRET \
      -e DBG_DB_PASSWORD \
      "${var.collector_image}" --config-file /etc/dbgorilla/collector.toml
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
    network    = var.network
    subnetwork = var.subnetwork == "" ? null : var.subnetwork
  }

  service_account {
    email  = google_service_account.collector.email
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  metadata = {
    startup-script          = local.startup_script
    collector-config        = var.collector_config
    google-logging-enabled  = "true"
    cos-update-strategy     = "update_enabled"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_compute_region_instance_group_manager" "collector" {
  name               = local.name
  region             = var.region
  base_instance_name = local.name
  target_size        = 1

  # A zonal capacity stockout must not brick the deploy. The default EVEN
  # shape pins the single instance to its assigned zone and recreates it there
  # forever (observed live 2026-09-03: us-central1-f out of e2-small, the
  # group never converged and no CLI lever could move it). ANY lets a recreate
  # land in whichever zone has capacity.
  distribution_policy_target_shape = "ANY"

  version {
    instance_template = google_compute_instance_template.collector.id
  }

  update_policy {
    type                  = "PROACTIVE"
    minimal_action        = "REPLACE"
    max_surge_fixed       = 0
    max_unavailable_fixed = 3
    replacement_method    = "RECREATE"
  }

  # The instance reads its secrets at boot; do not start it before it may.
  depends_on = [
    google_project_iam_member.collector,
    google_secret_manager_secret_version.server_secret,
    google_secret_manager_secret_version.db_password,
    google_secret_manager_secret_iam_member.server_secret,
    google_secret_manager_secret_iam_member.db_password,
  ]
}

output "instance_group" {
  value = google_compute_region_instance_group_manager.collector.instance_group
}

output "service_account" {
  value = google_service_account.collector.email
}
