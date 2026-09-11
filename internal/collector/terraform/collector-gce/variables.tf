# The template's input contract: one variable per CLI-rendered input
# (gcpInputKeys). Changing this set is a template-version bump. No variable
# carries a credential — the CLI writes the secrets to Secret Manager itself,
# so nothing sensitive enters the deployment's input values or state.

variable "collector_config" {
  description = "Base64-encoded collector.toml. Contains no secrets, only $${ENV} references."
  type        = string
}

variable "collector_image" {
  description = "Collector container image (digest-pinned by the CLI)."
  type        = string
}

variable "database_roles" {
  description = "Grant the collector's service account the Cloud SQL / AlloyDB viewer, client and IAM-login roles. Off for sources that are not Google-managed databases (Instaclustr), where the project-wide grants would be pure excess."
  type        = bool
  default     = true
}

variable "network" {
  description = "VPC the collector instance joins (projects/<project>/global/networks/<name>)."
  type        = string
}

variable "subnetwork" {
  description = "Subnetwork in the region for the collector instance. Required on a custom-mode VPC; empty on an auto-mode VPC."
  type        = string
  default     = ""
}

variable "region" {
  description = "Region for the instance group (the databases' region)."
  type        = string
}

variable "runtime_service_account" {
  description = "Email of the service account this template creates for the collector VM. Its local part names the deployment's resources."
  type        = string
}

variable "stable_egress" {
  description = "Create a dedicated subnetwork routed through a Cloud NAT with a reserved static address, so the collector's outbound IP never changes (IP-allowlist-gated databases require it)."
  type        = bool
  default     = false
}

variable "nat_subnet_cidr" {
  description = "Unused CIDR in the VPC for the stable-egress subnetwork, e.g. 10.10.200.0/28. Required when stable_egress is true."
  type        = string
  default     = ""
}
