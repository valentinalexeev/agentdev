# Break-glass admin (docs/admin.md, "Keep a break-glass admin"), run by the Job authentik-breakglass-<hash> with
# `ak shell` in the authentik image (#382, was a kubectl exec step). akadmin is created by authentik's bootstrap
# blueprint; here it is added to agentdev-admins with the REST API in-process: add_user only adds, a second run is a
# no-op, no member is ever removed and no token is created or printed. A missing akadmin only gives a warning line;
# the Job fails only when the group never appears or no active superuser exists (nobody could log in).
# #566: the worker applies the blueprint shortly after its start; poll every second (same 10 min bound as before) and
# print the wait, so a slow run shows whether the time went into `ak shell` start-up or into the blueprint.
import sys, time
t0 = time.monotonic()
from rest_framework.test import APIClient
from authentik.core.models import Group, User
for _ in range(600):
    g = Group.objects.filter(name="agentdev-admins").first()
    if g: break
    time.sleep(1)
else: sys.exit("group agentdev-admins missing: blueprint not applied")
print("GROUP_WAIT_SECONDS=%d" % (time.monotonic() - t0))
admin = User.objects.filter(username="akadmin").first()
if admin is None:
    print("BREAKGLASS=missing WARNING: akadmin not found (renamed or deleted), nothing recreated; see docs/admin.md")
elif g.users.filter(pk=admin.pk).exists():
    print("BREAKGLASS=present")
else:
    c = APIClient(); c.force_authenticate(admin)
    r = c.post("/api/v3/core/groups/%s/add_user/" % g.pk, {"pk": admin.pk}, format="json")
    if r.status_code != 204: sys.exit("add_user: HTTP %s" % r.status_code)
    print("BREAKGLASS=added")
n = sum(1 for u in User.objects.filter(is_active=True) if u.is_superuser)
print("SUPERUSERS=%d" % n)
if n == 0: sys.exit("no active superuser: nobody could log in")
