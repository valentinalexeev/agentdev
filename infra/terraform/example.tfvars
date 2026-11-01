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
# public_hosts         = ["agentdev.example.com"]   # public HTTPS entry point (#120); DNS A record -> webhook_ip
# cloud_armor          = false
