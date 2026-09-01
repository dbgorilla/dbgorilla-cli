# The template's input contract — one variable per CLI-rendered input
# (gcpparams.go's gcpInputKeys). Adding, removing, or reinterpreting one of
# these is a template-version bump; TestGcpTemplateContract pins the two
# against each other.

variable "collector_config" {
  description = "Base64-encoded collector.toml. Contains no secrets — only $${ENV} references."
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
  description = "VPC self-link the collector instance joins."
  type        = string
}

variable "region" {
  description = "Region for the instance group (the databases' region)."
  type        = string
}

variable "runtime_service_account" {
  description = "Email of the service account this template creates for the collector VM. Its local part names the deployment's resources (the CLI's naming contract)."
  type        = string
}

variable "server_secret" {
  description = "The collector's DBGorilla identity secret."
  type        = string
  sensitive   = true
}
