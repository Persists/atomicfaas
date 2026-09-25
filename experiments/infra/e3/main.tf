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
  default = "europe-west3-a"
}

variable "network" {
  type    = string
  default = "default"
}

locals {
  image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
  machines = {
    "afaas-e3-cluster" = { type = "e2-standard-8", disk = 50, tags = ["afaas-cluster"] }
    "afaas-e3-driver"  = { type = "e2-standard-4", disk = 30, tags = ["afaas-driver"] }
  }
}

resource "google_compute_instance" "vm" {
  for_each                  = local.machines
  name                      = each.key
  machine_type              = each.value.type
  zone                      = var.zone
  tags                      = each.value.tags
  allow_stopping_for_update = true

  boot_disk {
    initialize_params {
      image = local.image
      size  = each.value.disk
      type  = "pd-balanced"
    }
  }

  network_interface {
    network = var.network
    access_config {}
  }
}

# Kourier is a NodePort on the kubeadm clusters, so the driver reaches the ingress on 31080, not 80.
resource "google_compute_firewall" "driver_to_cluster" {
  name    = "afaas-e3-driver-to-cluster"
  network = var.network
  allow {
    protocol = "tcp"
    ports    = ["31080"]
  }
  source_tags = ["afaas-driver"]
  target_tags = ["afaas-cluster"]
}

resource "google_compute_firewall" "cluster_to_driver" {
  name    = "afaas-e3-cluster-to-driver"
  network = var.network
  allow {
    protocol = "tcp"
    ports    = ["8090"]
  }
  source_tags = ["afaas-cluster"]
  target_tags = ["afaas-driver"]
}

output "ips" {
  value = { for k, v in google_compute_instance.vm : k => v.network_interface[0].network_ip }
}
