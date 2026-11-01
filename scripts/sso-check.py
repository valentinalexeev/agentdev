#!/usr/bin/env python3
"""Sign-in check of the company SSO source on kind (#122), run by 09-authentik.sh.

Drives real browser sign-ins through authentik and the local OIDC stand-in (manifests/kind/sso-standin.yaml), then
reads the result inside the authentik worker. Checks the owner rule of #103 and the default of #122:
  1. A person in the directory group is added to agentdev-admins-sso and gets agentdev-admins in the token claim.
     forge_login, forge_login_github and forge_login_gitea come from their custom attributes, normalised. Other directory groups do not reach authentik.
  2. A member added by hand (directly to agentdev-admins) and akadmin are kept.
  3. When the person leaves the directory group, the next sign-in removes them from agentdev-admins-sso only.
     A bad or absent custom attribute clears forge_login / forge_login_github / forge_login_gitea (fail closed).
  4. authentik logged each membership change.
The test user is deleted at the end. Standard library only; no credential is printed.
"""
import argparse
import html
import json
import os
import re
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from http.cookiejar import CookieJar

TEST_USER = "sso-check-alice"
FOREIGN_USER = "sso-check-mallory"


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **kw):
        return None


class Browser:
    def __init__(self):
        self.jar = CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar), NoRedirect,
            # authentik in kind has a self-signed certificate; its session cookie is Secure, so https is required
            urllib.request.HTTPSHandler(context=ssl._create_unverified_context()))

    def req(self, url, data=None, headers=None):
        body = None
        h = dict(headers or {})
        if isinstance(data, dict):
            body = urllib.parse.urlencode(data).encode()
            h.setdefault("Content-Type", "application/x-www-form-urlencoded")
        elif isinstance(data, str):
            body = data.encode()
            h.setdefault("Content-Type", "application/json")
        r = urllib.request.Request(url, data=body, headers=h, method="POST" if body is not None else "GET")
        for attempt in range(4):
            try:
                resp = self.op.open(r, timeout=30)
                break
            except urllib.error.HTTPError as e:
                resp = e
                break
            except (urllib.error.URLError, ConnectionError, TimeoutError):
                # kubectl port-forward drops a connection now and then: retry, then give up
                if attempt == 3:
                    raise
                time.sleep(2)
        return resp.status, resp.headers, resp.read().decode("utf-8", "replace")


def follow(b, url, base):
    """Follow redirects; return the final URL and body."""
    for _ in range(15):
        st, hd, body = b.req(url)
        if st in (301, 302, 303, 307) and hd.get("Location"):
            url = urllib.parse.urljoin(url, hd["Location"])
            continue
        return url, st, body
    raise RuntimeError("too many redirects")


def run_flow(b, base, flow_url):
    """Run an authentik flow through the executor API until it leaves the flow."""
    u = urllib.parse.urlparse(flow_url)
    m = re.match(r"^/if/flow/([^/]+)/", u.path)
    if not m:
        raise RuntimeError(f"not a flow URL: {u.path}")
    q = urllib.parse.parse_qs(u.query).get("query", [""])[0]
    api = f"{base}/api/v3/flows/executor/{m.group(1)}/?" + urllib.parse.urlencode({"query": q})
    js = {"Accept": "application/json"}
    st, hd, body = b.req(api, headers=js)
    for _ in range(20):
        if st in (301, 302, 303) and hd.get("Location"):
            # a redirect to the executor means: the next stage
            loc = urllib.parse.urljoin(api, hd["Location"])
            if "/api/v3/flows/executor/" not in loc:
                return loc
            st, hd, body = b.req(loc, headers=js)
            continue
        try:
            ch = json.loads(body)
        except ValueError:
            raise RuntimeError(f"flow executor answered HTTP {st} without JSON") from None
        comp = ch.get("component")
        if comp == "xak-flow-redirect":
            return ch.get("to")
        if comp in ("ak-stage-flow-error", "ak-stage-access-denied") or ch.get("response_errors"):
            raise RuntimeError(f"flow error: {ch.get('error_message') or ch.get('error') or ch.get('response_errors')}")
        st, hd, body = b.req(api, data=json.dumps({"component": comp}), headers=js)
    raise RuntimeError("flow did not finish")


def sign_in(base, idp, claims, user=TEST_USER):
    b = Browser()
    url, st, body = follow(b, f"{base}/source/oauth/login/agentdev-sso/", base)
    if not url.startswith(idp):
        raise RuntimeError(f"source did not redirect to the stand-in (got {urllib.parse.urlparse(url).netloc})")
    st, hd, _ = b.req(url, data={"username": user, "claims": json.dumps(claims)})
    loc = hd.get("Location", "")
    if "code=" not in loc:
        raise RuntimeError("stand-in gave no code")
    url, st, body = follow(b, loc, base)
    run_flow(b, base, url)


