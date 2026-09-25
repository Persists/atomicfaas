terraform {
  required_version = ">= 1.3"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

provider "google" {
  project = var.project_id
  zone    = var.zone
}

variable "project_id" { type = string }

variable "zone" {
  type    = string
  default = "europe-west1-c"
}

variable "network" {
  type    = string
  default = "default"
}

# One machine per variant; the load generator, the mock and the kills all run on it.
variable "variants" {
  type    = set(string)
  default = ["atomic", "saga", "vanilla"]
}

variable "machine_type" {
  type    = string
  default = "e2-standard-4"
}

resource "google_compute_instance" "vm" {
  for_each     = var.variants
  name         = "afaas-e2-${each.key}"
  machine_type = var.machine_type
  zone         = var.zone

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = 50
      type  = "pd-balanced"
    }
  }

  network_interface {
    network = var.network
    access_config {}
  }
}

output "ips" {
  value = { for k, v in google_compute_instance.vm : k => v.network_interface[0].network_ip }
}
