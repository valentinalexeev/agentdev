# Example inputs for one agentdev instance. Copy into your deployment repo (see examples/deployment/gcp-gke),
# never commit real values here.
project_id         = "my-agentdev"
region             = "europe-west4"
zone               = "europe-west4-a"
billing_account_id = "000000-000000-000000"
github_repository  = "my-org/my-agentdev-deploy" # the deployment repo, not this one

# Optional (defaults shown in variables.tf)
# budget_amount_usd    = 150
# budget_alert_emails  = ["ops@example.com"]
# agent_pool_max_nodes = 3
# agent_pool_spot      = true
# system_disk_size_gb  = 30              # node boot disks (#170); changing one replaces the pool
# system_disk_type     = "pd-standard"   # default; "pd-balanced" keeps a pre-#170 pool
# agent_disk_size_gb   = 50
# agent_disk_type      = "pd-standard"
# system_pool_spot     = false   # #171: Spot for the system pool; short platform gaps on preemption
# gateway_enabled           = null    # #208: null = on only with public_hosts; true keeps the load balancer anyway
# enable_managed_prometheus = false   # #208: agentdev does not use it
# enable_filestore_csi      = false   # #208: agentdev uses persistent disks only
# monitoring_components     = ["SYSTEM_COMPONENTS"]   # #208: null = the GKE default list (adds kube-state-metrics)
# public_hosts         = ["agentdev.example.com"]   # public HTTPS entry point (#120); DNS A record -> webhook_ip
# cloud_armor          = false
# labels               = { environment = "prod" }   # #184: service = "agentdev" is added; dev, stage or prod