def ak_shell(ctx, ns, code):
    pod = subprocess.check_output(
        ["kubectl", "--context", ctx, "-n", ns, "get", "pod", "-l",
         "app.kubernetes.io/name=authentik,app.kubernetes.io/component=worker",
         "--field-selector=status.phase=Running", "-o", "jsonpath={.items[0].metadata.name}"], text=True)
    out = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "exec", "-i", pod, "--", "ak", "shell"],
                         input=code, capture_output=True, text=True, timeout=180)
    for line in out.stdout.splitlines():
        if line.startswith("RESULT="):
            return json.loads(line[len("RESULT="):])
    raise RuntimeError("ak shell gave no result: " + (out.stderr.strip().splitlines() or ["?"])[-1])


STATE = """
import json
from authentik.core.models import Group, User
from authentik.events.models import Event
from authentik.providers.oauth2.models import ScopeMapping
u = User.objects.filter(username=%(user)r).first()
sso = Group.objects.get(name="agentdev-admins-sso")
adm = Group.objects.get(name="agentdev-admins")
claims = ScopeMapping.objects.get(name="agentdev: forge login").evaluate(user=u, request=None, provider=None, token=None) if u else {}
ev = sorted(Event.objects.filter(action="custom_agentdev_admin_sync", created__gte=%(since)r, context__username=%(user)r).values_list("context__change", flat=True))
print("RESULT=" + json.dumps({
  "exists": bool(u),
  "in_sso": bool(u) and sso.users.filter(pk=u.pk).exists(),
  "in_admins_direct": bool(u) and adm.users.filter(pk=u.pk).exists(),
  "forge_login": (u.attributes.get("forge_login") if u else None),
  "forge_login_github": (u.attributes.get("forge_login_github") if u else None),
  "forge_login_gitea": (u.attributes.get("forge_login_gitea") if u else None),
  "claims": claims,
  "akadmin_admin": adm.users.filter(username="akadmin").exists(),
  "groups_from_source": sorted(Group.objects.filter(groupsourceconnection__source__slug="agentdev-sso").values_list("name", flat=True)),
  "events": ev,
}))
"""

REAPPLY = """
import json
from authentik.blueprints.models import BlueprintInstance
from authentik.blueprints.v1.importer import Importer
ok = []
for bi in BlueprintInstance.objects.filter(path__startswith="mounted/cm-authentik-blueprints/"):
    ok.append(Importer.from_string(bi.retrieve(), bi.context).apply())
print("RESULT=" + json.dumps(ok))
"""

EXISTS = """
import json
from authentik.core.models import User
print("RESULT=" + json.dumps(User.objects.filter(username=%(user)r).exists()))
"""

HAND_ADD = """
import json
from authentik.core.models import Group, User
Group.objects.get(name="agentdev-admins").users.add(User.objects.get(username=%(user)r))
print("RESULT=true")
"""

