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

variable "variants" {
  type    = set(string)
  default = ["atomic", "vanilla"]
}

variable "cluster_machine_type" {
  type    = string
  default = "e2-standard-8"
}

# One loadgen process (with its own mock) per variant, so the driver's cores scale with the clusters it drives.
variable "driver_machine_type" {
  type    = string
  default = "e2-standard-8"
}

# Kourier is a NodePort on the kubeadm clusters, so the driver reaches the ingress here, not on 80.
variable "ingress_node_port" {
  type    = number
  default = 31080
}

# One mock per variant on the shared driver, assigned by bringup.sh (8090 atomic, 8091 saga, 8092 vanilla).
variable "mock_port_base" {
  type    = number
  default = 8090
}

locals {
  image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
  # One cluster per variant (isolation is about the function pods) but a single shared driver.
  machines = merge(
    { for v in var.variants :
    "afaas-e4-${v}-cluster" => { type = var.cluster_machine_type, disk = 50, tags = ["afaas-cluster"] } },
    { "afaas-e4-driver" = { type = var.driver_machine_type, disk = 30, tags = ["afaas-driver"] } },
  )
  mock_ports = "${var.mock_port_base}-${var.mock_port_base + 2}"
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

resource "google_compute_firewall" "driver_to_cluster" {
  name    = "afaas-e4-driver-to-cluster"
  network = var.network
  allow {
    protocol = "tcp"
    ports    = [tostring(var.ingress_node_port)]
  }
  source_tags = ["afaas-driver"]
  target_tags = ["afaas-cluster"]
}

resource "google_compute_firewall" "cluster_to_driver" {
  name    = "afaas-e4-cluster-to-driver"
  network = var.network
  allow {
    protocol = "tcp"
    ports    = [local.mock_ports]
  }
  source_tags = ["afaas-cluster"]
  target_tags = ["afaas-driver"]
}

output "ips" {
  value = { for k, v in google_compute_instance.vm : k => v.network_interface[0].network_ip }
}

output "ingress_node_port" { value = var.ingress_node_port }
output "mock_ports" { value = local.mock_ports }
