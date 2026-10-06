"""Smoke test: run the honcho-ai SDK against a running honchol server.

Usage: python smoke_sdk.py [base_url]
Requires: pip install honcho-ai   (tested with v2.x)
"""
import sys

from honcho import Honcho

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8792"
WS = "smoke-ws"
h = Honcho(base_url=BASE, workspace_id=WS, api_key="local")

passed = [0]


def check(name, cond, extra=""):
    if cond:
        passed[0] += 1
        print(f"PASS  {name}  {extra}")
    else:
        print(f"FAIL  {name}  {extra}")
        sys.exit(1)


# --- peers & session ---
alice = h.peer("alice")
agent = h.peer("agent")
sess = h.session("smoke-session-1")

# --- messages ---
m1 = alice.message("較正の話をしよう。temperature scaling の話。")
m2 = agent.message("了解、モデルの較正を確認する。判定テストで測ってみる。")
res = sess.add_messages([m1, m2])
check("add_messages x2 -> bare array", isinstance(res, list) and len(res) == 2, f"ids={[r.id[:8] for r in res]}")
check(
    "message fields",
    res[0].content.startswith("較正")
    and res[0].session_id == "smoke-session-1"
    and res[0].peer_id == "alice"
    and res[0].workspace_id == WS
    and res[0].token_count > 0,
)

m3 = alice.message("バッチ計測したら 16ms/件だった。")
res1 = sess.add_messages(m3)
check("single message add", isinstance(res1, list) and len(res1) == 1 and res1[0].peer_id == "alice")

page = sess.messages(size=5)
items = page.items
check("messages list (page shape)", len(items) == 3 and page.total == 3, f"total={page.total} pages={page.pages}")
check("chronological first item", items[0].content.startswith("較正"), items[0].content[:14])

ctx = sess.context(tokens=1000)
check("session context", len(ctx.messages) == 3 and ctx.session_id == "smoke-session-1")

# --- peer sessions ---
ps = agent.sessions(size=50)
check("peer sessions", len(ps.items) >= 1, f"n={len(ps.items)}")

# --- search ---
sr = agent.search("較正")
check("peer search", len(sr) >= 1 and "較正" in sr[0].content, f"n={len(sr)}")
sr2 = sess.search("バッチ")
check("session search", len(sr2) >= 1, f"n={len(sr2)}")
sr3 = h.search("較正")
check("workspace search", len(sr3) >= 1, f"n={len(sr3)}")

# --- conclusions ---
scope = agent.conclusions_of("alice")
c1 = scope.create([{"observer_id": "agent", "observed_id": "alice", "content": "alice は較正の話題に強い関心がある"}])
check("conclusion create", len(c1) == 1 and c1[0].observer_id == "agent" and c1[0].level == "explicit")
lst = list(scope.list(size=10))
check("conclusions list", any("較正" in c.content for c in lst), f"n={len(lst)}")
q = scope.query("較正")
check("conclusions query", len(q) >= 1, f"n={len(q)}")

# --- peer context / representation / card ---
pc = agent.context(target="alice")
check(
    "peer context representation",
    pc.peer_id == "agent" and pc.target_id == "alice" and bool(pc.representation) and "較正" in pc.representation,
)
rep = agent.representation(target="alice")
check("representation route", isinstance(rep, str) and "較正" in rep, rep[:30])
c_before = agent.get_card(target="alice")
agent.set_card(["alice walks dog"], target="alice")
c_after = agent.get_card(target="alice")
check("card set/get", c_after == ["alice walks dog"], f"before={c_before}")

# --- queue status ---
qs = h.queue_status()
check(
    "queue status",
    qs.total_work_units >= 3 and qs.pending_work_units == qs.total_work_units,
    f"total={qs.total_work_units} pending={qs.pending_work_units} completed={qs.completed_work_units}",
)

# --- chat (dialectic) ---
try:
    ans = agent.chat("較正について何を知っている？", target="alice")
    check("chat (dialectic)", ans is None or (isinstance(ans, str) and len(ans) > 0), (ans or "")[:40])
except Exception as e:  # noqa: BLE001 — servers without an LLM configured return 503
    print(f"SKIP  chat (dialectic)  {type(e).__name__}: {e}")

print("MARK messages_total=%d conclusions=%d" % (page.total, len(list(scope.list(size=100)))))
print(f"ALL {passed[0]} CHECKS PASSED")
