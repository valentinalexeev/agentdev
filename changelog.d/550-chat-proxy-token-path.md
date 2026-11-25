---
issue: 550
type: security
section: Security
status: done
breaking: false
title: The chat Job policy pins the path of the projected claude-proxy token to token
components: [api]
pr: 561
---
## Summary

The chat Job ValidatingAdmissionPolicy (`manifests/api.yaml.tmpl`) now requires `path: token` for the projected
`claude-proxy` token, the path that the operator and the chat pods already use. A chat Job that projects the token under
another file name is refused. Re-run: the playbook or `./run.sh 12`.
