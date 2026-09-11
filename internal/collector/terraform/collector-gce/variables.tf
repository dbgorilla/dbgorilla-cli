# The template's input contract: one variable per CLI-rendered input
# (gcpInputKeys). Changing this set is a template-version bump.

variable "collector_config" {
  description = "Base64-encoded collector.toml. Contains no secrets, only $${ENV} references."
  type        = string
}

variable "collector_image" {
  description = "Collector container image (digest-pinned by the CLI)."
  type        = string
}

variable "db_password" {
  description = "Database password for password-auth targets; empty when every target uses IAM auth."
  type        = string
  sensitive   = true
  default     = ""
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

variable "server_secret" {
  description = "The collector's DBGorilla identity secret."
  type        = string
  sensitive   = true
}

variable "instaclustr_api_key" {
  description = "Read-only Instaclustr provisioning API key for node discovery; empty when no Instaclustr source is monitored."
  type        = string
  sensitive   = true
  default     = ""
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