CLEANUP = """
import json
from authentik.core.models import User
n = User.objects.filter(username__in=[%(user)r, %(foreign)r]).delete()[0]
print("RESULT=" + json.dumps(n))
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--context", required=True)
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--admin-group", required=True)
    ap.add_argument("--forge-attr", required=True, help="fallback attribute (forge_login)")
    ap.add_argument("--forge-attr-github", required=True)
    ap.add_argument("--forge-attr-gitea", required=True)
    ap.add_argument("--domain", required=True, help="an allowed company domain")
    a = ap.parse_args()
    ctx, ns = a.context, a.namespace
    ak_port = free_port()
    pfs = [subprocess.Popen(["kubectl", "--context", ctx, "-n", ns, "port-forward", "svc/authentik-server",
                             f"{ak_port}:443"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL),
           # the authorize URL in the blueprint secret is http://localhost:18080 (browser side)
           subprocess.Popen(["kubectl", "--context", ctx, "-n", ns, "port-forward", "svc/sso-standin", "18080:8080"],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)]
    base, idp = f"https://localhost:{ak_port}", "http://localhost:18080/"
    fails = []

    def check(cond, msg):
        print(("  ok   " if cond else "  FAIL ") + msg)
        if not cond:
            fails.append(msg)

    try:
        for _ in range(60):
            try:
                urllib.request.urlopen(f"{base}/-/health/ready/", timeout=3, context=ssl._create_unverified_context())
                urllib.request.urlopen(f"{idp}default/.well-known/openid-configuration", timeout=3)
                break
            except Exception:
                time.sleep(2)
        since = ak_shell(ctx, ns, "import json\nfrom django.utils import timezone\n"
                         "print('RESULT=' + json.dumps(timezone.now().isoformat()))")
        ak_shell(ctx, ns, CLEANUP % {"user": TEST_USER, "foreign": FOREIGN_USER})
        claims = {"preferred_username": TEST_USER, "email": f"{TEST_USER}@{a.domain}", "name": "SSO check",
                  "groups": ["engineering", a.admin_group], a.forge_attr: " Alice-1 ",
                  a.forge_attr_github: "Alice-GH", a.forge_attr_gitea: "alice-gitea"}

        print("1. sign-in as a member of the directory group")
        sign_in(base, idp, claims)
        s = ak_shell(ctx, ns, STATE % {"user": TEST_USER, "since": since})
        check(s["exists"], "user created by the source")
        check(s["in_sso"], "added to agentdev-admins-sso")
        check(not s["in_admins_direct"], "not written into agentdev-admins directly")
        check("agentdev-admins" in s["claims"].get("groups", []), "token claim groups has agentdev-admins (inherited)")
        check(s["forge_login"] == "alice-1" and s["claims"].get("forge_login") == "alice-1",
              "forge_login from the custom attribute, normalised")
        check(s["forge_login_github"] == "alice-gh" and s["claims"].get("forge_login_github") == "alice-gh"
              and s["forge_login_gitea"] == "alice-gitea" and s["claims"].get("forge_login_gitea") == "alice-gitea",
              "forge_login_github and forge_login_gitea from their custom attributes")
        check(s["groups_from_source"] == ["agentdev-admins-sso"], "no other directory group reached authentik")

        print("2. a member added by hand is kept when the blueprints are re-applied")
        ak_shell(ctx, ns, HAND_ADD % {"user": TEST_USER})
        applied = ak_shell(ctx, ns, REAPPLY)
        s = ak_shell(ctx, ns, STATE % {"user": TEST_USER, "since": since})
        check(len(applied) >= 2 and all(applied), f"all agentdev blueprints re-applied ({len(applied)})")
        check(s["in_admins_direct"], "hand-added member still in agentdev-admins after the re-apply")
        check(s["akadmin_admin"], "akadmin still in agentdev-admins after the re-apply")

        print("3. sign-in after leaving the directory group, with a bad custom attribute")
        claims.update({"groups": ["engineering"], a.forge_attr: "bad login!", a.forge_attr_github: "bad/gh"})
        del claims[a.forge_attr_gitea]
        sign_in(base, idp, claims)
        s = ak_shell(ctx, ns, STATE % {"user": TEST_USER, "since": since})
        check(not s["in_sso"], "removed from agentdev-admins-sso")
        check(s["in_admins_direct"], "hand-added membership of agentdev-admins kept")
        check(s["akadmin_admin"], "akadmin still in agentdev-admins")
        check(s["forge_login"] == "" and "forge_login" not in s["claims"], "bad attribute clears forge_login")
        check(s["forge_login_github"] == "" and "forge_login_github" not in s["claims"]
              and s["forge_login_gitea"] == "" and "forge_login_gitea" not in s["claims"],
              "bad or absent per-forge attribute clears forge_login_github / forge_login_gitea")

        print("4. company domain")
        for label, extra in [("foreign e-mail domain", {"email": f"{FOREIGN_USER}@evil.example"}),
                             ("foreign Google hd claim", {"email": f"{FOREIGN_USER}@{a.domain}", "hd": "evil.example"})]:
            refused = False
            try:
                sign_in(base, idp, {"preferred_username": FOREIGN_USER, "groups": [a.admin_group],
                                    a.forge_attr: "mallory", **extra}, user=FOREIGN_USER)
            except RuntimeError:
                refused = True
            check(refused and not ak_shell(ctx, ns, EXISTS % {"user": FOREIGN_USER}),
                  f"{label}: sign-in refused, no user created")

        print("5. audit")
        check(s["events"] == ["add", "remove"], f"one audit event per change: {s['events']}")
    finally:
        try:
            if os.environ.get("SSO_KEEP") != "1":
                ak_shell(ctx, ns, CLEANUP % {"user": TEST_USER, "foreign": FOREIGN_USER})
        except Exception as exc:  # noqa: BLE001
            print(f"  ! cleanup failed: {exc}")
        for p in pfs:
            p.terminate()
    if fails:
        print(f"{len(fails)} check(s) failed", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as exc:
        print(f"SSO check error: {html.unescape(str(exc))}", file=sys.stderr)
        sys.exit(1)
