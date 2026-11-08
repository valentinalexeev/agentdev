# kind only (#100, #106, was 09-authentik.sh): akadmin in agentdev-admins (add only), then a check that a member
# added by a person survives a blueprint re-apply. Runs in the authentik worker with `ak shell`; no token is created.
import sys, time
from rest_framework.test import APIClient
from authentik.core.models import Group, User
from authentik.blueprints.models import BlueprintInstance
from authentik.blueprints.v1.importer import Importer
for _ in range(60):
    g = Group.objects.filter(name="agentdev-admins").first()
    if g: break
    time.sleep(5)
else: sys.exit("group agentdev-admins missing: blueprint not applied")
admin = User.objects.get(username="akadmin")
c = APIClient(); c.force_authenticate(admin)
api = "/api/v3/core/groups/%s/" % g.pk
def call(op, pk):
    r = c.post(api + op + "/", {"pk": pk}, format="json")
    if r.status_code != 204: sys.exit("group %s: HTTP %s" % (op, r.status_code))
def add(pk): call("add_user", pk)
def members(): g.refresh_from_db(); return set(g.users.values_list("username", flat=True))
test = None
try:
    add(admin.pk)
    print("akadmin in agentdev-admins")
    test, _ = User.objects.get_or_create(username="agentdev-09-membership-test", defaults={"name": "09 test", "is_active": False})
    add(test.pk)
    # the old kind blueprint (before #103 review) set the members: forget its instance, its file is gone
    BlueprintInstance.objects.filter(path__endswith="authentik-blueprint-local-admin.yaml").delete()
    applied = 0
    for b in BlueprintInstance.objects.filter(path__contains="authentik-blueprint-"):
        imp = Importer.from_string(b.retrieve(), b.context)
        ok, logs = imp.validate()
        if not ok or not imp.apply(): sys.exit("blueprint %s did not apply" % b.path)
        applied += 1
    if not applied: sys.exit("no agentdev blueprint found to re-apply")
    m = members()
    if not {"akadmin", test.username} <= m: sys.exit("re-apply removed members: %s" % sorted(m))
    print("check: members kept after re-applying %d blueprint(s)" % applied)
finally:
    if test:
        call("remove_user", test.pk)
        test.delete()
