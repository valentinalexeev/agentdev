# agentdev deployment: GCP / GKE (template)

Copy this directory into a **new, private repository**: it becomes the deployment of *one* agentdev
instance. The product (modules, playbook, images) stays in `valentinalexeev/agentdev` and is consumed here
**by release tag** (strict SemVer), never by branch.

```
terraform/main.tf                root module → agentdev//infra/terraform?ref=vX.Y.Z
terraform/instance.tfvars        your project, region, zone, billing account, this repo's name
ansible/instance.yml             your project, zone, image_tag (= the same vX.Y.Z), reviewer, agents
.github/workflows/deploy.yml     PR: plan + check   main: apply + deploy   (Workload Identity, no keys)
AGENTS.md / CLAUDE.md            how an agent operates this instance
```

## First time
1. Copy this directory to the new repo; rename `*.example` files and fill them in.
2. Follow agentdev `AGENTS.md` steps 1 to 3 (logins, **you** run the bootstrap apply: project, billing, state bucket).
3. `cd terraform && terraform init -backend-config="bucket=<project>-tfstate" && terraform plan -var-file=instance.tfvars`
   → you apply once locally (creates the Workload Identity setup the CI needs).
4. Set repository variables from `terraform output github_actions_auth`: `GCP_WIF_PROVIDER`, `GCP_SERVICE_ACCOUNT`, and `GCP_PROJECT`.
5. Fill Secret Manager values (agentdev `infra/ansible/README.md`).
6. Protect `main`: require a PR, 1 approval, and the `deploy` checks. From now on every change is a PR here.

## Upgrading agentdev
Change `ref=vX.Y.Z` in `terraform/main.tf` **and** `image_tag` in `ansible/instance.yml` together, in one PR.
Read the agentdev `CHANGELOG.md` first; a MAJOR bump (MINOR while `0.x`) can require input changes.
